// Adapter-tools depth/cycle conformance (CRI-168): automated coverage for the
// M6.2 runtime behaviors delivered by CRI-162, locked against ADR-0004 so a
// future refactor cannot silently drift from the ruled semantics
// (ADR-0004 §6 Cycle policy, §7 Validation posture, §8 Wire decision). Like
// the CRI-167 failure matrix, every scenario runs through the real engine
// against in-memory chain-hop fakes.
//
// Scenarios:
//
//  1. depth_exceeded — a four-adapter call chain under policy.max_tool_depth
//     = 2: the caller's call to hopb nests at depth 1 and hopb's call to
//     hopc nests at depth 2 (both within the bound); hopc's call to hopd
//     would nest at depth 3, one past the bound, so the depth gate fails
//     that call with the typed depth_exceeded error — and only that call.
//     The test asserts hopc received the typed reply, hopd never executed,
//     and the RUN CONTINUED: the caller's next outcome routing executed (the
//     only FSM transition is the caller step -> terminal through the
//     caller's declared outcome), per ADR-0004 §6 ("the run continues: a
//     failed tool call is data for the caller, not a run failure").
//  2. cycle_detected — a runtime cycle (hopb calls back to the caller
//     adapter it was reached from) fails only the offending call with the
//     typed cycle_detected error, while hopb's next call to hopc in the same
//     run completes unaffected (ADR-0004 §6: the compile-time check is a
//     warning; the runtime gate is the enforcement point). The scenario runs
//     without a policy block, so the documented default depth (8) is
//     exercised implicitly by the unaffected depth-2 call. The caller still
//     received a clean result for its own call, so the run continued through
//     the caller's own outcome routing.
//  3. compile_cycle_warning_advisory — a workflow whose adapter call graph
//     closes on itself compiles successfully with exactly one warning; the
//     test asserts no compile error is raised and the warning names the
//     runtime cap it defers to (policy.max_tool_depth, currently 8)
//     (ADR-0004 §6: the compile-time check is a warning, not an error).
//
// Audit assertions lock the enforcement fingerprint (ADR-0004 §6: "an audit
// entry is written for the enforcement event, as for other permission
// decisions"): the typed gate reject is a deny entry on the offending
// session at its own nesting layer, every policy-evaluated call is allowed
// at its layer, and each session's close summary counts the calls its policy
// actually evaluated. Observability assertions lock the CRI-163 event
// contract for dispatched calls (ADR-0004 §8: the call rides the Execute
// stream as permission.request, the result returns on the Permissions
// stream); a typed-gate-rejected call is never dispatched, so it emits no
// tool.call and no tool.call_result.
//
// This suite is host-side and unconditional: the depth and cycle gates live
// in the host runtime, not in adapters, so a per-adapter matrix suite
// (matrix.yaml gates suites on adapter capabilities) would skip in CI
// forever. It runs as its own always-on conformance entry point, mirroring
// the CRI-167 matrix and the engine's adapter_tool_call_loop_test.go
// template.
package conformance

import (
	"context"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/internal/engine"
	"github.com/brokenbots/criteria/workflow"
)

// Tool-surface targets on the depth/cycle chain. Every callee in the runtime
// scenarios declares a static helper_task surface, so graph validation
// (ADR-0004 §7) passes and the depth/cycle gates diagnose the call instead of
// unknown_tool.
const (
	depthCycleHopBTarget   = "adapter.hopb.default.tools.helper_task"
	depthCycleHopCTarget   = "adapter.hopc.default.tools.helper_task"
	depthCycleHopDTarget   = "adapter.hopd.default.tools.helper_task"
	depthCycleCallerTarget = "adapter.caller.default.tools.helper_task"
)

// Runtime glob patterns for the two policy surfaces: the caller step's
// step-level allow_tools gates the caller's own call, and the workflow-level
// permissions.allow_tools gates the nested callee sessions' own calls (the
// caller's allow_tools does not carry into the callee session — the synthetic
// callee step is assigned the declaring workflow's list).
const (
	depthCycleCallerGlob = "adapter.caller.default.tools.*"
	depthCycleHopBGlob   = "adapter.hopb.default.tools.*"
	depthCycleHopCGlob   = "adapter.hopc.default.tools.*"
	depthCycleHopDGlob   = "adapter.hopd.default.tools.*"
)

// chainHopExecution records one Execute of a chain hop: the session it ran in
// and the task input its step carried.
type chainHopExecution struct {
	sessionID string
	task      string
}

