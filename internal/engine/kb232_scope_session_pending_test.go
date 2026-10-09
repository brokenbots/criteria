package engine

import (
	"context"
	"testing"
	"time"

	remote "github.com/brokenbots/criteria/internal/adapter/environment/remote"
)

// kb232SessionWaitTestRunID is the run identity the pending-event bridge must
// stamp: the runSink fallback path carries the engine's own run identity
// because the shim starts before Run wires the run-scoped sink.
func kb232SessionWaitPending() *remote.ScopeSessionPending {
	return &remote.ScopeSessionPending{
		AdapterType: "shell",
		Scope:       "run_handler/9c1a2b3c-4d5e-6f70-8192-a3b4c5d6e7f8",
		Window:      30 * time.Second,
		Waited:      15 * time.Second,
		Dials:       4,
		Rejections:  3,
	}
}

// TestScopeSessionEventBridge_EmitsSessionPendingEvent pins the engine-side
// translation (KB-232 item 3): a shim scope_session_pending signal becomes a
// structured AdapterLifecycleEvent with Status scope_session_pending, the
// scope split into scope_name / scope_instance_id, the environment name, and
// the dialed/rejected counts — the payload the orchestrator logs for the
// operator.
func TestScopeSessionEventBridge_EmitsSessionPendingEvent(t *testing.T) {
	tracker := &eventTrackingSink{}
	e := &Engine{runID: "kb232-bridge-run", sink: tracker}
	bridge := &scopeSessionEventBridge{engine: e, environmentName: "prod"}

	bridge.OnScopeSessionPending(kb232SessionWaitPending())

	evt, ok := tracker.firstStatus(ScopeSessionPendingStatus)
	if !ok {
		t.Fatal("bridge emitted no scope_session_pending event")
	}
	if evt.Status != "scope_session_pending" {
		t.Errorf("status: got %q want scope_session_pending", evt.Status)
	}
	if evt.RunID != "kb232-bridge-run" {
		t.Errorf("run_id: got %q want the engine run id", evt.RunID)
	}
	if evt.AdapterType != "shell" {
		t.Errorf("adapter_type: got %q want shell", evt.AdapterType)
	}
	if evt.ScopeName != "run_handler" {
		t.Errorf("scope_name: got %q want run_handler", evt.ScopeName)
	}
	if evt.ScopeInstanceID != "9c1a2b3c-4d5e-6f70-8192-a3b4c5d6e7f8" {
		t.Errorf("scope_instance_id: got %q want the scope's instance id", evt.ScopeInstanceID)
	}
	if evt.EnvironmentName != "prod" {
		t.Errorf("environment_name: got %q want prod", evt.EnvironmentName)
	}
	if evt.SessionWaitSeconds != 15 {
		t.Errorf("session_wait_seconds: got %d want 15", evt.SessionWaitSeconds)
	}
	if evt.SessionDials != 4 {
		t.Errorf("session_dials: got %d want 4", evt.SessionDials)
	}
	if evt.SessionRejections != 3 {
		t.Errorf("session_rejections: got %d want 3", evt.SessionRejections)
	}
}

// TestScopeSessionEventBridge_RootScopeShapeFallback pins the empty-scope
// decode: the root workflow's "/<instance>" full scope key falls back to an
// empty scope_name with the instance id carried whole.
func TestScopeSessionEventBridge_RootScopeShapeFallback(t *testing.T) {
	tracker := &eventTrackingSink{}
	e := &Engine{runID: "kb232-root-run", sink: tracker}
	bridge := &scopeSessionEventBridge{engine: e, environmentName: "prod"}

	bridge.OnScopeSessionPending(&remote.ScopeSessionPending{
		AdapterType: "copilot",
		Scope:       "/0e1d2c3b-4a59-6877-8695-94938271605f",
		Window:      time.Minute,
		Waited:      31 * time.Second,
	})

	evt, ok := tracker.firstStatus(ScopeSessionPendingStatus)
	if !ok {
		t.Fatal("bridge emitted no scope_session_pending event")
	}
	if evt.ScopeName != "" {
		t.Errorf("scope_name: got %q want empty for the root scope shape", evt.ScopeName)
	}
	if evt.ScopeInstanceID != "0e1d2c3b-4a59-6877-8695-94938271605f" {
		t.Errorf("scope_instance_id: got %q want the instance id", evt.ScopeInstanceID)
	}
	if evt.AdapterType != "copilot" {
		t.Errorf("adapter_type: got %q want copilot", evt.AdapterType)
	}
}

