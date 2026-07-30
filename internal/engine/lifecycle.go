// Package engine provides lifecycle functions for automatic adapter provisioning and teardown.
package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/zclconf/go-cty/cty"

	"github.com/brokenbots/criteria/internal/adapter/secrets"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow"
)

// initScopeAdapters provisions all adapters declared in the given FSMGraph at the start of its execution scope.
// Adapters are provisioned in declaration order (from AdapterOrder).
// If any adapter fails to initialize, all successfully provisioned adapters are torn down in reverse order,
// an event is emitted, and the error is returned.
// Returns the ordered slice of provisioned adapter IDs (for correct LIFO teardown)
// and an error if any adapter failed to initialize.
func initScopeAdapters(ctx context.Context, g *workflow.FSMGraph, deps Deps, vars map[string]cty.Value, workflowDir, scopeName string) (order []string, err error) {
	if len(g.Adapters) == 0 {
		return nil, nil
	}

	provisioned := make([]string, 0, len(g.Adapters)) // track in order for LIFO rollback

	// Provision adapters in declaration order (from AdapterOrder)
	for _, instanceID := range g.AdapterOrder {
		adapter := g.Adapters[instanceID]

		// Prepare the adapter inputs (secrets, origin refs, working dir, runtime
		// config). A prepare error means the adapter was never opened, so we emit
		// init_failed and return without rolling back already-provisioned peers.
		config, secretMap, originRefs, workingDir, perr := prepareScopeAdapter(ctx, g, instanceID, adapter, vars, workflowDir, deps, scopeName)
		if perr != nil {
			return nil, perr
		}

		// Reject working_directory values that are structurally invalid before any
		// step runs. A path containing ".." or falling outside the configured
		// allowed roots is an error that a later step cannot fix; only a missing
		// directory is deferred to session binding.
		if fvErr := deps.Sessions.ValidateWorkingDirFaceValue(workingDir); fvErr != nil {
			deps.Sink.OnAdapterLifecycle(scopeName, instanceID, "init_failed", fvErr.Error())
			return nil, fmt.Errorf("initialize adapter %q: %w", instanceID, fvErr)
		}

		verifyErr := deps.Sessions.Verify(ctx, instanceID, adapter.Type, adapter.OnCrash, config, secretMap, originRefs, workingDir)

		// Silently swallow ErrSessionAlreadyOpen to support subworkflow bodies that
		// re-declare parent adapters for safety through re-declaration. Same-scope
		// duplicate adapters are rejected at compile time by compileAdapters
		// (in workflow/compile_adapters.go:57-61), so already-open here always means
		// a parent-scope adapter being re-opened in a child scope.
		// Only adapters we newly verified are tracked for teardown.
		if verifyErr != nil && !errors.Is(verifyErr, adapterhost.ErrSessionAlreadyOpen) {
			// Rollback: tear down any sessions that were already bound. Verified-only
			// records hold no process, so they need no explicit cleanup.
			for i := len(provisioned) - 1; i >= 0; i-- {
				_ = deps.Sessions.Close(ctx, provisioned[i]) // ignore teardown errors during rollback
			}
			deps.Sink.OnAdapterLifecycle(scopeName, instanceID, "init_failed", verifyErr.Error())
			return nil, fmt.Errorf("initialize adapter %q: %w", instanceID, verifyErr)
		}
		// Only track adapters that we newly verified (not already-verified ones)
		// This prevents tearing down adapters that belong to a parent scope.
		if verifyErr == nil {
			provisioned = append(provisioned, instanceID)
			deps.Sink.OnAdapterLifecycle(scopeName, instanceID, "verified", "")
		}
	}

	return provisioned, nil
}

