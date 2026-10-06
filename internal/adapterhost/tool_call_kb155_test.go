package adapterhost

// tool_call_kb155_test.go — KB-155: request_id-multiplexed concurrent tool
// calls over ONE v2 adapter session. The suite covers both sides of the
// capability:
//
//   - sessions whose adapter declares the concurrent_execute capability fan
//     independent caller executes out over the shared session, each execute
//     deciding permissions under its own step policy and each reply
//     correlated per request_id;
//   - sessions without the capability stay serialized by the execute turn
//     gate: a second caller's execute queues, and cancelling while queued
//     produces the typed canceled call_error without losing calls;
//   - the per-execute sink registry and the turn gate's own semantics.
//
// The tests are written to fail deterministically against a pre-KB-155 tree:
// the shared-session fan-out needs the capability route, the serialization,
// the attribute-on-cancel, and the per-execute policy attribution.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/workflow"
)

// kb155WaitFor polls cond until it holds, failing the test after 5s.
// Condition functions must not take locks the polled goroutine holds.
func kb155WaitFor(t *testing.T, msg string, cond func() bool) {
	t.Helper()
	const timeout = 5 * time.Second
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s: %s", timeout, msg)
}

func kb155WaitGroupWithTimeout(t *testing.T, wg *sync.WaitGroup, timeout time.Duration, msg string) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatal(msg)
	}
}

// calleeRecordCount snapshots the recorder's execute count.
func calleeRecordCount(rec *nestedCalleeRecorder) int {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return len(rec.sess)
}

// indexWhere returns the stream index of the first event of kind whose
// payload matches, or -1.
func indexWhere(t *testing.T, c *adapterEventCollector, kind string, match func(map[string]any) bool) int {
	t.Helper()
	for i, evt := range collectAllEvents(c) {
		if evt.kind == kind && match(evt.data) {
			return i
		}
	}
	return -1
}