// chainHopAdapter is a chain hop fake: it executes as a nested callee (its
// result flows back up as its caller's tool result) and issues its own
// scripted adapter-tools calls, awaiting each typed reply before the next.
// The embedded matrixCallerAdapter supplies the Permissions-stream plumbing
// (stream capture, typed reply correlation and recording, cancel recording)
// and the no-op lifecycle methods; Execute adds the hop behaviors — an
// execution record and the matrix callee's derived report/count outputs, so
// every settled call in the chain carries its own observable result.
type chainHopAdapter struct {
	*matrixCallerAdapter

	identity string

	mu    sync.Mutex
	execs []chainHopExecution
}

// newChainHop builds a chain hop fake issuing the given scripted calls in
// order and completing successfully once its script settles — the outcome
// every depth/cycle scenario routes to its terminal state through.
func newChainHop(identity string, calls ...matrixCall) *chainHopAdapter {
	return &chainHopAdapter{
		matrixCallerAdapter: newMatrixCaller([]string{"adapter_tools", "execute"}, "success", calls...),
		identity:            identity,
	}
}

func (a *chainHopAdapter) Info(context.Context) (adapterhost.Info, error) {
	return adapterhost.Info{
		Name:         a.identity,
		Version:      "0.0.0-depth-cycle",
		Capabilities: []string{"adapter_tools", "execute"},
		AdapterInfo: workflow.AdapterInfo{
			InputSchema:  map[string]workflow.ConfigField{"task": {Required: true}},
			OutputSchema: matrixCalleeOutputSchema(),
		},
	}, nil
}

func (a *chainHopAdapter) Execute(_ context.Context, sessionID string, step *workflow.StepNode, sink adapter.EventSink) (adapter.Result, error) {
	task := step.Input["task"]
	a.mu.Lock()
	a.execs = append(a.execs, chainHopExecution{sessionID: sessionID, task: task})
	a.mu.Unlock()
	for _, call := range a.script {
		sink.Adapter("permission.request", map[string]any{
			"request_id": call.requestID,
			"target":     call.target,
			"args":       call.args,
		})
		if !a.awaitReply(call) {
			break
		}
	}
	return adapter.Result{
		Outcome: a.outcome,
		Outputs: map[string]cty.Value{
			"report": cty.StringVal(task),
			"count":  cty.NumberIntVal(int64(len(task))),
		},
	}, nil
}

func (a *chainHopAdapter) executions() []chainHopExecution {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]chainHopExecution(nil), a.execs...)
}

// assertNoCancels asserts the hop observed no permission cancels: a typed
// depth or cycle failure is a tool_call_result reply, not a deny (the policy
// gate granted the call before the runtime gate rejected it).
func (a *chainHopAdapter) assertNoCancels(t *testing.T) {
	t.Helper()
	if cancels := a.gotCancels(); len(cancels) != 0 {
		t.Fatalf("%s received cancels = %+v, want none", a.identity, cancels)
	}
}

// assertExecutions asserts the hop executed exactly once in the expected
// session with the expected task (an empty task matches the engine-targeted
// caller, whose step carries no input).
func (a *chainHopAdapter) assertExecutions(t *testing.T, sessionID, task string) {
	t.Helper()
	execs := a.executions()
	if len(execs) != 1 {
		t.Fatalf("%s executions = %+v, want exactly one", a.identity, execs)
	}
	if execs[0].sessionID != sessionID || execs[0].task != task {
		t.Fatalf("%s execution = session %q task %q, want session %q task %q", a.identity, execs[0].sessionID, execs[0].task, sessionID, task)
	}
}

// assertNeverExecuted asserts the hop stayed out of a rejected call path
// entirely: no session opened, no nested execution.
func (a *chainHopAdapter) assertNeverExecuted(t *testing.T) {
	t.Helper()
	if execs := a.executions(); len(execs) != 0 {
		t.Fatalf("%s executed %d time(s), want 0 (a rejected call must never reach the callee): %+v", a.identity, len(execs), execs)
	}
}

// assertTypedReplies asserts the hop's recorded replies in order: each entry
// must match the expected request id, typed call error (empty on success) and
// outcome (empty on a typed failure), plus the callee's derived report output
// when one is expected.
func (a *chainHopAdapter) assertTypedReplies(t *testing.T, want []matrixReply) {
	t.Helper()
	replies := a.gotResults()
	if len(replies) != len(want) {
		t.Fatalf("%s replies = %+v, want %d: %+v", a.identity, replies, len(want), want)
	}
	for i, w := range want {
		got := replies[i]
		if got.requestID != w.requestID || got.callError != w.callError || got.outcome != w.outcome {
			t.Fatalf("%s reply %d = %+v, want %+v", a.identity, i, got, w)
		}
		if w.outputs != nil && got.outputs["report"] != w.outputs["report"] {
			t.Fatalf("%s reply %d outputs[report] = %v, want %v", a.identity, i, got.outputs["report"], w.outputs["report"])
		}
	}
}