// prepareScopeAdapter resolves everything an adapter needs before it is opened:
// secrets, origin refs, the bound environment's working directory, and the
// runtime-evaluated config. On any failure it emits an init_failed lifecycle
// event and returns a wrapped error; the caller must return that error without
// rolling back already-provisioned adapters (the adapter was never opened).
func prepareScopeAdapter(
	ctx context.Context,
	g *workflow.FSMGraph,
	instanceID string,
	adapter *workflow.AdapterNode,
	vars map[string]cty.Value,
	workflowDir string,
	deps Deps,
	scopeName string,
) (config, secretMap map[string]string, originRefs map[string]secrets.OriginRef, workingDir string, err error) {
	// Resolve adapter secrets (WS13).
	if len(adapter.Secrets) > 0 {
		secretMap, err = resolveAdapterSecrets(ctx, g, adapter, vars, deps.Sessions.RedactionRegistry)
		if err != nil {
			deps.Sink.OnAdapterLifecycle(scopeName, instanceID, "init_failed", err.Error())
			return nil, nil, nil, "", fmt.Errorf("initialize adapter %q: %w", instanceID, err)
		}
	}

	originRefs, err = buildOriginRefs(adapter, vars)
	if err != nil {
		deps.Sink.OnAdapterLifecycle(scopeName, instanceID, "init_failed", err.Error())
		return nil, nil, nil, "", fmt.Errorf("initialize adapter %q: %w", instanceID, err)
	}

	// Resolve the bound environment's working_directory against the runtime
	// closure now, at adapter init, so the cwd can be dynamic (e.g.
	// var.worktree supplied via --var). The resolved value becomes the
	// adapter process launch cwd.
	workingDir, err = resolveAdapterWorkingDir(g, adapter, vars)
	if err != nil {
		deps.Sink.OnAdapterLifecycle(scopeName, instanceID, "init_failed", err.Error())
		return nil, nil, nil, "", fmt.Errorf("initialize adapter %q: resolve working_directory: %w", instanceID, err)
	}

	// Re-evaluate adapter config against runtime vars so that var.* references
	// in config blocks resolve to actual runtime values, not compile-time defaults.
	config = adapter.Config
	if len(adapter.ConfigExprs) > 0 {
		runtimeConfig, evalErr := workflow.ResolveInputExprsWithOpts(
			adapter.ConfigExprs, vars, workflow.DefaultFunctionOptions(workflowDir),
		)
		if evalErr != nil {
			deps.Sink.OnAdapterLifecycle(scopeName, instanceID, "init_failed", evalErr.Error())
			return nil, nil, nil, "", fmt.Errorf("initialize adapter %q: evaluate config: %w", instanceID, evalErr)
		}
		config = runtimeConfig
	}

	return config, secretMap, originRefs, workingDir, nil
}

// resolveAdapterWorkingDir resolves the working_directory of the environment
// bound to the adapter (its declared environment, or the workflow default)
// against the runtime vars. It returns "" when no environment is bound or the
// environment declares no working_directory.
func resolveAdapterWorkingDir(g *workflow.FSMGraph, ad *workflow.AdapterNode, vars map[string]cty.Value) (string, error) {
	envKey := ad.Environment
	if envKey == "" {
		envKey = g.DefaultEnvironment
	}
	if envKey == "" {
		return "", nil
	}
	return g.Environments[envKey].ResolveWorkingDir(vars)
}

// buildOriginRefs evaluates an adapter's secret expressions and converts them into
// unevaluated origin references for snapshot/restore (WS18).
func buildOriginRefs(adapter *workflow.AdapterNode, vars map[string]cty.Value) (map[string]secrets.OriginRef, error) {
	if len(adapter.Secrets) == 0 {
		return nil, nil
	}
	evaluated, err := workflow.ResolveInputExprs(adapter.Secrets, vars)
	if err != nil {
		return nil, err
	}
	originRefs := make(map[string]secrets.OriginRef, len(evaluated))
	for k, v := range evaluated {
		originRefs[k] = secrets.ParseOriginRef(v)
	}
	return originRefs, nil
}

// tearDownScopeAdapters releases all adapter sessions in the given order in reverse (LIFO).
// The order slice must be the one returned by initScopeAdapters to ensure correct teardown order.
// Errors during teardown are logged via the adapter lifecycle sink but do not change the run's terminal state.
// Always called, even if the run errored or was cancelled.
// Uses context.WithoutCancel to ensure teardown completes even if the run context was canceled.
func tearDownScopeAdapters(ctx context.Context, order []string, deps Deps) {
	if len(order) == 0 {
		return
	}

	// Use context.WithoutCancel to detach from parent cancellation,
	// ensuring cleanup runs even if the main run context was cancelled.
	cleanupCtx := context.WithoutCancel(ctx)

	// Teardown in reverse order (LIFO)
	for i := len(order) - 1; i >= 0; i-- {
		adapterID := order[i]
		err := deps.Sessions.Close(cleanupCtx, adapterID)
		if err != nil {
			// Emit lifecycle event for the failure but don't abort
			deps.Sink.OnAdapterLifecycle("", adapterID, "close_failed", err.Error())
		} else {
			// Emit successful close event
			deps.Sink.OnAdapterLifecycle("", adapterID, "closed", "")
		}
	}
}
