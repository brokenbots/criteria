package conformance

// conformance_pause_midcall.go — CRI-169 conformance coverage for the
// drain-first pause posture of nested adapter tool calls (ADR-0004 §11):
//
//  1. pause_drains_in_flight — pausing while a nested Execute is in flight
//     blocks until the in-flight call drains within the bounded window (its
//     typed reply is delivered while the stream is still live, and the call
//     is not canceled), and after Resume the session makes further calls
//     that work (resume continuity).
//  2. pause_cancels_straggler — a nested call that does not settle within
//     the bounded window is canceled (typed `canceled` call_error, the
//     callee observes the context cancellation), a call issued while the
//     gate is up is refused with the typed `paused` call_error before it
//     starts (the callee never executes it), and after Resume further calls
//     work.
//  3. snapshot_restore_no_inflight — a snapshot taken when no nested call is
//     in flight round-trips through restore cleanly and the restored
//     sessions make further calls that work.
//
// Every case asserts the run-continuation invariant alongside the
// call-level behavior: the pause semantics are data for the caller, not a
// run failure.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/internal/engine"
	"github.com/brokenbots/criteria/workflow"
)

// pauseMidCallDrainWindow bounds the pause drain in the straggler case. The
// production default is 60s; conformance uses a short window so the
// straggler path exercises in bounded time (the engine option exists for
// exactly this).
const pauseMidCallDrainWindow = 300 * time.Millisecond

// pauseMidCallWaitTimeout bounds every mid-call poll (in-flight waits,
// reply waits, run completion). Generous: the host settles every path long
// before this fires.
const pauseMidCallWaitTimeout = 10 * time.Second