// stepEventsOf returns every event of a kind recorded under the caller step,
// in emission order. The depth/cycle scenarios issue calls from multiple
// nesting layers, all attributed under the engine's caller step, so
// first-event access is not enough to index them.
func (s *matrixEngineSink) stepEventsOf(kind string) []matrixEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []matrixEvent
	for _, ev := range s.stepEvents[matrixCallerStep] {
		if ev.kind == kind {
			out = append(out, ev)
		}
	}
	return out
}

// matrixEventDepth reads the nesting depth a recorded tool.call was
// dispatched at.
func matrixEventDepth(ev matrixEvent) int {
	n, _ := ev.payload["depth"].(int)
	return n
}

// assertSessionCloseSummaries asserts the host's session-close audit
// summaries across a multi-session chain: exactly one
// session_closed_with_pending entry per session that had policy decisions at
// close, carrying that session's decision count ("pending: N") — the
// fingerprint of how many calls the session's policy actually evaluated.
// Typed gate rejects record no permission decision and contribute nothing.
func assertSessionCloseSummaries(t *testing.T, audit *matrixAuditCollector, want map[string]int) {
	t.Helper()
	all := audit.all()
	var summaries []adapterhost.DecisionLogEntry
	for i := range all {
		if all[i].Decision == "session_closed_with_pending" {
			summaries = append(summaries, all[i])
		}
	}
	if len(summaries) != len(want) {
		t.Fatalf("session-close summary entries = %d, want %d: %+v", len(summaries), len(want), summaries)
	}
	got := map[string]int{}
	for i := range summaries {
		summary := &summaries[i]
		n, err := strconv.Atoi(strings.TrimPrefix(summary.Reason, "pending: "))
		if err != nil {
			t.Fatalf("session-close summary for %q = %q, want \"pending: N\"", summary.SessionID, summary.Reason)
		}
		got[summary.SessionID] = n
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("session-close summaries = %v, want %v", got, want)
	}
}