// TestScopeSessionEventBridge_PrefersRunSink pins the sink precedence: when
// Run has wired the run-scoped sink the pending event rides it, not the
// engine-level fallback.
func TestScopeSessionEventBridge_PrefersRunSink(t *testing.T) {
	tracker := &eventTrackingSink{}
	e := &Engine{runID: "kb232-run-sink-run", sink: &fakeSink{}}
	runTracker := &eventTrackingSink{}
	e.runSink = runTracker
	bridge := &scopeSessionEventBridge{engine: e, environmentName: "prod"}

	bridge.OnScopeSessionPending(kb232SessionWaitPending())

	if _, ok := runTracker.firstStatus(ScopeSessionPendingStatus); !ok {
		t.Fatal("pending event missing from the run sink")
	}
	if tracker.hasStatus(ScopeSessionPendingStatus) {
		t.Error("pending event must not also land on the engine fallback sink")
	}
}

// TestEngine_ScopeSessionPendingEventWiredThroughShim proves the full wiring
// (KB-232 item 3): an engine-started per-scope shim, whose env names the event
// bridge at startRemoteShimForEnv time, delivers the half-window signal to the
// engine sink as a scope_session_pending lifecycle event while a session wait
// that never sees its adapter dials.
func TestEngine_ScopeSessionPendingEventWiredThroughShim(t *testing.T) {
	g := kb232PerScopeTwoAdapterGraph(t)
	dataDir := t.TempDir()
	eng := New(g, nil, &fakeSink{}, WithDataDir(dataDir))
	eng.runID = "kb232-wired-run"
	tracker := &eventTrackingSink{}
	eng.sink = tracker
	sessions := eng.newCRI293Sessions(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := eng.maybeStartRemoteShim(ctx, sessions); err != nil {
		t.Fatalf("maybeStartRemoteShim: %v", err)
	}
	t.Cleanup(func() {
		_ = sessions.Shutdown(context.WithoutCancel(ctx))
	})

	wrapped, ok := sessions.RemoteShimForEnv("remote.prod").(interface{ WrappedShim() *remote.Shim })
	if !ok {
		t.Fatal("per-scope session shim is not the remote shim this engine wires the event bridge into")
	}

	const scope = "kb-handler/7b6a5c4d-3e2f-4150-9283-a4b5c6d7e8f9"
	waitCtx, waitCancel := context.WithTimeout(context.WithoutCancel(ctx), 300*time.Millisecond)
	defer waitCancel()
	waitErr := make(chan error, 1)
	go func() {
		_, err := wrapped.WrappedShim().WaitForFreshHandle(waitCtx, "shell", scope, nil)
		waitErr <- err
	}()

	// The half-window signal fires on the waiting goroutine's wake timer;
	// poll the tracker until the translation lands or the wait fails.
	deadline := time.Now().Add(5 * time.Second)
	var evt AdapterLifecycleEvent
	var found bool
	for {
		evt, found = tracker.firstStatus(ScopeSessionPendingStatus)
		if found {
			break
		}
		select {
		case err := <-waitErr:
			t.Fatalf("session wait failed before the pending event arrived: %v", err)
		case now := <-time.After(5 * time.Millisecond):
			if now.After(deadline) {
				t.Fatal("engine never received the scope_session_pending event")
			}
		}
	}

	if evt.RunID != "kb232-wired-run" {
		t.Errorf("run_id: got %q want the engine run id (fallback path)", evt.RunID)
	}
	if evt.AdapterType != "shell" {
		t.Errorf("adapter_type: got %q want shell (from the awaited key)", evt.AdapterType)
	}
	if evt.ScopeName != "kb-handler" {
		t.Errorf("scope_name: got %q want kb-handler", evt.ScopeName)
	}
	if evt.ScopeInstanceID != "7b6a5c4d-3e2f-4150-9283-a4b5c6d7e8f9" {
		t.Errorf("scope_instance_id: got %q want the scope's instance id", evt.ScopeInstanceID)
	}
	if evt.EnvironmentName != "prod" {
		t.Errorf("environment_name: got %q want prod (the env that started the shim)", evt.EnvironmentName)
	}
	if evt.SessionDials != 0 || evt.SessionRejections != 0 {
		t.Errorf("counts: got dialed=%d rejected=%d want zeros (no adapter dialed)", evt.SessionDials, evt.SessionRejections)
	}
}

// TestEngine_ScopeSessionPendingStatusConstValue pins the status string the
// orchestrator keys its operator log on — an accidental rename would silently
// strand the observability contract.
func TestEngine_ScopeSessionPendingStatusConstValue(t *testing.T) {
	if ScopeSessionPendingStatus != "scope_session_pending" {
		t.Fatalf("ScopeSessionPendingStatus = %q, want \"scope_session_pending\"", ScopeSessionPendingStatus)
	}
}