// waitForCalleeExecutions polls until the callee has recorded n executions
// (the calls are in flight).
func waitForCalleeExecutions(t *testing.T, callee *matrixCalleeAdapter, n int) {
	t.Helper()
	deadline := time.Now().Add(pauseMidCallWaitTimeout)
	for time.Now().Before(deadline) {
		if len(callee.executions()) >= n {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("callee never reached %d execution(s); recorded %+v", n, callee.executions())
}

// waitForCallerResults polls until the caller has recorded n typed replies.
func waitForCallerResults(t *testing.T, caller *matrixCallerAdapter, n int) []matrixReply {
	t.Helper()
	deadline := time.Now().Add(pauseMidCallWaitTimeout)
	for time.Now().Before(deadline) {
		if results := caller.gotResults(); len(results) >= n {
			return results
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("caller never received %d typed repl(ies); recorded %+v", n, caller.gotResults())
	return nil
}

// runMatrixCaseAsync drives a matrix case through the real engine on its own
// goroutine so a case can pause the run mid-Execute (the conformance
// equivalent of a server-issued pause arriving while a step is executing).
func runMatrixCaseAsync(t *testing.T, src string, caller *matrixCallerAdapter, callee *matrixCalleeAdapter, opts ...engine.Option) (*engine.Engine, *matrixEngineSink, *matrixAuditCollector, <-chan error) {
	t.Helper()
	sink := &matrixEngineSink{}
	audit := &matrixAuditCollector{}
	loader := &matrixLoader{handles: map[string]adapterhost.Handle{
		"caller": caller,
		"callee": callee,
	}}
	e := engine.New(compileMatrixGraph(t, src), loader, sink,
		append([]engine.Option{engine.WithAuditWriter(audit)}, opts...)...)
	done := make(chan error, 1)
	go func() {
		done <- e.Run(context.Background())
	}()
	return e, sink, audit, done
}

// assertPauseReplies asserts the caller-visible replies of a pause case in
// delivery order. Field-wise on purpose: matrixReply carries an outputs map
// (not comparable with ==). A non-empty outcome is a successful call; an
// empty outcome with a call error is a typed failure.
func assertPauseReplies(t *testing.T, caller *matrixCallerAdapter, want []matrixReply) {
	t.Helper()
	results := caller.gotResults()
	if len(results) != len(want) {
		t.Fatalf("results received = %+v, want %d entries: %+v", results, len(want), want)
	}
	for i, w := range want {
		got := results[i]
		if got.requestID != w.requestID || got.callError != w.callError || got.outcome != w.outcome {
			t.Fatalf("reply %d = request %q call_error %q outcome %q, want request %q call_error %q outcome %q",
				i, got.requestID, got.callError, got.outcome, w.requestID, w.callError, w.outcome)
		}
		if w.outputs != nil {
			if got.outputs == nil || got.outputs["report"] != w.outputs["report"] {
				t.Fatalf("reply %d outputs = %v, want report %q", i, got.outputs, w.outputs["report"])
			}
		}
	}
	if cancels := caller.gotCancels(); len(cancels) != 0 {
		t.Fatalf("cancels received = %d, want 0 (pause semantics are typed call_errors, not permission cancels): %+v", len(cancels), cancels)
	}
}

// pauseMidCallGraph renders the shared workflow: a caller step allowed to
// tool-call the callee, routing on the caller's own "handled" outcome.
func pauseMidCallGraph(name string) string {
	return matrixGraph(name, "handled", "\n"+
		"  allow_tools = [\"adapter.callee.default.tools.*\"]\n"+
		"\n"+
		"  outcome \"handled\" {\n"+
		"    next = step.handled\n"+
		"  }\n"+
		"\n")
}

// pauseMidCallCaseDrain covers the drain-first posture with an in-flight
// call (CRI-169, ADR-0004 §11): the caller holds its call open inside the
// callee; the pause blocks instead of returning while the call is in flight
// (drain-first: the pause never abandons an in-flight call that can still
// settle); the released call settles inside the drain and its typed reply is
// delivered while the stream is still live; the callee never observed a
// context cancellation (a draining call is not canceled); and after Resume
// the caller's next call works — resume continuity.
func pauseMidCallCaseDrain(t *testing.T) {
	releaseCall2 := make(chan struct{})
	caller := newMatrixCaller([]string{"adapter_tools", "execute"}, "handled",
		matrixCall{requestID: "call-1", target: matrixToolCallTarget, args: map[string]any{"task": "hold"}},
		matrixCall{requestID: "call-2", target: matrixToolCallTarget, args: map[string]any{"task": "fast"}},
	)
	caller.beforeCall = func(index int) {
		if index == 1 {
			<-releaseCall2
		}
	}
	callee := newMatrixCallee()
	callee.holdRelease = make(chan struct{})

	e, sink, _, runDone := runMatrixCaseAsync(t, pauseMidCallGraph("adapter_tools_pause_drain"), caller, callee)

	// call-1 is in flight inside the callee's own session.
	waitForCalleeExecutions(t, callee, 1)

	// The pause blocks while the call is in flight (drain-first posture).
	pauseDone := make(chan error, 1)
	go func() { pauseDone <- e.Pause(context.Background()) }()
	select {
	case err := <-pauseDone:
		t.Fatalf("pause completed while the nested call was still in flight (drain-first posture violated): err=%v", err)
	case <-time.After(100 * time.Millisecond):
	}

	// The released call settles inside the drain: its reply is delivered
	// while the stream is still live, and the pause completes after it.
	close(callee.holdRelease)
	waitForCallerResults(t, caller, 1)
	if err := <-pauseDone; err != nil {
		t.Fatalf("engine pause: %v", err)
	}

	// A draining call is not canceled: the callee saw no context error.
	assertSingleHoldExecution(t, callee, false)

	// Resume continuity: after Resume the caller's next call works.
	if err := e.Resume(context.Background()); err != nil {
		t.Fatalf("engine resume: %v", err)
	}
	close(releaseCall2)
	waitForCallerResults(t, caller, 2)
	if err := <-runDone; err != nil {
		t.Fatalf("engine run: %v", err)
	}

	assertPauseReplies(t, caller, []matrixReply{
		{requestID: "call-1", outcome: "success", outputs: map[string]any{"report": "hold"}},
		{requestID: "call-2", outcome: "success", outputs: map[string]any{"report": "fast"}},
	})
	assertMatrixRunContinued(t, sink, "handled")
}

// pauseMidCallCaseStraggler covers the straggler path and the pause gate
// (CRI-169, ADR-0004 §11): with a 300ms drain window, the held call does not
// settle, so the drain cancels it — the callee observes the context
// cancellation and the caller receives the typed `canceled` call_error — a
// call issued while the gate is up is refused with the typed `paused`
// call_error before it starts (the callee never executes it), and after
// Resume the caller's next call succeeds. The run continues through the
// caller's own outcome routing.
func pauseMidCallCaseStraggler(t *testing.T) {
	releaseCall3 := make(chan struct{})
	caller := newMatrixCaller([]string{"adapter_tools", "execute"}, "handled",
		matrixCall{requestID: "call-1", target: matrixToolCallTarget, args: map[string]any{"task": "hold"}},
		matrixCall{requestID: "call-2", target: matrixToolCallTarget, args: map[string]any{"task": "fast"}},
		matrixCall{requestID: "call-3", target: matrixToolCallTarget, args: map[string]any{"task": "fast"}},
	)
	caller.beforeCall = func(index int) {
		if index == 2 {
			<-releaseCall3
		}
	}
	callee := newMatrixCallee()
	callee.holdRelease = make(chan struct{}) // never released: the straggler must be canceled
	defer close(callee.holdRelease)

	e, sink, audit, runDone := runMatrixCaseAsync(t, pauseMidCallGraph("adapter_tools_pause_cancels_straggler"), caller, callee,
		engine.WithPauseToolCallDrainTimeout(pauseMidCallDrainWindow))

	// call-1 is in flight and never settles.
	waitForCalleeExecutions(t, callee, 1)

	pauseDone := make(chan error, 1)
	go func() { pauseDone <- e.Pause(context.Background()) }()

	// The drain cancels the straggler: the caller receives the typed
	// `canceled` call_error, then its next call is refused with the typed
	// `paused` call_error before it starts (the gate is up). The caller then
	// holds before its third call until the test resumes the run.
	results := waitForCallerResults(t, caller, 2)
	assertTypedCallErrorReply(t, results[0], "call-1", "canceled")
	assertTypedCallErrorReply(t, results[1], "call-2", "paused")
	if err := <-pauseDone; err != nil {
		t.Fatalf("engine pause: %v", err)
	}

	// The canceled straggler's callee observed the context cancellation;
	// neither the canceled call nor the gate-refused call reached the
	// callee twice.
	assertSingleHoldExecution(t, callee, true)

	// Resume continuity: after Resume the caller's next call works.
	if err := e.Resume(context.Background()); err != nil {
		t.Fatalf("engine resume: %v", err)
	}
	close(releaseCall3)
	waitForCallerResults(t, caller, 3)
	if err := <-runDone; err != nil {
		t.Fatalf("engine run: %v", err)
	}

	assertPauseReplies(t, caller, []matrixReply{
		{requestID: "call-1", callError: "canceled"},
		{requestID: "call-2", callError: "paused"},
		{requestID: "call-3", outcome: "success", outputs: map[string]any{"report": "fast"}},
	})
	assertMatrixRunContinued(t, sink, "handled")

	// Audit: typed denies and allows (see the helper for the split).
	assertStragglerAuditDecisions(t, audit)
}

// assertTypedCallErrorReply asserts a caller result carrying only a typed
// call_error (empty outcome, no outputs): the visible shape of a pause-gate
// refusal or a drain cancellation.
func assertTypedCallErrorReply(t *testing.T, got matrixReply, requestID, callError string) {
	t.Helper()
	if got.requestID != requestID || got.callError != callError || got.outcome != "" {
		t.Fatalf("reply = request %q call_error %q outcome %q, want request %q call_error %q outcome empty",
			got.requestID, got.callError, got.outcome, requestID, callError)
	}
}

// assertSingleHoldExecution asserts the callee executed exactly one "hold"
// task in its own session and, when wantCanceled, that it observed the
// context cancellation the drain applies to stragglers (a drained call must
// not observe one).
func assertSingleHoldExecution(t *testing.T, callee *matrixCalleeAdapter, wantCanceled bool) {
	t.Helper()
	execs := callee.executions()
	if len(execs) != 1 || execs[0].sessionID != "callee.default" || execs[0].task != "hold" {
		t.Fatalf("callee executions = %+v, want one hold execution in session \"callee.default\"", execs)
	}
	if wantCanceled {
		if !errors.Is(execs[0].ctxErr, context.Canceled) {
			t.Fatalf("straggler context error = %v, want context.Canceled (the drain must cancel the nested context cleanly)", execs[0].ctxErr)
		}
		return
	}
	if execs[0].ctxErr != nil {
		t.Fatalf("drained call observed context error %v, want nil (an in-flight call that settles inside the drain window is not canceled)", execs[0].ctxErr)
	}
}

// assertStragglerAuditDecisions asserts the typed audit trail of the
// straggler case: the canceled call's nested failure and the paused-gate
// rejection are typed denies at the caller's layer, and only the two
// policy-evaluated calls (call-1, call-3) are allows — the gate-refused
// call was rejected before the policy gate.
func assertStragglerAuditDecisions(t *testing.T, audit *matrixAuditCollector) {
	t.Helper()
	denies := audit.auditDecisions("deny")
	if len(denies) != 2 {
		t.Fatalf("deny audit entries = %+v, want 2 (straggler cancellation + paused-gate rejection)", denies)
	}
	var canceledDeny, pausedDeny bool
	for i := range denies {
		entry := &denies[i]
		if entry.SessionID != "caller.default" || entry.Layer != 0 {
			t.Fatalf("deny audit entry = session %q layer %d, want session \"caller.default\" layer 0", entry.SessionID, entry.Layer)
		}
		switch {
		case strings.Contains(entry.Reason, "canceled") && strings.Contains(entry.RequestID, "call-1"):
			canceledDeny = true
		case strings.Contains(entry.Reason, "paused") && strings.Contains(entry.RequestID, "call-2"):
			pausedDeny = true
		}
	}
	if !canceledDeny || !pausedDeny {
		t.Fatalf("deny audit entries missing the typed cancellations: %+v (canceled=%v paused=%v)", denies, canceledDeny, pausedDeny)
	}
	if allows := audit.auditDecisions("allow"); len(allows) != 2 {
		t.Fatalf("allow audit entries = %d, want 2 (call-1 and call-3; the gate-refused call never reached the policy)", len(allows))
	}
}

// pauseMidCallCaseSnapshotRestore covers snapshot/restore with no in-flight
// call (CRI-169, ADR-0004 §11): a snapshot taken after the caller's call
// completed (nothing in flight) round-trips through restore, and the
// restored sessions make further calls that work.
func pauseMidCallCaseSnapshotRestore(t *testing.T) {
	ctx := context.Background()
	caller := newMatrixCaller([]string{"adapter_tools", "execute"}, "success",
		matrixCall{requestID: "call-1", target: matrixToolCallTarget, args: map[string]any{"task": "fast"}},
	)
	callee := newMatrixCallee()
	loader := &matrixLoader{handles: map[string]adapterhost.Handle{
		"caller": caller,
		"callee": callee,
	}}
	// The pre-snapshot run: the call completes, so nothing is in flight.
	graph := compileMatrixGraph(t, pauseMidCallGraph("adapter_tools_pause_snapshot_restore"))
	audit := &matrixAuditCollector{}
	step := &workflow.StepNode{
		Name:       "call",
		AdapterRef: "caller.default",
		AllowTools: []string{"adapter.callee.default.tools.*"},
		Outcomes: map[string]*workflow.CompiledOutcome{
			"success": {Name: "success"},
			"failure": {Name: "failure"},
		},
	}
	sm1 := newPauseMidCallManager(loader, graph, audit)
	snaps := snapshotAfterCompletedCall(t, ctx, sm1, step, caller)

	// Restore on a fresh manager (the engine's restart path) and drive a
	// further call through the restored sessions.
	sm2 := newPauseMidCallManager(loader, graph, audit)
	restorePauseMidCallSessions(t, ctx, sm2, snaps)
	sink2 := &matrixEngineSink{}
	if _, err := sm2.Execute(ctx, "caller.default", step, sink2.StepEventSink("call")); err != nil {
		t.Fatalf("post-restore execute: %v", err)
	}
	if results := caller.gotResults(); len(results) != 2 || results[1].outcome != "success" {
		t.Fatalf("post-restore replies = %+v, want a second success (further calls work after restore)", results)
	}
	assertPostRestoreCalls(t, callee)
}

// snapshotAfterCompletedCall opens the caller/callee sessions on the
// manager, drives the caller's scripted call to completion (nothing in
// flight), pauses — the drain returns immediately with nothing pending — and
// returns the session snapshots.
func snapshotAfterCompletedCall(t *testing.T, ctx context.Context, sm *adapterhost.SessionManager, step *workflow.StepNode, caller *matrixCallerAdapter) map[string]*adapterhost.SessionSnapshot {
	t.Helper()
	if err := sm.Open(ctx, "caller.default", "caller", "", nil, nil); err != nil {
		t.Fatalf("open caller: %v", err)
	}
	if err := sm.Open(ctx, "callee.default", "callee", "", nil, nil); err != nil {
		t.Fatalf("open callee: %v", err)
	}
	sink := &matrixEngineSink{}
	if _, err := sm.Execute(ctx, "caller.default", step, sink.StepEventSink("call")); err != nil {
		t.Fatalf("pre-snapshot execute: %v", err)
	}
	if results := caller.gotResults(); len(results) != 1 || results[0].outcome != "success" {
		t.Fatalf("pre-snapshot replies = %+v, want one success", results)
	}
	if err := sm.PauseAll(ctx); err != nil {
		t.Fatalf("pause all: %v", err)
	}
	snaps, err := sm.SnapshotAll(ctx)
	if err != nil {
		t.Fatalf("snapshot all: %v", err)
	}
	if len(snaps) != 2 {
		t.Fatalf("snapshots = %d sessions, want 2", len(snaps))
	}
	return snaps
}

// assertPostRestoreCalls asserts the restored sessions made further calls
// work: the callee executed twice (one call per manager), each in its own
// session with no context error.
func assertPostRestoreCalls(t *testing.T, callee *matrixCalleeAdapter) {
	t.Helper()
	execs := callee.executions()
	if len(execs) != 2 {
		t.Fatalf("callee executions = %d, want 2 (one before the snapshot, one after restore): %+v", len(execs), execs)
	}
	for _, exec := range execs {
		if exec.sessionID != "callee.default" || exec.ctxErr != nil {
			t.Fatalf("callee execution = session %q ctxErr %v, want session \"callee.default\" with no context error", exec.sessionID, exec.ctxErr)
		}
	}
}

// RunAdapterToolsPauseMidCallConformance runs the CRI-169 pause/resume
// mid-call conformance suite.
func RunAdapterToolsPauseMidCallConformance(t *testing.T) {
	t.Run("pause_drains_in_flight", pauseMidCallCaseDrain)
	t.Run("pause_cancels_straggler", pauseMidCallCaseStraggler)
	t.Run("snapshot_restore_no_inflight", pauseMidCallCaseSnapshotRestore)
}

// newPauseMidCallManager builds a SessionManager wired with the given
// loader, audit collector, and compiled graph for the pause/resume cases.
func newPauseMidCallManager(loader *matrixLoader, graph *workflow.FSMGraph, audit *matrixAuditCollector) *adapterhost.SessionManager {
	sm := adapterhost.NewSessionManager(loader)
	sm.Audit = audit
	sm.PauseToolCallDrainTimeout = 5 * time.Second
	sm.SetGraph(graph)
	return sm
}

// restorePauseMidCallSessions restores every snapshot on the manager, keyed
// by the adapter type parsed from the session name.
func restorePauseMidCallSessions(t *testing.T, ctx context.Context, sm *adapterhost.SessionManager, snaps map[string]*adapterhost.SessionSnapshot) {
	t.Helper()
	for name, snap := range snaps {
		adapterType, _, _ := strings.Cut(name, ".")
		if _, err := sm.Restore(ctx, name, adapterType, "", nil, nil, snap); err != nil {
			t.Fatalf("restore session %q: %v", name, err)
		}
	}
}