// assertGrantedRequests asserts the permission.granted events on the caller
// step: exactly one per (request id, matched glob) pair, in any order.
func assertGrantedRequests(t *testing.T, sink *matrixEngineSink, want [][2]string) {
	t.Helper()
	grants := sink.stepEventsOf("permission.granted")
	if len(grants) != len(want) {
		t.Fatalf("permission.granted events = %d, want %d: %+v", len(grants), len(want), grants)
	}
	for _, w := range want {
		found := false
		for _, ev := range grants {
			if matrixEventString(ev, "request_id") == w[0] && matrixEventString(ev, "pattern") == w[1] {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("permission.granted missing request %q with pattern %q: %+v", w[0], w[1], grants)
		}
	}
}

// assertSuccessfulResults asserts the tool.call_result events for the
// scenario's dispatched calls: exactly one per request id, all settled
// successfully (the typed gate rejects under test are never dispatched, so
// they emit no result event).
func assertSuccessfulResults(t *testing.T, sink *matrixEngineSink, wantRequestIDs []string) {
	t.Helper()
	results := sink.stepEventsOf("tool.call_result")
	if len(results) != len(wantRequestIDs) {
		t.Fatalf("tool.call_result events = %d, want %d: %+v", len(results), len(wantRequestIDs), results)
	}
	seen := map[string]bool{}
	for _, res := range results {
		id := matrixEventString(res, "request_id")
		if matrixEventString(res, "outcome") != "success" {
			t.Fatalf("tool.call_result for %q outcome = %v, want success", id, res.payload["outcome"])
		}
		if _, has := res.payload["call_error"]; has {
			t.Fatalf("tool.call_result for %q carries call_error %v, want none", id, res.payload["call_error"])
		}
		seen[id] = true
	}
	for _, id := range wantRequestIDs {
		if !seen[id] {
			t.Fatalf("tool.call_result missing request %q: %+v", id, results)
		}
	}
}

// assertAllowedLayers asserts the allow audit entries per "session@layer"
// key: each key must appear exactly want[key] times, every entry carrying a
// matched-glob reason.
func assertAllowedLayers(t *testing.T, allows []adapterhost.DecisionLogEntry, want map[string]int) {
	t.Helper()
	got := map[string]int{}
	for i := range allows {
		allow := &allows[i]
		if !strings.HasPrefix(allow.Reason, "matched: ") {
			t.Fatalf("allow audit entry reason = %q, want a matched-glob reason", allow.Reason)
		}
		got[allow.SessionID+"@"+strconv.Itoa(allow.Layer)]++
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("allow audit entries per session@layer = %v, want %v (entries: %+v)", got, want, allows)
	}
}

// dispatchExpectation is one expected tool.call event: the (redacted) target,
// request id and nesting depth the call was dispatched at.
type dispatchExpectation struct {
	target    string
	requestID string
	depth     int
}

// assertChainObservability asserts the CRI-163 event contract for one
// depth/cycle scenario run (ADR-0004 §8: the call rides the Execute stream as
// permission.request, the result returns on the Permissions stream):
// exactly one permission.granted per policy-evaluated call (a typed
// gate-rejected call still passes the policy gate first, ADR-0004 §7), no
// permission.denied, one tool.call per dispatched call carrying its redacted
// target, request id and nesting depth, and one successful tool.call_result
// per dispatched call. A gate-rejected call is never dispatched, so it emits
// no tool.call and no tool.call_result.
func assertChainObservability(t *testing.T, sink *matrixEngineSink, wantGrants [][2]string, wantDispatch []dispatchExpectation, wantResultIDs []string) {
	t.Helper()
	if grants := len(sink.stepEventsOf("permission.granted")); grants != len(wantGrants) {
		t.Fatalf("permission.granted events = %d, want %d (a typed-gate-rejected call still passes the policy gate)", grants, len(wantGrants))
	}
	assertNoStepEvents(t, sink, "permission.denied")
	assertGrantedRequests(t, sink, wantGrants)
	calls := sink.stepEventsOf("tool.call")
	if len(calls) != len(wantDispatch) {
		t.Fatalf("tool.call events = %d, want %d (a gate-rejected call is never dispatched, so it emits no tool.call): %+v", len(calls), len(wantDispatch), calls)
	}
	for i, w := range wantDispatch {
		if got := matrixEventString(calls[i], "target"); got != w.target || matrixEventString(calls[i], "request_id") != w.requestID || matrixEventDepth(calls[i]) != w.depth {
			t.Fatalf("tool.call %d = target %q request %q depth %d, want %q %q depth %d", i, got, matrixEventString(calls[i], "request_id"), matrixEventDepth(calls[i]), w.target, w.requestID, w.depth)
		}
	}
	assertSuccessfulResults(t, sink, wantResultIDs)
}

// assertTypedRejectAudit asserts the audit fingerprint of one typed runtime
// gate rejection (ADR-0004 §6: an audit entry is written for the enforcement
// event, as for other permission decisions): exactly one deny entry for the
// gate's code on the offending session at the layer the call was evaluated
// at, every policy-evaluated call allowed at its own nesting layer (the
// rejected call's allow is kept — the policy gate ran before the runtime
// gate), and one session-close summary per session carrying its policy
// evaluation count. The typed reject itself recorded no permission decision.
func assertTypedRejectAudit(t *testing.T, audit *matrixAuditCollector, wantCode, wantSessionID string, wantLayer int, wantRequestID string, wantAllows, wantSummaries map[string]int) {
	t.Helper()
	denies := audit.auditDecisions("deny")
	if len(denies) != 1 || denies[0].Reason != "adapter tool call rejected: "+wantCode {
		t.Fatalf("deny audit entries = %+v, want exactly one \"adapter tool call rejected: %s\"", denies, wantCode)
	}
	if denies[0].Layer != wantLayer || denies[0].SessionID != wantSessionID || denies[0].RequestID != wantRequestID {
		t.Fatalf("deny audit entry = layer %d session %q request %q, want layer %d session %q request %q", denies[0].Layer, denies[0].SessionID, denies[0].RequestID, wantLayer, wantSessionID, wantRequestID)
	}
	assertAllowedLayers(t, audit.auditDecisions("allow"), wantAllows)
	assertSessionCloseSummaries(t, audit, wantSummaries)
}

// compileDepthCycleGraph parses and compiles a depth/cycle conformance
// workflow. The compile-time schemas are keyed by adapter TYPE (the
// compiler's adapterInfo lookup key): the chain hops mirror the runtime
// handshake (task in, report/count out). Runtime scenarios use only
// allow_tools (no step-level tools grants, so no compile edges and no cycle
// warning) and must compile cleanly — errors AND warnings — keeping a noisy
// compile out of the runtime signal.
func compileDepthCycleGraph(t *testing.T, src string) *workflow.FSMGraph {
	t.Helper()
	spec, diags := workflow.Parse("depth_cycle_case.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse depth/cycle workflow: %s", diags)
	}
	calleeSchema := workflow.AdapterInfo{
		InputSchema:  map[string]workflow.ConfigField{"task": {Required: true}},
		OutputSchema: matrixCalleeOutputSchema(),
	}
	g, diags := workflow.Compile(spec, map[string]workflow.AdapterInfo{
		"caller": {InputSchema: map[string]workflow.ConfigField{}, OutputSchema: map[string]workflow.ConfigField{}},
		"hopb":   calleeSchema,
		"hopc":   calleeSchema,
		"hopd":   calleeSchema,
	})
	if diags.HasErrors() {
		t.Fatalf("compile depth/cycle workflow: %s", diags)
	}
	if len(diags) != 0 {
		t.Fatalf("depth/cycle workflow compiled with warnings, want clean: %s", diags)
	}
	return g
}