// TestNestedToolCall_ConcurrentCallersSharedCalleeSession (KB-155): two
// independent caller sessions issue tool calls over ONE shared callee
// session whose adapter declares concurrent_execute. Both nested executes
// run concurrently on that one session — the second caller reaches the
// callee while the first is still blocked on its hold — and each caller
// receives its own correlated reply with its own call's outputs.
//
// The overlap observation is ordered, not racy: the second caller's
// dispatch is only confirmed once the callee is executing both calls
// (record count 2), while the blocked first call cannot have finished
// (its hold is still closed).
func TestNestedToolCall_ConcurrentCallersSharedCalleeSession(t *testing.T) {
	audit := &sliceAuditWriter{}
	calleeRec := &nestedCalleeRecorder{}
	holdRelease := make(chan struct{})
	callee := &nestedCalleeAdapter{rec: calleeRec, holdRelease: holdRelease,
		caps: []string{"execute", concurrentExecuteCapability}}
	caller1 := &nestedCallerAdapter{target: nestedCallTarget, args: map[string]any{"task": "hold"}}
	caller2 := &nestedCallerAdapter{target: nestedCallTarget, args: map[string]any{"task": "slow"}}

	// Separate fake instances per caller type: loader factories are keyed by
	// adapter type, and sharing one caller fake would share its permission
	// requests channel across two sessions.
	loader := NewLoaderWithDiscovery(func(string) (string, error) { return "", nil })
	loader.RegisterBuiltin("caller1", func() Handle { return caller1 })
	loader.RegisterBuiltin("caller2", func() Handle { return caller2 })
	loader.RegisterBuiltin("callee", func() Handle { return callee })
	sm := NewSessionManager(loader)
	sm.Audit = audit

	graph := &workflow.FSMGraph{
		Adapters: map[string]*workflow.AdapterNode{
			"caller1.instance": {Type: "caller1", Name: "instance"},
			"caller2.instance": {Type: "caller2", Name: "instance"},
			"callee.helper":    {Type: "callee", Name: "helper", DynamicTools: true},
		},
	}
	sm.SetGraph(graph)
	ctx := context.Background()
	if err := sm.Open(ctx, nestedCalleeSession, "callee", "", nil, nil); err != nil {
		t.Fatalf("Open callee: %v", err)
	}
	defer func() { _ = sm.Close(ctx, nestedCalleeSession) }()
	if err := sm.Open(ctx, "caller1.instance", "caller1", "", nil, nil); err != nil {
		t.Fatalf("Open caller1: %v", err)
	}
	defer func() { _ = sm.Close(ctx, "caller1.instance") }()
	if err := sm.Open(ctx, "caller2.instance", "caller2", "", nil, nil); err != nil {
		t.Fatalf("Open caller2: %v", err)
	}
	defer func() { _ = sm.Close(ctx, "caller2.instance") }()
	// Caller steps must not carry other steps' allow_tools.
	step1 := &workflow.StepNode{Name: "call-a", AdapterRef: "caller1.instance", AllowTools: []string{"adapter.callee.helper.tools.*"}}
	step2 := &workflow.StepNode{Name: "call-b", AdapterRef: "caller2.instance", AllowTools: []string{"adapter.callee.helper.tools.*"}}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if _, err := sm.Execute(ctx, "caller1.instance", step1, &adapterEventCollector{}, nil); err != nil {
			t.Errorf("caller1 Execute: %v", err)
		}
	}()

	// Confirm caller1's nested dispatch reached the shared callee session...
	kb155WaitFor(t, "callee executing caller1's call", func() bool {
		return calleeRecordCount(calleeRec) >= 1
	})
	// ...then fan the second caller onto the same session. With the
	// capability route broken (the shared session turn-gated), caller2 would
	// queue behind caller1's still-held call and never reach the callee.
	go func() {
		defer wg.Done()
		if _, err := sm.Execute(ctx, "caller2.instance", step2, &adapterEventCollector{}, nil); err != nil {
			t.Errorf("caller2 Execute: %v", err)
		}
	}()
	kb155WaitFor(t, "callee executing both calls concurrently on ONE session", func() bool {
		return calleeRecordCount(calleeRec) >= 2
	})

	// Caller1 is provably still in flight (its hold is closed), so the
	// observed caller2 start is true same-session overlap, not serialization.
	close(holdRelease)
	kb155WaitGroupWithTimeout(t, &wg, 10*time.Second, "caller executes did not settle")

	for name, caller := range map[string]*nestedCallerAdapter{"caller1": caller1, "caller2": caller2} {
		tcr := caller.gotResult()
		if tcr == nil {
			t.Fatalf("%s got no tool_call_result", name)
		}
		if tcr.CallError != "" || tcr.Outcome != "success" {
			t.Errorf("%s result = %q/%q, want success with no error", name, tcr.Outcome, tcr.CallError)
		}
	}
	// Each reply carries its own call's outputs — correlation per request_id,
	// not per session slot order.
	if report := nestedCalleeReport(t, caller1.gotResult()); report != "hold" {
		t.Errorf("caller1 outputs.report = %q, want hold", report)
	}
	if report := nestedCalleeReport(t, caller2.gotResult()); report != "slow" {
		t.Errorf("caller2 outputs.report = %q, want slow", report)
	}

	// Both calls executed, on the ONE shared callee session.
	if got := calleeRecordCount(calleeRec); got != 2 {
		t.Fatalf("callee executed %d times, want 2 concurrent executes on one session", got)
	}
	for i := 0; i < 2; i++ {
		calleeRec.mu.Lock()
		gotSession, gotTask := calleeRec.sess[i], calleeRec.steps[i].Input["task"]
		calleeRec.mu.Unlock()
		if gotSession != nestedCalleeSession {
			t.Errorf("callee execute %d in session %q, want the shared session %q", i, gotSession, nestedCalleeSession)
		}
		if gotTask != "hold" && gotTask != "slow" {
			t.Errorf("callee execute %d task = %q, want one of hold/slow (launch order)", i, gotTask)
		}
	}
}

