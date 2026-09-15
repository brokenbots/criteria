package adapterhost

// pause_midcall_test.go — CRI-169 drain-first pause posture (ADR-0004 §11):
// the pause gate refuses new nested tool calls with the typed `paused`
// call_error, Session.Pause drains in-flight nested calls within a bounded
// window, non-draining stragglers are canceled with a typed `canceled`
// reply, Resume clears the gate, and the snapshot/restore path preserves the
// restored permission state instead of replacing it with a fresh one.

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/brokenbots/criteria/workflow"
)

// waitForCalleeStart polls until the callee adapter has recorded an Execute
// entry (the nested call is in flight).
func waitForCalleeStart(t *testing.T, rec *nestedCalleeRecorder) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if rec.calleeSession() != "" {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("callee never started executing")
}

// waitForToolCallResult polls the caller fake until a typed reply arrives.
func waitForToolCallResult(t *testing.T, caller *nestedCallerAdapter) *v2.ToolCallResult {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if tcr := caller.gotResult(); tcr != nil {
			return tcr
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("caller never received tool_call_result")
	return nil
}

// openNestedToolCallSessions verifies the graph and opens the caller/callee
// sessions on the manager.
func openNestedToolCallSessions(t *testing.T, sm *SessionManager, graph *workflow.FSMGraph) {
	t.Helper()
	ctx := context.Background()
	if err := sm.VerifyGraph(ctx, graph, nil); err != nil {
		t.Fatalf("VerifyGraph: %v", err)
	}
	if err := sm.Open(ctx, nestedCallerSession, "caller", "", nil, nil); err != nil {
		t.Fatalf("Open caller: %v", err)
	}
	if err := sm.Open(ctx, nestedCalleeSession, "callee", "", nil, nil); err != nil {
		t.Fatalf("Open callee: %v", err)
	}
	t.Cleanup(func() {
		_ = sm.Close(context.WithoutCancel(ctx), nestedCallerSession)
		_ = sm.Close(context.WithoutCancel(ctx), nestedCalleeSession)
	})
}

// directSinkForSession builds a permissionInterceptSink on the session's real
// permission state so tests can issue follow-up tool calls directly.
func directSinkForSession(sess *Session, sm *SessionManager, graph *workflow.FSMGraph) *permissionInterceptSink {
	return &permissionInterceptSink{
		inner:     &adapterEventCollector{},
		permState: sess.PermissionState,
		session:   sess,
		step:      nestedCallerStep(),
		graph:     graph,
		mgr:       sm,
		nesting: toolCallNesting{
			chain: []string{nestedCallerSession},
		},
	}
}

// TestSession_Pause_DrainsInFlightToolCall (CRI-169): pausing while a nested
// tool call is in flight sets the pause gate, drains the in-flight call
// within the bounded window (its typed reply is delivered while the stream
// is still live), and leaves the session paused. After Resume the gate is
// cleared and further calls work.
func TestSession_Pause_DrainsInFlightToolCall(t *testing.T) {
	ctx := context.Background()
	audit := &sliceAuditWriter{}
	calleeRec := &nestedCalleeRecorder{}
	holdRelease := make(chan struct{})
	caller := &nestedCallerAdapter{target: nestedCallTarget, args: map[string]any{"task": "hold"}}
	callee := &nestedCalleeAdapter{rec: calleeRec, holdRelease: holdRelease}
	sm := newNestedToolCallManager(t, caller, callee)
	sm.Audit = audit
	graph := compileNestedToolCallGraph(t)
	sm.SetGraph(graph)
	openNestedToolCallSessions(t, sm, graph)

	sess, err := sm.lookup(nestedCallerSession)
	if err != nil {
		t.Fatalf("lookup caller: %v", err)
	}

	// The caller's Execute issues the tool call and awaits the reply.
	execDone := make(chan error, 1)
	go func() {
		_, execErr := sm.Execute(ctx, nestedCallerSession, nestedCallerStep(), &adapterEventCollector{})
		execDone <- execErr
	}()
	waitForCalleeStart(t, calleeRec)

	// Pause mid-call: the drain waits for the in-flight call to settle.
	pauseDone := make(chan error, 1)
	go func() { pauseDone <- sess.Pause(ctx) }()

	// Release the held call; it settles inside the drain window.
	close(holdRelease)

	tcr := waitForToolCallResult(t, caller)
	if tcr.RequestId != "call-1" || tcr.CallError != "" || tcr.Outcome != "success" {
		t.Errorf("drained call reply = %+v, want call-1/success/no error", tcr)
	}
	if execErr := <-execDone; execErr != nil {
		t.Errorf("caller Execute: %v", execErr)
	}
	if err := <-pauseDone; err != nil {
		t.Fatalf("Session.Pause: %v", err)
	}
	if !sess.PermissionState.toolCallsPaused() {
		t.Error("pause gate not set after Session.Pause")
	}

	// Resume clears the gate; further calls work.
	if err := sess.Resume(ctx); err != nil {
		t.Fatalf("Session.Resume: %v", err)
	}
	if sess.PermissionState.toolCallsPaused() {
		t.Error("pause gate still set after Resume")
	}

	sink := directSinkForSession(sess, sm, graph)
	sink.Adapter("permission.request", map[string]any{
		"request_id": "call-2",
		"target":     nestedCallTarget,
		"args":       map[string]any{"task": "do-thing"},
	})
	tcr = readToolCallResult(t, sess.PermissionState)
	if tcr.RequestId != "call-2" || tcr.CallError != "" || tcr.Outcome != "success" {
		t.Errorf("post-resume call reply = %+v, want call-2/success/no error", tcr)
	}
	if got := calleeRec.calleeSession(); got != nestedCalleeSession {
		t.Errorf("post-resume callee session = %q, want %q", got, nestedCalleeSession)
	}
}

// TestSession_Pause_CancelsNonDrainingToolCall (CRI-169): a nested call that
// does not settle within the bounded drain window is canceled; its reply is
// the typed `canceled` call_error delivered while the stream is still live,
// and a call issued while the gate is up is refused with the typed `paused`
// call_error.
func TestSession_Pause_CancelsNonDrainingToolCall(t *testing.T) {
	ctx := context.Background()
	audit := &sliceAuditWriter{}
	calleeRec := &nestedCalleeRecorder{}
	caller := &nestedCallerAdapter{target: nestedCallTarget, args: map[string]any{"task": "block"}}
	callee := &nestedCalleeAdapter{rec: calleeRec} // "block" task never settles
	sm := newNestedToolCallManager(t, caller, callee)
	sm.Audit = audit
	// Set before Open: the window is stamped onto the session's permission
	// state at creation time.
	sm.PauseToolCallDrainTimeout = 300 * time.Millisecond
	graph := compileNestedToolCallGraph(t)
	sm.SetGraph(graph)
	openNestedToolCallSessions(t, sm, graph)

	sess, err := sm.lookup(nestedCallerSession)
	if err != nil {
		t.Fatalf("lookup caller: %v", err)
	}
	if got := sess.PermissionState.pauseDrainWindow; got != 300*time.Millisecond {
		t.Fatalf("stamped drain window = %v, want 300ms", got)
	}

	execDone := make(chan error, 1)
	go func() {
		_, execErr := sm.Execute(ctx, nestedCallerSession, nestedCallerStep(), &adapterEventCollector{})
		execDone <- execErr
	}()
	waitForCalleeStart(t, calleeRec)

	pauseDone := make(chan error, 1)
	go func() { pauseDone <- sess.Pause(ctx) }()

	tcr := waitForToolCallResult(t, caller)
	if tcr.RequestId != "call-1" || tcr.CallError != callErrorCanceled {
		t.Errorf("straggler reply = %+v, want call-1/%s", tcr, callErrorCanceled)
	}
	if execErr := <-execDone; execErr != nil {
		t.Errorf("caller Execute: %v", execErr)
	}
	if err := <-pauseDone; err != nil {
		t.Fatalf("Session.Pause: %v", err)
	}
	if got := callee.recordedCtxErr(0); !errors.Is(got, context.Canceled) {
		t.Errorf("callee observed ctx.Err() = %v, want context.Canceled", got)
	}
	if !sess.PermissionState.toolCallsPaused() {
		t.Error("pause gate not set after Session.Pause")
	}

	// A call issued while the gate is up is refused typed `paused`.
	sink := directSinkForSession(sess, sm, graph)
	sink.Adapter("permission.request", map[string]any{
		"request_id": "call-2",
		"target":     nestedCallTarget,
		"args":       map[string]any{"task": "do-thing"},
	})
	tcr = readToolCallResult(t, sess.PermissionState)
	if tcr.RequestId != "call-2" || tcr.CallError != callErrorPaused {
		t.Errorf("paused-gate reply = %+v, want call-2/%s", tcr, callErrorPaused)
	}
	if got := calleeRec.calleeSession(); got != nestedCalleeSession {
		t.Errorf("paused-gate call unexpectedly executed in session %q", got)
	}
}

// TestPermissionState_PauseGate (CRI-169): beginToolCallPause refuses new
// pending registrations until resumeToolCalls clears the gate; the settled
// marker is closed exactly once by clearPendingToolCall; Pause/Resume set
// and clear the gate atomically with the stream flag.
func TestPermissionState_PauseGate(t *testing.T) {
	ps := newToolCallState(t, &sliceAuditWriter{})
	if !ps.registerPendingToolCall("call-1", "adapter.callee.helper.tools.a", func() {}) {
		t.Fatal("registration refused before the gate was set")
	}

	ps.beginToolCallPause()
	if !ps.toolCallsPaused() {
		t.Fatal("gate not set by beginToolCallPause")
	}
	if ps.registerPendingToolCall("call-2", "adapter.callee.helper.tools.b", func() {}) {
		t.Fatal("registration accepted while the pause gate is set")
	}
	// First-writer-wins still holds for a pre-gate registration.
	if !ps.registerPendingToolCall("call-1", "adapter.callee.helper.tools.a", func() {}) {
		t.Fatal("duplicate registration reported refused")
	}

	ps.resumeToolCalls()
	if ps.toolCallsPaused() {
		t.Fatal("gate not cleared by resumeToolCalls")
	}
	if !ps.registerPendingToolCall("call-2", "adapter.callee.helper.tools.b", func() {}) {
		t.Fatal("registration refused after the gate was cleared")
	}

	// The settled marker is closed exactly once when the entry is cleared.
	ps.mu.Lock()
	settled := ps.pendingToolCalls["call-2"].settled
	ps.mu.Unlock()
	ps.clearPendingToolCall("call-2")
	select {
	case <-settled:
	default:
		t.Fatal("settled marker not closed by clearPendingToolCall")
	}
	// Clearing again must not panic (one-shot close under the mutex).
	ps.clearPendingToolCall("call-2")

	// Pause sets the gate atomically with the stream flag; Resume clears
	// both.
	ps.Pause()
	if !ps.toolCallsPaused() {
		t.Fatal("gate not set by Pause")
	}
	ps.Resume()
	if ps.toolCallsPaused() {
		t.Fatal("gate not cleared by Resume")
	}
}

// TestPermissionState_AwaitToolCallDrain (CRI-169): the drain waits for
// in-flight calls to settle within the bounded window, cancels stragglers,
// grants a settle grace for the canceled calls to clear, and returns the
// entries still pending after the grace.
func TestPermissionState_AwaitToolCallDrain(t *testing.T) {
	ps := newToolCallState(t, &sliceAuditWriter{})
	_, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()

	if !ps.registerPendingToolCall("call-1", "adapter.callee.helper.tools.a", cancelA) ||
		!ps.registerPendingToolCall("call-2", "adapter.callee.helper.tools.b", cancelB) {
		t.Fatal("registrations refused before the gate was set")
	}

	// call-1 settles immediately; call-2 settles when its cancel fires
	// (mirroring the nested goroutine's failure-report path).
	ps.clearPendingToolCall("call-1")
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-ctxB.Done()
		ps.clearPendingToolCall("call-2")
	}()

	still := ps.awaitToolCallDrain(2 * time.Second)
	wg.Wait()
	if len(still) != 0 {
		t.Fatalf("drain returned still-pending %v, want none", still)
	}
	if err := ctxB.Err(); !errors.Is(err, context.Canceled) {
		t.Errorf("straggler cancel not invoked: ctx.Err() = %v", err)
	}

	// A call that never settles after cancelation is abandoned: the drain
	// returns it (the session-close abandonment audit reports it later).
	ctxC, cancelC := context.WithCancel(context.Background())
	defer cancelC()
	if !ps.registerPendingToolCall("call-3", "adapter.callee.helper.tools.c", cancelC) {
		t.Fatal("registration refused")
	}
	still = ps.awaitToolCallDrain(150 * time.Millisecond)
	if len(still) != 1 || still[0].requestID != "call-3" {
		t.Fatalf("drain returned still-pending %v, want exactly call-3", still)
	}
	if err := ctxC.Err(); !errors.Is(err, context.Canceled) {
		t.Errorf("straggler cancel not invoked: ctx.Err() = %v", err)
	}

	// The abandoned registration is still reported by the teardown drain.
	drained := ps.drainPendingToolCalls()
	if len(drained) != 1 || drained[0].requestID != "call-3" {
		t.Fatalf("teardown drain = %v, want exactly call-3", drained)
	}
}