// runDepthCycleCase drives one depth/cycle scenario through the real engine.
func runDepthCycleCase(t *testing.T, src string, handles map[string]*chainHopAdapter) (*matrixEngineSink, *matrixAuditCollector) {
	t.Helper()
	sink := &matrixEngineSink{}
	audit := &matrixAuditCollector{}
	loaderHandles := make(map[string]adapterhost.Handle, len(handles))
	for name, hop := range handles {
		loaderHandles[name] = hop
	}
	loader := &matrixLoader{handles: loaderHandles}
	if err := engine.New(compileDepthCycleGraph(t, src), loader, sink, engine.WithAuditWriter(audit)).Run(context.Background()); err != nil {
		t.Fatalf("engine run: %v", err)
	}
	return sink, audit
}

// depthExceededWorkflowHCL renders the depth scenario workflow: a four-adapter
// call chain under policy.max_tool_depth = 2, the caller step's allow_tools
// gating the first hop, and the workflow-level permissions block gating the
// nested callee sessions' own calls. Compile-time call edges stay empty (no
// step-level tools grants), so the compile is clean and the runtime depth
// gate is the only enforcement signal.
func depthExceededWorkflowHCL() string {
	return `
workflow {
  name          = "adapter_tools_depth_exceeded"
  version       = "0.1"
  initial_state = "call"
  target_state  = "success"

  policy {
    max_tool_depth = 2
  }
}

adapter "caller" "default" {}
adapter "hopb" "default" {
  tool "helper_task" {}
}
adapter "hopc" "default" {
  tool "helper_task" {}
}
adapter "hopd" "default" {
  tool "helper_task" {}
}

step "call" {
  target = adapter.caller.default
  allow_tools = ["` + depthCycleHopBGlob + `"]

  outcome "success" { next = step.success }
}

state "success" { terminal = true }

permissions {
  allow_tools = ["` + depthCycleHopCGlob + `", "` + depthCycleHopDGlob + `"]
}
`
}

// cycleDetectedWorkflowHCL renders the cycle scenario workflow. It carries NO
// policy block, so the documented default depth (8) is exercised implicitly:
// hopb's call to hopc nests at depth 2 and completes under the default bound.
// The caller adapter declares the helper_task surface so hopb's call back to
// it passes graph validation (ADR-0004 §7) and reaches the runtime cycle gate
// (ADR-0004 §6), which is the enforcement point for the cycle.
func cycleDetectedWorkflowHCL() string {
	return `
workflow {
  name          = "adapter_tools_cycle_detected"
  version       = "0.1"
  initial_state = "call"
  target_state  = "success"
}

adapter "caller" "default" {
  tool "helper_task" {}
}
adapter "hopb" "default" {
  tool "helper_task" {}
}
adapter "hopc" "default" {
  tool "helper_task" {}
}

step "call" {
  target = adapter.caller.default
  allow_tools = ["` + depthCycleHopBGlob + `"]

  outcome "success" { next = step.success }
}

state "success" { terminal = true }

permissions {
  allow_tools = ["` + depthCycleCallerGlob + `", "` + depthCycleHopCGlob + `"]
}
`
}