// nestedCalleeReport decodes a successful tool_call_result's outputs to the
// callee's report string (both callee fakes emit the report/count contract).
func nestedCalleeReport(t *testing.T, tcr *v2.ToolCallResult) string {
	t.Helper()
	if tcr == nil {
		return ""
	}
	vals, err := ctyjson.Unmarshal(tcr.OutputsJson, cty.Object(map[string]cty.Type{"report": cty.String, "count": cty.Number}))
	if err != nil {
		t.Fatalf("decode outputs: %v", err)
	}
	return vals.GetAttr("report").AsString()
}

// TestNestedToolCall_NonMultiplexedCalleeSerializes (KB-155): a callee
// session without the concurrent_execute capability is turn-gated — the
// second tool call queues until the first settles, so the adapter never
// observes two in-flight executes. Both calls still complete with their own
// outputs.
func TestNestedToolCall_NonMultiplexedCalleeSerializes(t *testing.T) {
	audit := &sliceAuditWriter{}
	calleeRec := &nestedCalleeRecorder{}
	callee := &cri163Callee{rec: calleeRec, delay: 150 * time.Millisecond}
	sm := newNestedToolCallManager(t, &nestedCallerAdapter{target: nestedCallTarget}, callee)
	sm.Audit = audit
	sm.SetGraph(compileNestedToolCallGraph(t))
	ctx := context.Background()
	if err := sm.VerifyGraph(ctx, sm.graph, nil); err != nil {
		t.Fatalf("VerifyGraph: %v", err)
	}
	if err := sm.Open(ctx, nestedCalleeSession, "callee", "", nil, nil); err != nil {
		t.Fatalf("Open callee: %v", err)
	}
	defer func() { _ = sm.Close(ctx, nestedCalleeSession) }()

	sink, ps := directToolCallSink(t, sm, audit, nestedCallerStep(), sm.graph, 0)
	col := sink.inner.(*adapterEventCollector)

	sink.Adapter("permission.request", map[string]any{
		"request_id": "call-1",
		"target":     nestedCallTarget,
		"args":       map[string]any{"task": "slow-a"},
	})
	sink.Adapter("permission.request", map[string]any{
		"request_id": "call-2",
		"target":     nestedCallTarget,
		"args":       map[string]any{"task": "slow-b"},
	})
	results := readToolCallResults(t, ps, 2)
	byID := map[string]*v2.ToolCallResult{}
	for _, tcr := range results {
		byID[tcr.RequestId] = tcr
	}
	for _, want := range []struct{ id, report string }{{"call-1", "slow-a"}, {"call-2", "slow-b"}} {
		tcr, ok := byID[want.id]
		if !ok {
			t.Fatalf("no tool_call_result for %s (replies: %d)", want.id, len(byID))
		}
		if tcr.CallError != "" || tcr.Outcome != "success" {
			t.Errorf("%s = %q/%q, want success without error", want.id, tcr.Outcome, tcr.CallError)
		}
		if report := nestedCalleeReport(t, tcr); report != want.report {
			t.Errorf("%s outputs.report = %q, want %q (correlated per request_id)", want.id, report, want.report)
		}
	}

	// Serialization: the two callee executes must not overlap — whichever
	// execute grabbed the turn first completes before the other starts. The
	// scheduling order of the two dispatch goroutines is unspecified, so the
	// property is checked on the started markers: exactly one callee.finished
	// lies between the two callee.started events.
	sA := indexWhere(t, col, "callee.started", func(d map[string]any) bool { return d["task"] == "slow-a" })
	sB := indexWhere(t, col, "callee.started", func(d map[string]any) bool { return d["task"] == "slow-b" })
	if sA < 0 || sB < 0 {
		t.Fatalf("missing callee.started markers (slow-a@%d slow-b@%d)", sA, sB)
	}
	first, second := sA, sB
	if sB < sA {
		first, second = sB, sA
	}
	finishes := 0
	for _, ev := range collectAllEvents(col)[first+1 : second] {
		if ev.kind == "callee.finished" {
			finishes++
		}
	}
	if finishes != 1 {
		t.Fatalf("the two callee executes were not serialized: %d callee.finished between started@%d and started@%d (without the turn gate both execute at once and there is no finished between the starteds)", finishes, first, second)
	}

	// Both calls ran the callee, in its own session.
	if got := calleeRecordCount(calleeRec); got != 2 {
		t.Fatalf("callee executed %d times, want 2", got)
	}
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		calleeRec.mu.Lock()
		gotSession, gotTask := calleeRec.sess[i], calleeRec.steps[i].Input["task"]
		calleeRec.mu.Unlock()
		if gotSession != nestedCalleeSession {
			t.Errorf("callee execute %d in session %q, want %q", i, gotSession, nestedCalleeSession)
		}
		if gotTask != "slow-a" && gotTask != "slow-b" {
			t.Errorf("callee execute %d task = %q", i, gotTask)
		}
		seen[gotTask] = true
	}
	if !seen["slow-a"] || !seen["slow-b"] {
		t.Errorf("callee tasks %v, want exactly one slow-a and one slow-b (scheduling order unspecified)", seen)
	}
}

