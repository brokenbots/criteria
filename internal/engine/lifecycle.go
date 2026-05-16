// Package engine provides lifecycle functions for automatic adapter provisioning and teardown.
package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow"
)

// initScopeAdapters provisions all adapters declared in the given FSMGraph at the start of its execution scope.
// Adapters are provisioned in declaration order (from AdapterOrder).
// If any adapter fails to initialize, all successfully provisioned adapters are torn down in reverse order,
// an event is emitted, and the error is returned.
// Returns the ordered slice of provisioned adapter IDs (for correct LIFO teardown)
// and an error if any adapter failed to initialize.
func initScopeAdapters(ctx context.Context, g *workflow.FSMGraph, deps Deps) (order []string, err error) {
	if len(g.Adapters) == 0 {
		return nil, nil
	}

	provisioned := make([]string, 0, len(g.Adapters)) // track in order for LIFO rollback

	// Provision adapters in declaration order (from AdapterOrder)
	for _, instanceID := range g.AdapterOrder {
		adapter := g.Adapters[instanceID]
		openErr := deps.Sessions.Open(ctx, instanceID, adapter.Type, adapter.OnCrash, adapter.Config)

		// Silently swallow ErrSessionAlreadyOpen to support subworkflow bodies that
		// re-declare parent adapters for safety through re-declaration. Same-scope
		// duplicate adapters are rejected at compile time by compileAdapters
		// (in workflow/compile_adapters.go:57-61), so already-open here always means
		// a parent-scope adapter being re-opened in a child scope.
		// Only adapters we newly opened are tracked for teardown.
		if openErr != nil && !errors.Is(openErr, adapterhost.ErrSessionAlreadyOpen) {
			// Rollback: tear down all successfully provisioned adapters in reverse order
			for i := len(provisioned) - 1; i >= 0; i-- {
				adapterID := provisioned[i]
				_ = deps.Sessions.Close(ctx, adapterID) // ignore teardown errors during rollback
			}
			// Emit lifecycle event for the failure
			deps.Sink.OnAdapterLifecycle("", instanceID, "init_failed", openErr.Error())
			return nil, fmt.Errorf("initialize adapter %q: %w", instanceID, openErr)
		}
		// Only track adapters that we newly opened (not already-open ones)
		// This prevents tearing down adapters that belong to a parent scope
		if openErr == nil {
			provisioned = append(provisioned, instanceID)

			// Emit lifecycle event for successful provisioning
			deps.Sink.OnAdapterLifecycle("", instanceID, "opened", "")
		}
	}

	return provisioned, nil
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