// compileCycleWorkflowHCL renders the compile-only cycle scenario workflow:
// two steps granting tools to each other's adapter, closing the call graph on
// itself through step-level tools grants (the only edge source the compiler
// sees).
func compileCycleWorkflowHCL() string {
	return `
workflow {
  name          = "adapter_tools_compile_cycle"
  version       = "0.1"
  initial_state = "call"
  target_state  = "done"
}

adapter "caller" "default" {
  tool "helper_task" {}
}

adapter "hopb" "default" {
  tool "helper_task" {}
}

step "call" {
  target = adapter.caller.default
  tools  = [adapter.hopb.default.tools]
  outcome "success" { next = step.hop }
}

step "hop" {
  target = adapter.hopb.default
  tools  = [adapter.caller.default.tools]
  outcome "success" { next = state.done }
}

state "done" { terminal = true }
`
}

// RunAdapterToolsDepthCycleConformance runs the CRI-168 M6.2 depth/cycle
// conformance suite: three sub-tests locking the CRI-162 runtime behaviors
// against ADR-0004 — the typed depth failure with run continuation, the
// typed cycle failure affecting only the offending call, and the advisory
// compile-time cycle warning.
func RunAdapterToolsDepthCycleConformance(t *testing.T) {
	t.Run("depth_exceeded", depthCycleCaseDepthExceeded)
	t.Run("cycle_detected", depthCycleCaseCycleDetected)
	t.Run("compile_cycle_warning_advisory", depthCycleCaseCompileWarningAdvisory)
}

// assertDepthExceededCallBehavior asserts the call-level behavior of the
// depth scenario (ADR-0004 §6): hopc's rejected call failed typed — the
// depth_exceeded call error with no outcome (the call failed before any
// callee ran) and no permission cancel (the policy gate granted the call
// before the runtime depth gate rejected it); only that call failed (the
// caller's and hopb's calls settled with the callee's own outputs); and the
// rejected call never reached hopd, while the chain executed where it should
// — the caller step in its own session, hopb and hopc in their own nested
// sessions with the tasks their callers passed.
func assertDepthExceededCallBehavior(t *testing.T, caller, hopb, hopc, hopd *chainHopAdapter) {
	t.Helper()
	hopc.assertTypedReplies(t, []matrixReply{{
		requestID: "hopc-call-1",
		callError: "depth_exceeded",
	}})
	caller.assertTypedReplies(t, []matrixReply{{
		requestID: "call-1",
		outcome:   "success",
		outputs:   map[string]any{"report": "one"},
	}})
	hopb.assertTypedReplies(t, []matrixReply{{
		requestID: "hopb-call-1",
		outcome:   "success",
		outputs:   map[string]any{"report": "two"},
	}})
	for _, hop := range []*chainHopAdapter{caller, hopb, hopc, hopd} {
		hop.assertNoCancels(t)
	}
	hopd.assertNeverExecuted(t)
	caller.assertExecutions(t, "caller.default", "")
	hopb.assertExecutions(t, "hopb.default", "one")
	hopc.assertExecutions(t, "hopc.default", "two")
}

// assertCycleDetectedCallBehavior asserts the call-level behavior of the
// cycle scenario (ADR-0004 §6): the offending cycle call failed typed — the
// cycle_detected call error with an empty outcome and no permission cancel
// (the policy gate granted the call before the runtime cycle gate rejected
// it) — while hopb's next call in the same run settled with hopc's outputs;
// the cycle call never reached the caller adapter (rejected at dispatch —
// the callee never runs); the chain executed where it should; and the caller
// still received a clean result for its own call to hopb, so the outer
// outcome routing is unaffected.
func assertCycleDetectedCallBehavior(t *testing.T, caller, hopb, hopc *chainHopAdapter) {
	t.Helper()
	hopb.assertTypedReplies(t, []matrixReply{
		{requestID: "hopb-cycle-call", callError: "cycle_detected"},
		{requestID: "hopb-call-2", outcome: "success", outputs: map[string]any{"report": "two"}},
	})
	caller.assertTypedReplies(t, []matrixReply{{
		requestID: "call-1",
		outcome:   "success",
		outputs:   map[string]any{"report": "one"},
	}})
	for _, hop := range []*chainHopAdapter{caller, hopb, hopc} {
		hop.assertNoCancels(t)
	}
	hopc.assertExecutions(t, "hopc.default", "two")
	caller.assertExecutions(t, "caller.default", "")
	hopb.assertExecutions(t, "hopb.default", "one")
}