// TestNestedToolCall_QueuedCallCancelTyped (KB-155): cancelling the Execute
// context while a second call sits queued behind a single-flight in-flight
// call (CRI-162 sibling gate) typed `canceled` replies BOTH the in-flight
// call and the queued call, and the queued call never runs.
func TestNestedToolCall_QueuedCallCancelTyped(t *testing.T) {
	audit := &sliceAuditWriter{}
	calleeRec := &nestedCalleeRecorder{}
	callee := &cri163Callee{rec: calleeRec, delay: 150 * time.Millisecond}
	sm := newNestedToolCallManager(t, &nestedCallerAdapter{target: nestedCallTarget}, callee)
	sm.Audit = audit
	sm.SetGraph(nestedToolCallGraph())
	execCtx, cancelExec := context.WithCancel(context.Background())
	defer cancelExec()
	if err := sm.Open(context.Background(), nestedCalleeSession, "callee", "", nil, nil); err != nil {
		t.Fatalf("Open callee: %v", err)
	}
	defer func() { _ = sm.Close(context.Background(), nestedCalleeSession) }()

	sink, ps := directToolCallSink(t, sm, audit, nestedCallerStep(), sm.graph, 0)
	sink = withExecCtx(sink, execCtx)
	col := sink.inner.(*adapterEventCollector)

	sink.Adapter("permission.request", map[string]any{
		"request_id": "call-1",
		"target":     nestedCallTarget,
		"args":       map[string]any{"task": "slow-a"},
	})
	// Confirm call-1 owns the callee (it started its only execute), then
	// issue call-2: it queues behind the single-flight in-flight call.
	kb155WaitFor(t, "call-1 reached the callee", func() bool {
		return indexWhere(t, col, "callee.started", func(d map[string]any) bool { return d["task"] == "slow-a" }) >= 0
	})
	sink.Adapter("permission.request", map[string]any{
		"request_id": "call-2",
		"target":     nestedCallTarget,
		"args":       map[string]any{"task": "slow-b"},
	})
	kb155WaitFor(t, "call-2 registered for correlation", func() bool {
		return pendingCount(t, ps) == 2
	})

	// Cancel the owning Execute: the in-flight call is abandoned AND the
	// queued call never starts.
	cancelExec()

	byID := map[string]*v2.ToolCallResult{}
	for _, tcr := range readToolCallResults(t, ps, 2) {
		byID[tcr.RequestId] = tcr
	}
	for _, id := range []string{"call-1", "call-2"} {
		tcr, ok := byID[id]
		if !ok {
			t.Fatalf("no tool_call_result for %s (replies: %d)", id, len(byID))
		}
		if tcr.CallError != callErrorCanceled || tcr.Outcome != "" {
			t.Errorf("%s result = %q/%q, want empty outcome with %s", id, tcr.Outcome, tcr.CallError, callErrorCanceled)
		}
	}
	sink.waitPending()
	if got := calleeRecordCount(calleeRec); got != 1 {
		t.Errorf("callee executed %d times, want 1 (the queued call must never run)", got)
	}
	if got := pendingCount(t, ps); got != 0 {
		t.Errorf("pending registry = %d after cancellation, want 0", got)
	}
}