// TestSessionManager_Restore_KeepsRestoredPermissionState (CRI-169): the
// permission stream start must not replace a rehydrated permission state on
// the snapshot/restore path, and the pause drain window is stamped from the
// manager's configured value.
func TestSessionManager_Restore_KeepsRestoredPermissionState(t *testing.T) {
	ctx := context.Background()
	h := &snapshotMockHandle{}
	makeTestSession(NewSessionManager(nil), "s1", h, nil)

	ps := NewPermissionState("s1", nil)
	ps.SetStreamCancel(func() {})
	ps.SetPolicy(NewPolicy([]string{"read_file"}))
	if allow, _ := ps.Evaluate("read_file", "read_file", "", ""); !allow {
		t.Fatal("expected allow")
	}
	permBlob, err := ps.MarshalState()
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}

	snap := &SessionSnapshot{
		AdapterState:    []byte("adapter"),
		SchemaVersion:   currentSnapshotSchemaVersion,
		PermissionState: permBlob,
		HostArch:        runtime.GOOS + "/" + runtime.GOARCH,
		CreatedAt:       time.Now(),
	}

	sm2 := NewSessionManager(&mockLoaderForRestore{handle: h})
	sm2.PauseToolCallDrainTimeout = 5 * time.Second
	sess, err := sm2.Restore(ctx, "s1", "test", OnCrashFail, nil, nil, snap)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	restored := sess.PermissionState
	if restored == nil {
		t.Fatal("expected permission state to be restored")
	}
	restored.mu.Lock()
	decisions := len(restored.decisions)
	window := restored.pauseDrainWindow
	restored.mu.Unlock()
	if decisions != 1 {
		t.Errorf("restored decision window holds %d entries, want the snapshot's 1", decisions)
	}
	if window != 5*time.Second {
		t.Errorf("stamped pause drain window = %v, want 5s", window)
	}
}