// depthCycleCaseDepthExceeded covers the depth bound: with
// policy.max_tool_depth = 2, the chain caller -> hopb -> hopc stays within
// the bound (depths 1 and 2), and hopc's call to hopd nests at depth 3 — one
// past the bound — so the depth gate fails that call with the typed
// depth_exceeded error (ADR-0004 §6). The test asserts the failing call is
// typed on hopc's reply, hopd never executed, and the run continued past the
// failed call: the caller's next outcome routing executed (the only FSM
// transition is the caller step -> the terminal the caller's own "success"
// outcome selected), so a failed tool call is data for the caller, not a run
// failure.
func depthCycleCaseDepthExceeded(t *testing.T) {
	caller := newChainHop("depth-cycle-caller",
		matrixCall{requestID: "call-1", target: depthCycleHopBTarget, args: map[string]any{"task": "one"}})
	hopb := newChainHop("depth-cycle-hopb",
		matrixCall{requestID: "hopb-call-1", target: depthCycleHopCTarget, args: map[string]any{"task": "two"}})
	hopc := newChainHop("depth-cycle-hopc",
		matrixCall{requestID: "hopc-call-1", target: depthCycleHopDTarget, args: map[string]any{"task": "three"}})
	hopd := newChainHop("depth-cycle-hopd")
	sink, audit := runDepthCycleCase(t, depthExceededWorkflowHCL(), map[string]*chainHopAdapter{
		"caller": caller,
		"hopb":   hopb,
		"hopc":   hopc,
		"hopd":   hopd,
	})

	// The failing call is typed; only that call failed; the rejected call
	// never reached the callee; and the chain executed where it should.
	assertDepthExceededCallBehavior(t, caller, hopb, hopc, hopd)

	// CRI-163 observability (ADR-0004 §8): all three policy-evaluated calls
	// were granted — the depth-rejected call passed the policy gate
	// (ADR-0004 §7) before the runtime depth gate rejected it — and only the
	// two dispatched calls emitted tool.call / tool.call_result.
	assertChainObservability(t, sink,
		[][2]string{
			{"call-1", depthCycleHopBGlob},
			{"hopb-call-1", depthCycleHopCGlob},
			{"hopc-call-1", depthCycleHopDGlob},
		},
		[]dispatchExpectation{
			{target: depthCycleHopBTarget, requestID: "call-1", depth: 1},
			{target: depthCycleHopCTarget, requestID: "hopb-call-1", depth: 2},
		},
		[]string{"call-1", "hopb-call-1"})

	// Audit (ADR-0004 §6): the depth rejection is a typed deny on hopc's
	// session at layer 2 — the layer the call was evaluated at; each
	// session's close summary counts the calls its policy evaluated.
	assertTypedRejectAudit(t, audit, "depth_exceeded", "hopc.default", 2, "hopc-call-1",
		map[string]int{
			"caller.default@0": 1,
			"hopb.default@1":   1,
			"hopc.default@2":   1,
		},
		map[string]int{
			"caller.default": 1,
			"hopb.default":   1,
			"hopc.default":   1,
		})

	// The run continued past the failed call: the terminal was reached
	// through the caller's own "success" outcome — the caller's next
	// outcome routing executed (the only transition is call -> success, and
	// no outcome ever fell outside its declared set).
	assertMatrixRunContinued(t, sink, "success")
}