// TestNestedToolCall_PerCallPolicyAttribution (KB-155): two concurrent
// executes on ONE multiplexed session each decide their adapter's permission
// requests under their OWN step's allow_tools. With a session-global policy
// snapshot the second execute's allow_tools would decide the first
// execute's requests (last-writer-wins) — here alpha's request for its own
// family grants under alpha's policy while alpha's probe of beta's family
// denies, regardless of beta executing concurrently on the same session.
func TestNestedToolCall_PerCallPolicyAttribution(t *testing.T) {
	audit := &sliceAuditWriter{}
	calleeRec := &nestedCalleeRecorder{}
	callee := &cri163Callee{rec: calleeRec, delay: 200 * time.Millisecond,
		// Granted family last, so each execute's final decision is a grant
		// and the deny does not poison the outcome verdicts.
		permTools: []string{"beta.beta_tool", "alpha.alpha_tool"},
		caps:      []string{"execute", concurrentExecuteCapability}}
	loader := NewLoaderWithDiscovery(func(string) (string, error) { return "", nil })
	loader.RegisterBuiltin("callee", func() Handle { return callee })
	sm := NewSessionManager(loader)
	sm.Audit = audit
	sm.SetGraph(&workflow.FSMGraph{Adapters: map[string]*workflow.AdapterNode{
		"callee.helper": {Type: "callee", Name: "helper"},
	}})
	ctx := context.Background()
	if err := sm.Open(ctx, nestedCalleeSession, "callee", "", nil, nil); err != nil {
		t.Fatalf("Open callee: %v", err)
	}
	defer func() { _ = sm.Close(ctx, nestedCalleeSession) }()

	alphaStep := &workflow.StepNode{Name: "alpha", AdapterRef: nestedCalleeSession, AllowTools: []string{"alpha.*"}}
	betaStep := &workflow.StepNode{Name: "beta", AdapterRef: nestedCalleeSession, AllowTools: []string{"beta.*"}}
	colA, colB := &adapterEventCollector{}, &adapterEventCollector{}

	// Beta's trailing decision is its denied cross-probe (the callee's
	// perm list ends on alpha.alpha_tool, which beta.* denies): the KB-57c
	// trailing-deny override marks the success needs_review. Alpha's
	// trailing decision is its own grant, so it stays plain success.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		res, err := sm.Execute(ctx, nestedCalleeSession, alphaStep, colA, nil)
		if err != nil || res.Outcome != "success" {
			t.Errorf("alpha Execute = %q/%v, want success (the trailing grant must recover)", res.Outcome, err)
		}
	}()
	kb155WaitFor(t, "alpha executing", func() bool {
		return calleeRecordCount(calleeRec) >= 1
	})
	go func() {
		defer wg.Done()
		res, err := sm.Execute(ctx, nestedCalleeSession, betaStep, colB, nil)
		if err != nil || res.Outcome != "needs_review" {
			t.Errorf("beta Execute = %q/%v, want needs_review (its trailing decision is the denied cross-probe)", res.Outcome, err)
		}
	}()
	kb155WaitGroupWithTimeout(t, &wg, 10*time.Second, "concurrent executes did not settle")

	// alpha's requests decided under ALPHA's step policy: its own tool
	// granted, the beta probe denied. request_id-corrected plain decisions
	// (the callee emits callee-perm-0 for the beta request, callee-perm-1
	// for the alpha request).
	assertAttributedDecisions(t, colA, "alpha", "callee-perm-1", "alpha.alpha_tool", "callee-perm-0", "beta.beta_tool")
	assertAttributedDecisions(t, colB, "beta", "callee-perm-0", "beta.beta_tool", "callee-perm-1", "alpha.alpha_tool")

	if got := calleeRecordCount(calleeRec); got != 2 {
		t.Errorf("callee executed %d times, want 2 concurrent executes on the one shared session", got)
	}
}

