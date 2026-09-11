// Package engine provides lifecycle functions for automatic adapter provisioning and teardown.
package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/zclconf/go-cty/cty"

	"github.com/brokenbots/criteria/internal/adapter/environment/remote"
	"github.com/brokenbots/criteria/internal/adapter/secrets"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow"
	"github.com/brokenbots/criteria/workflow/lockfile"
)

// adapterLifecycleRecord holds the metadata needed to emit a controller-visible
// remote-adapter lifecycle event and to clean up the scope on teardown.
type adapterLifecycleRecord struct {
	scopeName       string
	scopeInstanceID string
	scopeKey        string
	adapterName     string
	adapterType     string
	envName         string
	digest          string
	tokenPath       string
	listenAddr      string
	perScope        bool
	runID           string
}

// remoteLifecycleContext carries the lockfile and per-scope provisioning state
// that the engine needs to emit remote adapter lifecycle events. It travels
// in RunState rather than Deps so that Deps stays small for node evaluators.
type remoteLifecycleContext struct {
	lockfile       *lockfile.Lockfile
	scopeLifecycle *scopeLifecycleState
}

// scopeLifecycleState tracks per-scope remote-adapter provisioning metadata.
type scopeLifecycleState struct {
	dataDir string
	runID   string
	mu      sync.Mutex
	records map[string]*adapterLifecycleRecord
}

func newScopeLifecycleState(dataDir string) *scopeLifecycleState {
	return &scopeLifecycleState{
		dataDir: dataDir,
		records: make(map[string]*adapterLifecycleRecord),
	}
}

func (ls *scopeLifecycleState) add(rec *adapterLifecycleRecord) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	ls.records[rec.adapterName] = rec
}

func (ls *scopeLifecycleState) get(adapterName string) *adapterLifecycleRecord {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	return ls.records[adapterName]
}

func (ls *scopeLifecycleState) remove(adapterName string) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	delete(ls.records, adapterName)
}

func (ls *scopeLifecycleState) setRunID(id string) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	ls.runID = id
}

func generateAcceptToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func writeRotatedToken(dataDir, scopeName, scopeInstanceID, adapterType, token string) (string, error) {
	dir := filepath.Join(dataDir, "remote-tokens", scopeName, scopeInstanceID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, adapterType+".token")
	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

func remoteEnvConfig(g *workflow.FSMGraph, ad *workflow.AdapterNode) (*remote.Config, *workflow.EnvironmentNode, bool) {
	envKey := ad.Environment
	if envKey == "" {
		envKey = g.DefaultEnvironment
	}
	env := g.Environments[envKey]
	if env == nil || env.Type != "remote" {
		return nil, nil, false
	}
	cfg, err := remote.ParseConfig(env.RawBody)
	if err != nil {
		return nil, env, false
	}
	return cfg, env, true
}

func lockedDigest(lf *lockfile.Lockfile, adapterType, adapterName string) string {
	if lf == nil {
		return ""
	}
	for i := range lf.Adapters {
		a := &lf.Adapters[i]
		if a.Type == adapterType && a.Name == adapterName {
			return a.ResolvedDigest
		}
	}
	return ""
}

func emitProvisionWanted(deps Deps, lifecycle *remoteLifecycleContext, scopeName, scopeInstanceID, scopeKey, instanceID string, adapter *workflow.AdapterNode, envNode *workflow.EnvironmentNode, tokenPath string) {
	digest := lockedDigest(lifecycle.lockfile, adapter.Type, adapter.Name)
	listenAddr := deps.Sessions.RemoteListenAddr()
	deps.Sink.OnAdapterLifecycleEvent(&AdapterLifecycleEvent{
		RunID:             lifecycle.scopeLifecycle.runID,
		ScopeName:         scopeName,
		ScopeInstanceID:   scopeInstanceID,
		AdapterName:       adapter.Name,
		Digest:            digest,
		ShimListenAddress: listenAddr,
		TokenRef:          tokenPath,
		Status:            "provision_wanted",
	})
	lifecycle.scopeLifecycle.add(&adapterLifecycleRecord{
		scopeName:       scopeName,
		scopeInstanceID: scopeInstanceID,
		scopeKey:        scopeKey,
		adapterName:     instanceID,
		adapterType:     adapter.Type,
		envName:         envNode.Name,
		digest:          digest,
		tokenPath:       tokenPath,
		listenAddr:      listenAddr,
		perScope:        true,
		runID:           lifecycle.scopeLifecycle.runID,
	})
}

// maybeRotateRemoteScope rotates the accept token for a per-scope remote adapter
// and emits a provision-wanted lifecycle event. It returns the shim scope key to
// pass to Verify, or an empty string when per-scope sessions are disabled.
func maybeRotateRemoteScope(deps Deps, lifecycle *remoteLifecycleContext, g *workflow.FSMGraph, adapter *workflow.AdapterNode, instanceID, scopeName string) (string, error) {
	remCfg, envNode, ok := remoteEnvConfig(g, adapter)
	if !ok || !remCfg.PerScopeSessions {
		return "", nil
	}
	if lifecycle == nil || lifecycle.scopeLifecycle == nil || lifecycle.scopeLifecycle.dataDir == "" {
		err := fmt.Errorf("remote environment %q uses per_scope_sessions but no run data directory is configured", envNode.Name)
		deps.Sink.OnAdapterLifecycle(scopeName, instanceID, "init_failed", err.Error())
		return "", fmt.Errorf("initialize adapter %q: %w", instanceID, err)
	}
	scopeInstanceID := uuid.NewString()
	scopeKey := scopeName + "/" + scopeInstanceID
	token, err := generateAcceptToken()
	if err != nil {
		deps.Sink.OnAdapterLifecycle(scopeName, instanceID, "init_failed", err.Error())
		return "", fmt.Errorf("initialize adapter %q: rotate accept token: %w", instanceID, err)
	}
	tokenPath, err := writeRotatedToken(lifecycle.scopeLifecycle.dataDir, scopeName, scopeInstanceID, adapter.Type, token)
	if err != nil {
		deps.Sink.OnAdapterLifecycle(scopeName, instanceID, "init_failed", err.Error())
		return "", fmt.Errorf("initialize adapter %q: write accept token: %w", instanceID, err)
	}
	if err := deps.Sessions.RegisterRemoteScope(scopeKey, token); err != nil {
		deps.Sink.OnAdapterLifecycle(scopeName, instanceID, "init_failed", err.Error())
		return "", fmt.Errorf("initialize adapter %q: register scope token: %w", instanceID, err)
	}
	emitProvisionWanted(deps, lifecycle, scopeName, scopeInstanceID, scopeKey, instanceID, adapter, envNode, tokenPath)
	return scopeKey, nil
}

// initScopeAdapters provisions all adapters declared in the given FSMGraph at the start of its execution scope.
// Adapters are provisioned in declaration order (from AdapterOrder).
// If any adapter fails to initialize, all successfully provisioned adapters are torn down in reverse order,
// an event is emitted, and the error is returned.
// Returns the ordered slice of provisioned adapter IDs (for correct LIFO teardown)
// and an error if any adapter failed to initialize.
func initScopeAdapters(ctx context.Context, g *workflow.FSMGraph, deps Deps, vars map[string]cty.Value, workflowDir, scopeName string, secretOrigins map[string]secrets.OriginRef, lifecycle *remoteLifecycleContext) (order []string, err error) {
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
		config, secretMap, originRefs, workingDir, perr := prepareScopeAdapter(ctx, g, instanceID, adapter, vars, workflowDir, deps, scopeName, secretOrigins)
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

		// CRI-115: when the bound remote environment enables per-scope sessions,
		// rotate a fresh accept token for this scope, persist it under the run
		// data directory, register it with the shim, and emit a provision-wanted
		// event *before* blocking on WaitForHandle.
		verifyScope, err := maybeRotateRemoteScope(deps, lifecycle, g, adapter, instanceID, scopeName)
		if err != nil {
			return nil, err
		}

		verifyErr := deps.Sessions.Verify(ctx, instanceID, adapter.Type, adapter.OnCrash, config, secretMap, originRefs, workingDir, scopeName, verifyScope)

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
	secretOrigins map[string]secrets.OriginRef,
) (config, secretMap map[string]string, originRefs map[string]secrets.OriginRef, workingDir string, err error) {
	// Resolve adapter secrets (WS13).
	if len(adapter.Secrets) > 0 {
		secretMap, err = resolveAdapterSecrets(ctx, g, adapter, vars, deps.Sessions.RedactionRegistry)
		if err != nil {
			deps.Sink.OnAdapterLifecycle(scopeName, instanceID, "init_failed", err.Error())
			return nil, nil, nil, "", fmt.Errorf("initialize adapter %q: %w", instanceID, err)
		}
	}

	originRefs, err = buildOriginRefs(adapter, secretOrigins)
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
	// file() content is served from the graph's compile-time cache so prompt
	// files cannot be altered mid-run.
	config = adapter.Config
	if len(adapter.ConfigExprs) > 0 {
		opts := workflow.DefaultFunctionOptions(workflowDir)
		opts.FileCache = g.FileCache
		runtimeConfig, evalErr := workflow.ResolveInputExprsWithOpts(
			adapter.ConfigExprs, vars, opts,
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

// buildOriginRefs maps an adapter's secret expressions to the declared origins
// of the referenced secret variables or data blocks. This keeps raw secret
// values out of session snapshots while still allowing them to re-resolve on
// resume (WS18, CRI-88).
func buildOriginRefs(adapter *workflow.AdapterNode, secretOrigins map[string]secrets.OriginRef) (map[string]secrets.OriginRef, error) {
	if len(adapter.Secrets) == 0 {
		return nil, nil
	}
	originRefs := make(map[string]secrets.OriginRef, len(adapter.Secrets))
	for k, expr := range adapter.Secrets {
		isVar, key, ok := workflow.SecretBindingRefFromExpr(expr, nil)
		if !ok {
			return nil, fmt.Errorf("adapter %q.%q secrets.%s: cannot build origin ref: expression is not a direct secret reference", adapter.Type, adapter.Name, k)
		}
		var origin secrets.OriginRef
		if isVar {
			origin = secretOrigins[key]
		} else {
			parts := strings.SplitN(key, ".", 2)
			if len(parts) != 2 {
				return nil, fmt.Errorf("adapter %q.%q secrets.%s: invalid data origin key %q", adapter.Type, adapter.Name, k, key)
			}
			origin = secretOrigins["data."+key]
		}
		if origin.Kind == "" {
			// No recorded origin means the value was a literal default. Persist it
			// as a literal origin so snapshots remain restorable. This path is
			// intended only for narrowly-controlled test fixtures.
			origin = secrets.OriginRef{Kind: "literal", Ref: "(untracked)"}
		}
		originRefs[k] = origin
	}
	return originRefs, nil
}

// tearDownScopeAdapters releases all adapter sessions in the given order in reverse (LIFO).
// The order slice must be the one returned by initScopeAdapters to ensure correct teardown order.
// Errors during teardown are logged via the adapter lifecycle sink but do not change the run's terminal state.
// Always called, even if the run errored or was cancelled.
// Uses context.WithoutCancel to ensure teardown completes even if the run context was canceled.
func tearDownScopeAdapters(ctx context.Context, order []string, deps Deps, lifecycle *remoteLifecycleContext) {
	if len(order) == 0 {
		return
	}

	// Use context.WithoutCancel to detach from parent cancellation,
	// ensuring cleanup runs even if the main run context was cancelled.
	cleanupCtx := context.WithoutCancel(ctx)

	// Teardown in reverse order (LIFO)
	for i := len(order) - 1; i >= 0; i-- {
		adapterID := order[i]

		// CRI-115: emit a release event for per-scope remote adapters before
		// closing the session, then unregister the scope token so a torn-down
		// pod cannot reconnect with the old token.
		if lifecycle != nil && lifecycle.scopeLifecycle != nil {
			if rec := lifecycle.scopeLifecycle.get(adapterID); rec != nil && rec.perScope {
				deps.Sink.OnAdapterLifecycleEvent(&AdapterLifecycleEvent{
					RunID:             rec.runID,
					ScopeName:         rec.scopeName,
					ScopeInstanceID:   rec.scopeInstanceID,
					AdapterName:       rec.adapterName,
					Digest:            rec.digest,
					ShimListenAddress: rec.listenAddr,
					TokenRef:          rec.tokenPath,
					Status:            "released",
				})
				_ = deps.Sessions.UnregisterRemoteScope(rec.scopeKey)
				_ = deps.Sessions.CloseRemoteHandle(cleanupCtx, rec.adapterType, rec.scopeKey)
				lifecycle.scopeLifecycle.remove(adapterID)
			}
		}

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