// depthCycleCaseCycleDetected covers runtime cycle detection: hopb calls back
// to the caller adapter it was reached from (a one-hop cycle through the call
// chain), so the runtime cycle gate fails that call with the typed
// cycle_detected error (ADR-0004 §6: the compile-time check is a warning; the
// runtime gate is the enforcement point). The scenario runs without a policy
// block, so the documented default depth (8) is exercised implicitly: hopb's
// call to hopc nests at depth 2 and completes under the default bound — and
// is the proof that other calls in the same run are unaffected. The caller
// still received a clean result for its own call to hopb, so the run
// continued through the caller's own outcome routing.
func depthCycleCaseCycleDetected(t *testing.T) {
	caller := newChainHop("depth-cycle-caller",
		matrixCall{requestID: "call-1", target: depthCycleHopBTarget, args: map[string]any{"task": "one"}})
	hopb := newChainHop("depth-cycle-hopb",
		matrixCall{requestID: "hopb-cycle-call", target: depthCycleCallerTarget, args: map[string]any{"task": "back"}},
		matrixCall{requestID: "hopb-call-2", target: depthCycleHopCTarget, args: map[string]any{"task": "two"}})
	hopc := newChainHop("depth-cycle-hopc")
	sink, audit := runDepthCycleCase(t, cycleDetectedWorkflowHCL(), map[string]*chainHopAdapter{
		"caller": caller,
		"hopb":   hopb,
		"hopc":   hopc,
	})

	// The offending call failed typed while hopb's next call in the same
	// run settled; the chain executed where it should; and the caller still
	// received a clean result for its own call.
	assertCycleDetectedCallBehavior(t, caller, hopb, hopc)

	// Observability (ADR-0004 §8): the cycle-rejected call passed the policy
	// gate (granted) before the runtime cycle gate rejected it; only the two
	// dispatched calls emitted tool.call / tool.call_result, and both
	// settled successfully (the cycle call never dispatched).
	assertChainObservability(t, sink,
		[][2]string{
			{"call-1", depthCycleHopBGlob},
			{"hopb-cycle-call", depthCycleCallerGlob},
			{"hopb-call-2", depthCycleHopCGlob},
		},
		[]dispatchExpectation{
			{target: depthCycleHopBTarget, requestID: "call-1", depth: 1},
			{target: depthCycleHopCTarget, requestID: "hopb-call-2", depth: 2},
		},
		[]string{"call-1", "hopb-call-2"})

	// Audit (ADR-0004 §6): the enforcement is a typed deny on hopb's session
	// at its own nesting layer (1); the cycle call's policy allow is kept —
	// the policy gate ran before the runtime gate — alongside the caller's
	// allow (layer 0) and hopb's unaffected call's allow (layer 1).
	assertTypedRejectAudit(t, audit, "cycle_detected", "hopb.default", 1, "hopb-cycle-call",
		map[string]int{
			"caller.default@0": 1,
			"hopb.default@1":   2,
		},
		map[string]int{
			"caller.default": 1,
			"hopb.default":   2,
		})

	// The run continued: the caller's next outcome routing executed.
	assertMatrixRunContinued(t, sink, "success")
}

// depthCycleCaseCompileWarningAdvisory covers the compile-time cycle check
// (ADR-0004 §6): a workflow whose adapter call graph closes on itself — two
// steps granting tools to each other's adapter — compiles SUCCESSFULLY with
// exactly one advisory warning; no compile error is raised. The warning
// carries the call path and names the runtime cap it defers to
// (policy.max_tool_depth, currently 8), and the runtime gate exercised by the
// cycle_detected scenario above is the enforcement point.
func depthCycleCaseCompileWarningAdvisory(t *testing.T) {
	spec, diags := workflow.Parse("depth_cycle_compile.hcl", []byte(compileCycleWorkflowHCL()))
	if diags.HasErrors() {
		t.Fatalf("parse compile-cycle workflow: %s", diags)
	}
	g, diags := workflow.Compile(spec, map[string]workflow.AdapterInfo{
		"caller": {InputSchema: map[string]workflow.ConfigField{}, Capabilities: []string{"adapter_tools"}},
		"hopb":   {InputSchema: map[string]workflow.ConfigField{}, Capabilities: []string{"adapter_tools"}},
	})

	// The cycle is a warning, not an error (ADR-0004 §6): the graph is
	// produced despite the cycle.
	if diags.HasErrors() {
		t.Fatalf("a call cycle must not be a compile error (ADR-0004 §6): %s", diags)
	}
	if g == nil {
		t.Fatal("expected a graph despite the cycle warning")
	}
	if len(diags) != 1 {
		t.Fatalf("diagnostics = %d, want exactly the one advisory cycle warning: %s", len(diags), diags)
	}
	d := diags[0]
	if d.Severity != hcl.DiagWarning {
		t.Fatalf("severity = %v, want warning (the cycle check is advisory)", d.Severity)
	}
	wantSummary := "adapter tool-call cycle detected: caller.default -> hopb.default -> caller.default"
	if d.Summary != wantSummary {
		t.Fatalf("summary = %q, want %q", d.Summary, wantSummary)
	}
	for _, want := range []string{"policy.max_tool_depth", "currently 8", "allowed"} {
		if !strings.Contains(d.Detail, want) {
			t.Fatalf("detail %q missing %q (the advisory must name the runtime cap it defers to)", d.Detail, want)
		}
	}

	// The grants produced the call edges that close the cycle: the same
	// adapter refs the runtime nesting chain seeds and tracks.
	wantEdges := []workflow.AdapterCallEdge{
		{CallerAdapterRef: "caller.default", CalleeAdapterRef: "hopb.default", Tool: "", StepName: "call"},
		{CallerAdapterRef: "hopb.default", CalleeAdapterRef: "caller.default", Tool: "", StepName: "hop"},
	}
	if !reflect.DeepEqual(g.AdapterCallEdges, wantEdges) {
		t.Fatalf("AdapterCallEdges = %+v, want %+v", g.AdapterCallEdges, wantEdges)
	}
}