// assertAttributedDecisions checks that col granted the matched request and
// denied the other, each carrying its own request_id — the per-execute
// policy attribution contract.
func assertAttributedDecisions(t *testing.T, col *adapterEventCollector, stepLabel, grantedID, grantedTool, deniedID, deniedTool string) {
	t.Helper()
	grantedEvents, deniedEvents := 0, 0
	for _, evt := range collectAllEvents(col) {
		switch evt.kind {
		case "permission.granted":
			if evt.data["tool"] == grantedTool {
				grantedEvents++
				if evt.data["request_id"] != grantedID {
					t.Errorf("%s grant request_id = %v, want %q", stepLabel, evt.data["request_id"], grantedID)
				}
			} else {
				t.Errorf("%s unexpectedly granted %v", stepLabel, evt.data["tool"])
			}
		case "permission.denied":
			if evt.data["tool"] == deniedTool {
				deniedEvents++
				if evt.data["request_id"] != deniedID {
					t.Errorf("%s deny request_id = %v, want %q", stepLabel, evt.data["request_id"], deniedID)
				}
			} else {
				t.Errorf("%s unexpectedly denied %v", stepLabel, evt.data["tool"])
			}
		}
	}
	if grantedEvents != 1 {
		t.Errorf("%s granted own tool %d times, want exactly 1; events: %+v", stepLabel, grantedEvents, collectAllEvents(col))
	}
	if deniedEvents != 1 {
		t.Errorf("%s denied other tool %d times, want exactly 1; events: %+v", stepLabel, deniedEvents, collectAllEvents(col))
	}
}

