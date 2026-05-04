// Package engine provides lifecycle functions for automatic adapter provisioning and teardown.
package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/brokenbots/criteria/internal/plugin"
	"github.com/brokenbots/criteria/workflow"
)

// SessionHandle is an opaque handle to a provisioned adapter session, returned by initScopeAdapters.
// It is used by tearDownScopeAdapters to release the session.
type SessionHandle = interface{}

// initScopeAdapters provisions all adapters declared in the given FSMGraph at the start of its execution scope.
// Adapters are provisioned in declaration order.
// If any adapter fails to initialize, all successfully provisioned adapters are torn down in reverse order,
// an event is emitted, and the error is returned.
// Returns a map of "<type>.<name>" -> SessionHandle for the provisioned adapters.
func initScopeAdapters(ctx context.Context, g *workflow.FSMGraph, deps Deps) (map[string]SessionHandle, error) {
	if len(g.Adapters) == 0 {
		return make(map[string]SessionHandle), nil
	}

	handles := make(map[string]SessionHandle)
	provisioned := make([]string, 0, len(g.Adapters)) // track in order for rollback

	// Provision adapters in declaration order
	for _, adapter := range g.Adapters {
		instanceID := adapter.Type + "." + adapter.Name
		err := deps.Sessions.Open(ctx, instanceID, adapter.Type, adapter.OnCrash, adapter.Config)
		if err != nil && !errors.Is(err, plugin.ErrSessionAlreadyOpen) {
			// Rollback: tear down all successfully provisioned adapters in reverse order
			for i := len(provisioned) - 1; i >= 0; i-- {
				adapterID := provisioned[i]
				_ = deps.Sessions.Close(ctx, adapterID) // ignore teardown errors during rollback
			}
			// Emit lifecycle event for the failure
			deps.Sink.OnAdapterLifecycle("", instanceID, "init_failed", err.Error())
			return nil, fmt.Errorf("initialize adapter %q: %w", instanceID, err)
		}
		// Only track adapters that we newly opened (not already-open ones)
		// This prevents tearing down adapters that belong to a parent scope
		if err == nil {
			provisioned = append(provisioned, instanceID)
			handles[instanceID] = nil // placeholder handle

			// Emit lifecycle event for successful provisioning
			deps.Sink.OnAdapterLifecycle("", instanceID, "opened", "")
		}
	}

	return handles, nil
}

// tearDownScopeAdapters releases all adapter sessions in the given handles map in reverse order.
// Errors during teardown are logged via the adapter lifecycle sink but do not change the run's terminal state.
// Always called, even if the run errored or was cancelled.
func tearDownScopeAdapters(ctx context.Context, handles map[string]SessionHandle, deps Deps) {
	if len(handles) == 0 {
		return
	}

	// Collect adapter IDs and sort in reverse declaration order
	// (in the absence of a specific order, just reverse the map iteration order)
	adapterIDs := make([]string, 0, len(handles))
	for adapterID := range handles {
		adapterIDs = append(adapterIDs, adapterID)
	}

	// Teardown in reverse order (LIFO)
	for i := len(adapterIDs) - 1; i >= 0; i-- {
		adapterID := adapterIDs[i]
		err := deps.Sessions.Close(ctx, adapterID)
		if err != nil {
			// Emit lifecycle event for the failure but don't abort
			deps.Sink.OnAdapterLifecycle("", adapterID, "close_failed", err.Error())
		} else {
			// Emit successful close event
			deps.Sink.OnAdapterLifecycle("", adapterID, "closed", "")
		}
	}
}