// TestNestedToolCall_BudgetUnderConcurrency (KB-58 + KB-155): the call-count
// budget is accounted per caller session across concurrent executes — a
// second concurrent call over an exhausted budget refused with typed
// budget_exhausted while the first stays in flight, and the first call
// still completes normally when its hold releases.
func TestNestedToolCall_BudgetUnderConcurrency(t *testing.T) {
	audit := &sliceAuditWriter{}
	calleeRec := &nestedCalleeRecorder{}
	holdRelease := make(chan struct{})
	callee := &nestedCalleeAdapter{rec: calleeRec, holdRelease: holdRelease,
		caps: []string{"execute", concurrentExecuteCapability}}
	sm := newNestedToolCallManager(t, &nestedCallerAdapter{target: nestedCallTarget}, callee)
	sm.Audit = audit
	graph := compileNestedToolCallGraph(t)
	graph.Policy = workflow.Policy{MaxToolCalls: 1}
	sm.SetGraph(graph)
	ctx := context.Background()
	if err := sm.VerifyGraph(ctx, graph, nil); err != nil {
		t.Fatalf("VerifyGraph: %v", err)
	}
	if err := sm.Open(ctx, nestedCalleeSession, "callee", "", nil, nil); err != nil {
		t.Fatalf("Open callee: %v", err)
	}
	defer func() { _ = sm.Close(ctx, nestedCalleeSession) }()

	sink, ps := directToolCallSink(t, sm, audit, nestedCallerStep(), graph, 0)

	sink.Adapter("permission.request", map[string]any{
		"request_id": "call-1",
		"target":     nestedCallTarget,
		"args":       map[string]any{"task": "hold"},
	})
	kb155WaitFor(t, "call-1 registered (budget reserved)", func() bool {
		return pendingCount(t, ps) == 1
	})

	// call-1 consumed the whole budget: the second concurrent call is
	// refused synchronously with budget_exhausted.
	sink.Adapter("permission.request", map[string]any{
		"request_id": "call-2",
		"target":     nestedCallTarget,
		"args":       map[string]any{"task": "slow"},
	})
	// call-1 consumed the whole budget: the second concurrent call is
	// refused synchronously with budget_exhausted while the first stays
	// in flight (registered, holding on the fixture's release channel).
	tcr := readToolCallResult(t, ps)
	if tcr.RequestId != "call-2" || tcr.CallError != callErrorBudgetExhausted {
		t.Errorf("call-2 result = %s/%q, want budget_exhausted", tcr.RequestId, tcr.CallError)
	}
	if got := pendingCount(t, ps); got != 1 {
		t.Fatalf("pending registry = %d while the first call holds, want 1 (the in-flight is not abandoned by the refusal)", got)
	}

	// The in-flight call settles normally when its hold releases.
	close(holdRelease)
	tcr1 := readToolCallResult(t, ps)
	if tcr1.RequestId != "call-1" || tcr1.CallError != "" || tcr1.Outcome != "success" {
		t.Errorf("call-1 result = %s/%q/%q, want success", tcr1.RequestId, tcr1.Outcome, tcr1.CallError)
	}
	if report := nestedCalleeReport(t, tcr1); report != "hold" {
		t.Errorf("call-1 outputs.report = %q, want hold", report)
	}
	sink.waitPending()
	if got := calleeRecordCount(calleeRec); got != 1 {
		t.Errorf("callee executed %d times, want 1", got)
	}
	if got := pendingCount(t, ps); got != 0 {
		t.Errorf("pending registry = %d after settle, want 0", got)
	}
}

// TestSession_ActiveSinkRegistry (KB-155): the per-execute sink registry is
// refcounted and the attribution single-sink rule only fires when exactly
// one execute (or one sink, possibly serving sibling nested executes) is
// bound.
func TestSession_ActiveSinkRegistry(t *testing.T) {
	calleeRec := &nestedCalleeRecorder{}
	sm := newNestedToolCallManager(t, &nestedCallerAdapter{target: nestedCallTarget}, &nestedCalleeAdapter{rec: calleeRec})
	sm.SetGraph(nestedToolCallGraph())
	ctx := context.Background()
	if err := sm.Open(ctx, nestedCalleeSession, "callee", "", nil, nil); err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = sm.Close(ctx, nestedCalleeSession) }()
	sess, err := sm.lookup(nestedCalleeSession)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}

	sinkA, sinkB := &adapterEventCollector{}, &adapterEventCollector{}

	if _, ok := sess.singleActiveSink(); ok {
		t.Error("registry should be empty after open, but single-sink attribution fired")
	}
	sm.bindActiveSink(sess, sinkA)
	if got, ok := sess.singleActiveSink(); !ok || got != adapter.EventSink(sinkA) {
		t.Error("one bound execute must expose single-sink attribution")
	}
	// A sibling nested execute sharing sinkA refcounts the same entry.
	sm.bindActiveSink(sess, sinkA)
	if got, ok := sess.singleActiveSink(); !ok || got != adapter.EventSink(sinkA) {
		t.Error("two references to the same sink must still expose single-sink attribution")
	}
	// A second distinct execute is ambiguous attribution: no single sink.
	sm.bindActiveSink(sess, sinkB)
	if _, ok := sess.singleActiveSink(); ok {
		t.Error("two distinct bound sinks must not expose single-sink attribution")
	}
	sm.unbindActiveSink(sess, sinkB)
	if got, ok := sess.singleActiveSink(); !ok || got != adapter.EventSink(sinkA) {
		t.Error("binding sinkA again (one distinct) must restore single-sink attribution")
	}
	sm.unbindActiveSink(sess, sinkA)
	sm.unbindActiveSink(sess, sinkA)
	if _, ok := sess.singleActiveSink(); ok {
		t.Error("fully unbound registry must not expose single-sink attribution")
	}
}

// TestSession_ExecuteTurnGate (KB-155): the turn gate serializes executes on
// non-multiplexable sessions (including cancel-while-queued returning
// ctx.Err() without consuming a turn) and is skipped outright for sessions
// declaring concurrent_execute.
func TestSession_ExecuteTurnGate(t *testing.T) {
	audit := &sliceAuditWriter{}
	calleeRec := &nestedCalleeRecorder{}
	mxRec := &nestedCalleeRecorder{}
	loader := NewLoaderWithDiscovery(func(string) (string, error) { return "", nil })
	loader.RegisterBuiltin("caller", func() Handle { return &nestedCallerAdapter{target: nestedCallTarget} })
	loader.RegisterBuiltin("callee", func() Handle { return &nestedCalleeAdapter{rec: calleeRec} })
	loader.RegisterBuiltin("callee2", func() Handle {
		return &cri163Callee{rec: mxRec, caps: []string{"execute", concurrentExecuteCapability}}
	})
	sm := NewSessionManager(loader)
	sm.Audit = audit
	sm.SetGraph(&workflow.FSMGraph{
		Adapters: map[string]*workflow.AdapterNode{
			"caller.instance": {Type: "caller", Name: "instance"},
			"callee.helper":   {Type: "callee", Name: "helper"},
			"callee2.mx":      {Type: "callee2", Name: "mx"},
		},
	})
	ctx := context.Background()
	if err := sm.Open(ctx, nestedCalleeSession, "callee", "", nil, nil); err != nil {
		t.Fatalf("Open callee: %v", err)
	}
	defer func() { _ = sm.Close(context.Background(), nestedCalleeSession) }()
	if err := sm.Open(ctx, "callee2.mx", "callee2", "", nil, nil); err != nil {
		t.Fatalf("Open callee2.mx: %v", err)
	}
	defer func() { _ = sm.Close(context.Background(), "callee2.mx") }()

	// Non-multiplexable: the first acquire takes the turn.
	sess, err := sm.lookup(nestedCalleeSession)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if queued, gateErr := sm.acquireExecuteTurn(ctx, sess); gateErr != nil || queued {
		t.Fatalf("first acquire: queued=%v err=%v, want immediate non-queued turn", queued, gateErr)
	}
	// A second caller queued behind it with a deadline: released only by
	// context cancellation, not by a phantom turn.
	queuedCtx, cancelQueued := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancelQueued()
	if queued, gateErr := sm.acquireExecuteTurn(queuedCtx, sess); !errors.Is(gateErr, context.DeadlineExceeded) || !queued {
		t.Errorf("cancel-while-queued err = %v queued=%v, want context.DeadlineExceeded queued", queued, gateErr)
	}
	// The cancelled waiter must NOT have consumed the turn: releasing hands
	// it to the next waiter, which acquires immediately.
	sm.releaseExecuteTurn(sess)
	if queued, gateErr := sm.acquireExecuteTurn(ctx, sess); gateErr != nil || queued {
		t.Fatalf("acquire after release: queued=%v err=%v", gateErr, queued)
	}
	sm.releaseExecuteTurn(sess)

	// Multiplexable: the gate is skipped and the turn stays untouched.
	mxSess, err := sm.lookup("callee2.mx")
	if err != nil {
		t.Fatalf("lookup mx: %v", err)
	}
	if queued, gateErr := sm.acquireExecuteTurn(ctx, mxSess); gateErr != nil || queued {
		t.Errorf("multiplexable acquire: queued=%v err=%v, want nil (gate skipped)", gateErr, queued)
	}
}
