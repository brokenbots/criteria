// Adapter-tools failure matrix (CRI-167): automated conformance coverage for
// the five failure scenarios of the adapter-tools call path (ADR-0004 §4-§8,
// §10), exercised end-to-end through the real engine against in-memory
// caller/callee fakes.
//
// Each case asserts BOTH the caller-visible result (the typed reply on the
// caller's Permissions stream, or the permission deny) AND the
// run-continuation invariant: the run reaches the terminal state the CALLER's
// own outcome routing selected, the callee never enters the FSM, and the
// host's own outcome routing is never used as the caller's mapping.
//
// Matrix cases:
//
//  1. deny — the caller's allow_tools excludes the target tool: the
//     callee-side permission gate cancels (PermissionEvent.cancel with the
//     policy reason reaches the caller), the callee is never executed, and
//     the run continues through the caller's own outcome routing.
//  2. unknown_adapter — the target names an adapter the workflow does not
//     declare: a typed call_error "unknown_adapter" reply reaches the caller
//     and the run continues.
//  3. unknown_tool — the target names a tool the declared static callee
//     surface does not declare: a typed call_error "unknown_tool" reply
//     reaches the caller and the run continues.
//  4. callee_crash — the callee Execute fails: the crash surfaces to the
//     caller as a typed call_error "callee_crash" (the callee's own
//     on_crash semantics), the callee session survives (a follow-up call
//     succeeds — no stream wedge), the callee's own failure outcome never
//     becomes the run's routing, and the run continues.
//  5. callee_timeout — the caller step's timeout fires while the nested
//     callee is blocked: a typed call_error "callee_timeout" reply reaches
//     the caller, the nested context is cancelled cleanly (the callee
//     observes context.DeadlineExceeded), no stream wedge, and the run
//     continues.
//  6. capability_missing — the caller adapter never declared the
//     adapter_tools capability: gate 1 rejects the call with a typed
//     call_error "capability_missing" BEFORE any policy evaluation (the
//     CRI-159 gate: no permission.granted/denied event, no policy call in
//     the audit, single deterministic reject entry), and the run continues.
//
// This suite is host-side and unconditional: no in-tree conformance adapter
// advertises adapter_tools today, so a per-adapter matrix suite
// (matrix.yaml, which only gates suites on adapter capabilities) would skip
// in CI forever. The matrix therefore runs as its own always-on conformance
// entry point, mirroring the engine's adapter_tool_call_loop_test.go
// template.
package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zclconf/go-cty/cty"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/internal/engine"
	"github.com/brokenbots/criteria/workflow"
)

const (
	// matrixCallerStep is the caller step name every matrix case uses.
	matrixCallerStep = "call"

	// matrixAwaitReplyTimeout bounds how long a matrix caller waits for a
	// typed reply before failing the case. Generous: every failure in the
	// matrix is delivered by the host long before this fires.
	matrixAwaitReplyTimeout = 10 * time.Second
)

// matrixCall is one scripted adapter-tools call the matrix caller issues.
type matrixCall struct {
	requestID string
	target    string
	args      map[string]any
}

// matrixReply records one caller-visible reply: a typed tool_call_result
// (outcome/call_error set, reason empty) or a permission cancel (reason set,
// outcome/call_error empty). outputs carries the callee's decoded outputs on
// success.
type matrixReply struct {
	requestID string
	outcome   string
	callError string
	reason    string
	outputs   map[string]any
}

// matrixCallerAdapter is the scripted caller fake. StartPermissionStream
// captures the session Permissions stream; Execute issues the script
// sequentially over the adapter-tools call path and awaits the matching
// reply for each call (tool_call_result or cancel, correlated by request id,
// skipping interleaved permission events), then returns its configured
// outcome. A cancel terminates the script: the caller observed the deny and
// routes it through its own outcome.
//
// The reply read loop deliberately does not select on the step context: a
// caller awaiting an in-flight call reads the host's typed failure reply
// (callee_timeout, callee_crash, ...) rather than abandoning the call, which
// is exactly the caller behavior the matrix asserts.
type matrixCallerAdapter struct {
	capabilities []string
	script       []matrixCall
	outcome      string
	// beforeCall (CRI-169) is invoked with the script index before the call
	// is issued; a conformance case holds the caller's Execute open at a
	// chosen point so it can pause the run mid-call. Nil-safe.
	beforeCall func(index int)

	mu       sync.Mutex
	requests <-chan *v2.PermissionEvent
	results  []matrixReply
	cancels  []matrixReply
}

func newMatrixCaller(capabilities []string, outcome string, script ...matrixCall) *matrixCallerAdapter {
	return &matrixCallerAdapter{capabilities: capabilities, outcome: outcome, script: script}
}

func (a *matrixCallerAdapter) Info(context.Context) (adapterhost.Info, error) {
	return adapterhost.Info{
		Name:         "matrix-caller",
		Version:      "0.0.0-matrix",
		Capabilities: append([]string(nil), a.capabilities...),
	}, nil
}

func (a *matrixCallerAdapter) OpenSession(context.Context, string, map[string]string, map[string]string) error {
	return nil
}

func (a *matrixCallerAdapter) StartPermissionStream(_ context.Context, _ string, requests <-chan *v2.PermissionEvent) (func(), error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.requests = requests
	return func() {}, nil
}

func (a *matrixCallerAdapter) Execute(_ context.Context, _ string, _ *workflow.StepNode, sink adapter.EventSink) (adapter.Result, error) {
	for i, call := range a.script {
		if a.beforeCall != nil {
			a.beforeCall(i)
		}
		sink.Adapter("permission.request", map[string]any{
			"request_id": call.requestID,
			"target":     call.target,
			"args":       call.args,
		})
		if !a.awaitReply(call) {
			break
		}
	}
	a.mu.Lock()
	outcome := a.outcome
	a.mu.Unlock()
	return adapter.Result{Outcome: outcome}, nil
}

// matrixTypedReply converts a typed tool_call_result stream event into the
// reply the caller records; settled=false for other stream traffic.
func matrixTypedReply(ev *v2.PermissionEvent) (matrixReply, bool) {
	tcr := ev.GetToolCallResult()
	if tcr == nil {
		return matrixReply{}, false
	}
	reply := matrixReply{requestID: tcr.GetRequestId(), outcome: tcr.GetOutcome(), callError: tcr.GetCallError()}
	if len(tcr.GetOutputsJson()) > 0 {
		var outputs map[string]any
		err := json.Unmarshal(tcr.GetOutputsJson(), &outputs)
		reply.outputs = outputs
		if err != nil {
			reply.callError = "matrix-harness-error: decoding outputs: " + err.Error()
			reply.outcome = ""
		}
	}
	return reply, true
}

// awaitReply blocks until the host settles the call: a typed
// tool_call_result is recorded and reported as delivered; a cancel is
// recorded and reported as a denial (which terminates the script).
func (a *matrixCallerAdapter) awaitReply(call matrixCall) bool {
	a.mu.Lock()
	requests := a.requests
	a.mu.Unlock()
	if requests == nil {
		a.recordResult(matrixReply{requestID: call.requestID, callError: "matrix-harness-error: permission stream not started"})
		return false
	}
	deadline := time.After(matrixAwaitReplyTimeout)
	for {
		select {
		case ev, ok := <-requests:
			if !ok {
				a.recordResult(matrixReply{requestID: call.requestID, callError: "matrix-harness-error: permission stream closed"})
				return false
			}
			if reply, settled := matrixTypedReply(ev); settled {
				a.recordResult(reply)
				return true
			}
			if cancel := ev.GetCancel(); cancel != nil {
				a.mu.Lock()
				a.cancels = append(a.cancels, matrixReply{requestID: cancel.GetRequestId(), reason: cancel.GetReason()})
				a.mu.Unlock()
				return false
			}
			// Plain permission requests and other stream traffic are not part
			// of the matrix call path; skip them.
		case <-deadline:
			a.recordResult(matrixReply{requestID: call.requestID, callError: "matrix-harness-error: timed out waiting for reply"})
			return false
		}
	}
}

func (a *matrixCallerAdapter) recordResult(reply matrixReply) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.results = append(a.results, reply)
}

func (a *matrixCallerAdapter) gotResults() []matrixReply {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]matrixReply(nil), a.results...)
}

func (a *matrixCallerAdapter) gotCancels() []matrixReply {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]matrixReply(nil), a.cancels...)
}

func (a *matrixCallerAdapter) CloseSession(context.Context, string) error { return nil }
func (a *matrixCallerAdapter) Kill()                                      {}
func (a *matrixCallerAdapter) Pause(context.Context, string) error        { return nil }
func (a *matrixCallerAdapter) Resume(context.Context, string) error       { return nil }
func (a *matrixCallerAdapter) Inspect(context.Context, string) (*v2.InspectResponse, error) {
	return &v2.InspectResponse{}, nil
}
func (a *matrixCallerAdapter) Snapshot(context.Context, string) (*v2.SnapshotResponse, error) {
	return &v2.SnapshotResponse{}, nil
}
func (a *matrixCallerAdapter) Restore(context.Context, string, []byte, uint32) error { return nil }

// matrixCalleeExecution records one nested callee Execute: the session it ran
// in, the synthetic step it was handed, the task argument it observed, and —
// for a blocked call — the context error it was cancelled with.
type matrixCalleeExecution struct {
	sessionID string
	stepName  string
	task      string
	ctxErr    error
}

// matrixCalleeAdapter is the callee fake. Its runtime handshake declares the
// typed schema surface the host uses for the synthetic nested step; tasks
// script four behaviors: "block" waits for the nested context cancellation
// (the timeout case), "hold" waits for the case's release channel or the
// nested context cancellation (the CRI-169 pause cases), "explode" fails the
// Execute with a plain error (the crash case), everything else succeeds with
// report/count outputs.
type matrixCalleeAdapter struct {
	mu    sync.Mutex
	sess  map[string]struct{}
	execs []matrixCalleeExecution
	// holdRelease unblocks the scripted "hold" task (CRI-169); a nil
	// channel makes "hold" wait for the nested context cancellation only.
	holdRelease chan struct{}
}

func newMatrixCallee() *matrixCalleeAdapter {
	return &matrixCalleeAdapter{sess: map[string]struct{}{}}
}

func (a *matrixCalleeAdapter) Info(context.Context) (adapterhost.Info, error) {
	return adapterhost.Info{
		Name:         "matrix-callee",
		Version:      "0.0.0-matrix",
		Capabilities: []string{"execute"},
		AdapterInfo: workflow.AdapterInfo{
			InputSchema:  map[string]workflow.ConfigField{"task": {Required: true}},
			OutputSchema: matrixCalleeOutputSchema(),
		},
	}, nil
}

func matrixCalleeOutputSchema() map[string]workflow.ConfigField {
	return map[string]workflow.ConfigField{
		"report": {CtyType: cty.String},
		"count":  {CtyType: cty.Number},
	}
}

func (a *matrixCalleeAdapter) OpenSession(_ context.Context, id string, _, _ map[string]string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sess[id] = struct{}{}
	return nil
}

func (a *matrixCalleeAdapter) Execute(ctx context.Context, sessionID string, step *workflow.StepNode, _ adapter.EventSink) (adapter.Result, error) {
	task := step.Input["task"]
	// Record at Execute entry so in-flight executions are observable (the
	// CRI-169 pause cases poll for them); a canceled task updates its
	// recorded context error on completion.
	exec := matrixCalleeExecution{sessionID: sessionID, stepName: step.Name, task: task}
	a.mu.Lock()
	idx := len(a.execs)
	a.execs = append(a.execs, exec)
	a.mu.Unlock()

	var ctxErr error
	outcome := "success"
	switch task {
	case "block":
		<-ctx.Done()
		ctxErr = ctx.Err()
		outcome = "failure"
	case "hold":
		// CRI-169: hold until the case releases it (drain case) or the
		// pause drain cancels the nested context (straggler case).
		select {
		case <-a.holdRelease:
		case <-ctx.Done():
			ctxErr = ctx.Err()
			outcome = "failure"
		}
	case "slow":
		// Delay long enough for a faster sibling call's reply to overtake
		// this one on the permissions stream (the CRI-161 interleave
		// precedent; the agent-caller matrix's concurrent-calls case).
		time.Sleep(150 * time.Millisecond)
	case "explode":
		outcome = "failure"
	}
	if ctxErr != nil {
		a.mu.Lock()
		a.execs[idx].ctxErr = ctxErr
		a.mu.Unlock()
	}
	if ctxErr != nil {
		return adapter.Result{Outcome: outcome}, ctxErr
	}
	if outcome == "failure" {
		return adapter.Result{Outcome: outcome}, errors.New("callee exploded")
	}
	return adapter.Result{
		Outcome: outcome,
		Outputs: map[string]cty.Value{
			"report": cty.StringVal(task),
			"count":  cty.NumberIntVal(int64(len(task))),
		},
	}, nil
}

func (a *matrixCalleeAdapter) executions() []matrixCalleeExecution {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]matrixCalleeExecution(nil), a.execs...)
}

func (a *matrixCalleeAdapter) CloseSession(context.Context, string) error { return nil }
func (a *matrixCalleeAdapter) Kill()                                      {}
func (a *matrixCalleeAdapter) Pause(context.Context, string) error        { return nil }
func (a *matrixCalleeAdapter) Resume(context.Context, string) error       { return nil }
func (a *matrixCalleeAdapter) Inspect(context.Context, string) (*v2.InspectResponse, error) {
	return &v2.InspectResponse{}, nil
}
func (a *matrixCalleeAdapter) Snapshot(context.Context, string) (*v2.SnapshotResponse, error) {
	return &v2.SnapshotResponse{}, nil
}
func (a *matrixCalleeAdapter) Restore(context.Context, string, []byte, uint32) error { return nil }

// matrixLoader resolves the matrix fakes by adapter type.
type matrixLoader struct {
	handles map[string]adapterhost.Handle
}

func (l *matrixLoader) Resolve(_ context.Context, name string) (adapterhost.Handle, error) {
	h, ok := l.handles[name]
	if !ok {
		return nil, errors.New("no matrix handle for adapter " + name)
	}
	return h, nil
}

func (l *matrixLoader) Shutdown(context.Context) error { return nil }

// matrixEvent is one recorded adapter event on a step's event sink.
type matrixEvent struct {
	kind    string
	payload map[string]any
}

// matrixEngineSink is a full engine.Sink recording the run-level facts the
// matrix asserts: which steps ran, which transitions routed the run, the
// terminal state, and — critically for the continuation invariant — that no
// step outcome ever fell outside its declared set (unknown or defaulted),
// which would mean something other than the caller's own outcome routing
// decided the run's path.
type matrixEngineSink struct {
	mu         sync.Mutex
	entered    []string
	transits   []string
	terminal   string
	ok         bool
	failure    string
	unknowns   []string
	outcomes   []string
	stepEvents map[string][]matrixEvent
}

func (s *matrixEngineSink) OnRunStarted(string, string) {}
func (s *matrixEngineSink) OnRunCompleted(finalState string, success bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.terminal = finalState
	s.ok = success
}
func (s *matrixEngineSink) OnRunFailed(reason, step string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failure = reason + " (step " + step + ")"
}
func (s *matrixEngineSink) OnStepEntered(step, _ string, _ int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entered = append(s.entered, step)
}
func (s *matrixEngineSink) OnStepOutcome(step, outcome string, _ time.Duration, _ error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.outcomes = append(s.outcomes, step+"="+outcome)
}
func (s *matrixEngineSink) OnStepTransition(from, to, _ string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.transits = append(s.transits, from+"->"+to)
}
func (s *matrixEngineSink) OnStepResumed(string, int, string)                            {}
func (s *matrixEngineSink) OnVariableSet(string, string, string)                         {}
func (s *matrixEngineSink) OnStepOutputCaptured(string, map[string]string)               {}
func (s *matrixEngineSink) OnRunPaused(string, string, string)                           {}
func (s *matrixEngineSink) OnWaitEntered(string, string, string, string)                 {}
func (s *matrixEngineSink) OnWaitResumed(string, string, string, map[string]string)      {}
func (s *matrixEngineSink) OnApprovalRequested(string, []string, string)                 {}
func (s *matrixEngineSink) OnApprovalDecision(string, string, string, map[string]string) {}
func (s *matrixEngineSink) OnBranchEvaluated(string, string, string, string)             {}
func (s *matrixEngineSink) OnForEachEntered(string, int)                                 {}
func (s *matrixEngineSink) OnStepIterationStarted(string, int, string, bool)             {}
func (s *matrixEngineSink) OnStepIterationCompleted(string, string, string)              {}
func (s *matrixEngineSink) OnStepIterationItem(string, int, string)                      {}
func (s *matrixEngineSink) OnScopeIterCursorSet(string)                                  {}
func (s *matrixEngineSink) OnAdapterLifecycle(string, string, string, string)            {}
func (s *matrixEngineSink) OnAdapterLifecycleEvent(*engine.AdapterLifecycleEvent)        {}
func (s *matrixEngineSink) OnRunOutputs([]map[string]string)                             {}
func (s *matrixEngineSink) OnStepOutcomeDefaulted(step, original, _ string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unknowns = append(s.unknowns, "defaulted: "+step+"="+original)
}
func (s *matrixEngineSink) OnStepOutcomeUnknown(step, outcome string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unknowns = append(s.unknowns, "unknown: "+step+"="+outcome)
}

func (s *matrixEngineSink) StepEventSink(step string) adapter.EventSink {
	return &matrixEventRecorder{sink: s, step: step}
}

func (s *matrixEngineSink) recordEvent(step, kind string, data any) {
	payload, _ := data.(map[string]any)
	cp := make(map[string]any, len(payload))
	for k, v := range payload {
		cp[k] = v
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stepEvents == nil {
		s.stepEvents = map[string][]matrixEvent{}
	}
	s.stepEvents[step] = append(s.stepEvents[step], matrixEvent{kind: kind, payload: cp})
}

func (s *matrixEngineSink) stepsRun() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.entered...)
}

func (s *matrixEngineSink) transitions() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.transits...)
}

func (s *matrixEngineSink) terminalState() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.terminal
}

func (s *matrixEngineSink) runOK() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ok
}

func (s *matrixEngineSink) runFailure() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failure
}

func (s *matrixEngineSink) unknownOutcomes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.unknowns...)
}

func (s *matrixEngineSink) stepOutcomes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.outcomes...)
}

func (s *matrixEngineSink) stepEventCount(kind string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, ev := range s.stepEvents[matrixCallerStep] {
		if ev.kind == kind {
			n++
		}
	}
	return n
}

func (s *matrixEngineSink) firstStepEvent(kind string) (matrixEvent, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ev := range s.stepEvents[matrixCallerStep] {
		if ev.kind == kind {
			return ev, true
		}
	}
	return matrixEvent{}, false
}

// matrixEventRecorder records adapter events attributed under a step.
type matrixEventRecorder struct {
	sink *matrixEngineSink
	step string
}

func (r *matrixEventRecorder) Log(string, []byte) {}
func (r *matrixEventRecorder) Adapter(kind string, data any) {
	r.sink.recordEvent(r.step, kind, data)
}

// matrixAuditCollector collects host decision-log entries.
type matrixAuditCollector struct {
	mu      sync.Mutex
	entries []adapterhost.DecisionLogEntry
}

func (c *matrixAuditCollector) Write(entry *adapterhost.DecisionLogEntry) {
	if entry == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = append(c.entries, *entry)
}

func (c *matrixAuditCollector) all() []adapterhost.DecisionLogEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]adapterhost.DecisionLogEntry(nil), c.entries...)
}

// assertSessionCloseSummary asserts the host's session-close audit summary:
// every permission decision a session recorded stays tracked on its
// permission state until close (recordDecision), so a session with N policy
// decisions closes with exactly one "session_closed_with_pending" summary
// entry. Typed gate rejects (rejectToolCall) record no permission decision
// and contribute nothing to the summary — so its count is the fingerprint of
// how many calls the policy actually evaluated.
func assertSessionCloseSummary(t *testing.T, audit *matrixAuditCollector, wantPending int) {
	t.Helper()
	all := audit.all()
	var summaries []adapterhost.DecisionLogEntry
	for i := range all {
		if all[i].Decision == "session_closed_with_pending" {
			summaries = append(summaries, all[i])
		}
	}
	if len(summaries) != 1 {
		t.Fatalf("session-close summary entries = %d, want 1: %+v", len(summaries), summaries)
	}
	if want := fmt.Sprintf("pending: %d", wantPending); summaries[0].Reason != want {
		t.Fatalf("session-close summary = %q, want %q", summaries[0].Reason, want)
	}
}

// auditDecisions filters entries by decision ("allow" | "deny" | ...).
func (c *matrixAuditCollector) auditDecisions(decision string) []adapterhost.DecisionLogEntry {
	all := c.all()
	var out []adapterhost.DecisionLogEntry
	for i := range all {
		if all[i].Decision == decision {
			out = append(out, all[i])
		}
	}
	return out
}

// compileMatrixGraph parses and compiles a matrix workflow. The compile-time
// schemas are keyed by adapter TYPE (the compiler's adapterInfo lookup key):
// the caller declares the adapter_tools capability so tools grants compile
// without the pointless-caller warning, the callee declares the input/output
// schemas its runtime handshake mirrors. Every case compiles cleanly
// (errors AND warnings), keeping a noisy compile out of the matrix signal.
func compileMatrixGraph(t *testing.T, src string) *workflow.FSMGraph {
	t.Helper()
	spec, diags := workflow.Parse("matrix_case.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse matrix workflow: %s", diags)
	}
	g, diags := workflow.Compile(spec, map[string]workflow.AdapterInfo{
		"caller": {
			InputSchema:  map[string]workflow.ConfigField{},
			OutputSchema: map[string]workflow.ConfigField{},
			Capabilities: []string{"adapter_tools"},
		},
		"callee": {
			InputSchema:  map[string]workflow.ConfigField{"task": {Required: true}},
			OutputSchema: matrixCalleeOutputSchema(),
		},
	})
	if diags.HasErrors() {
		t.Fatalf("compile matrix workflow: %s", diags)
	}
	if len(diags) != 0 {
		t.Fatalf("matrix workflow compiled with warnings, want clean: %s", diags)
	}
	return g
}

// runMatrixCase drives one matrix case through the real engine. The caller
// is accepted as a bare adapterhost.Handle so the agent-caller matrix
// (CRI-183) can ride the same wiring with its own caller fake.
func runMatrixCase(t *testing.T, src string, caller adapterhost.Handle, callee *matrixCalleeAdapter) (*matrixEngineSink, *matrixAuditCollector) {
	t.Helper()
	sink := &matrixEngineSink{}
	audit := &matrixAuditCollector{}
	loader := &matrixLoader{handles: map[string]adapterhost.Handle{
		"caller": caller,
		"callee": callee,
	}}
	if err := engine.New(compileMatrixGraph(t, src), loader, sink, engine.WithAuditWriter(audit)).Run(context.Background()); err != nil {
		t.Fatalf("engine run: %v", err)
	}
	return sink, audit
}

// assertMatrixRunContinued asserts the run-continuation invariant shared by
// every matrix case: the engine completed on the terminal state the CALLER's
// own outcome routing selected (OnRunCompleted success on that terminal, the
// only transition being callerStep -> that terminal), only the caller step
// ever entered the FSM, and no step outcome fell outside its declared set
// (no unknown/defaulted outcome — the caller's own mapping was always the
// deciding routing).
func assertMatrixRunContinued(t *testing.T, sink *matrixEngineSink, wantTerminal string) {
	t.Helper()
	if got, want := sink.terminalState(), wantTerminal; got != want {
		t.Fatalf("terminal state = %q, want %q", got, want)
	}
	if !sink.runOK() {
		t.Fatalf("run did not complete successfully: failure=%q", sink.runFailure())
	}
	if got, want := sink.stepsRun(), []string{matrixCallerStep}; !reflect.DeepEqual(got, want) {
		t.Fatalf("steps run = %v, want %v", got, want)
	}
	if got, want := sink.transitions(), []string{matrixCallerStep + "->" + wantTerminal}; !reflect.DeepEqual(got, want) {
		t.Fatalf("transitions = %v, want %v (the run must continue through the caller's own outcome routing)", got, want)
	}
	if got, want := sink.stepOutcomes(), []string{matrixCallerStep + "=" + wantTerminal}; !reflect.DeepEqual(got, want) {
		t.Fatalf("step outcomes = %v, want %v", got, want)
	}
	if unknown := sink.unknownOutcomes(); len(unknown) != 0 {
		t.Fatalf("unexpected unknown/defaulted outcomes %v (the caller's declared outcome routing must decide the run)", unknown)
	}
}

// assertCalleeNeverExecuted asserts the callee stayed out of a rejected call
// path entirely: no session opened, no nested execution.
func assertCalleeNeverExecuted(t *testing.T, callee *matrixCalleeAdapter) {
	t.Helper()
	if execs := callee.executions(); len(execs) != 0 {
		t.Fatalf("callee executed %d time(s), want 0 (a rejected call must never reach the callee): %+v", len(execs), execs)
	}
}

// assertNoStepEvents asserts no event of a kind was emitted under the
// caller's step.
func assertNoStepEvents(t *testing.T, sink *matrixEngineSink, kind string) {
	t.Helper()
	if n := sink.stepEventCount(kind); n != 0 {
		t.Fatalf("caller step emitted %d %q event(s), want 0", n, kind)
	}
}

// matrixEventString reads a string field from a recorded event payload.
func matrixEventString(ev matrixEvent, key string) string {
	v, _ := ev.payload[key].(string)
	return v
}

// matrixGraph renders the shared HCL shape for a matrix case: a caller step
// issuing one adapter-tools call (scripted by the caller fake), the callee
// adapter declaring one static tool, and the caller's own outcome routing to
// the case terminal.
func matrixGraph(name, targetState, callerStepBody string) string {
	return "workflow {\n" +
		"  name          = \"" + name + "\"\n" +
		"  version       = \"0.1\"\n" +
		"  initial_state = \"call\"\n" +
		"  target_state  = \"" + targetState + "\"\n" +
		"}\n" +
		"\n" +
		"adapter \"callee\" \"default\" {\n" +
		"  tool \"helper_task\" {}\n" +
		"}\n" +
		"\n" +
		"adapter \"caller\" \"default\" {}\n" +
		"\n" +
		"step \"call\" {\n" +
		"  target = adapter.caller.default\n" +
		callerStepBody +
		"}\n" +
		"\n" +
		"state \"" + targetState + "\" {\n" +
		"  terminal = true\n" +
		"}\n"
}

// matrixToolCallTarget is the canonical callee target the matrix calls.
const matrixToolCallTarget = "adapter.callee.default.tools.helper_task"

// assertTypedCallError asserts the caller-visible result of a typed gate
// failure: exactly one reply for the scripted request carrying the call
// error and an empty outcome (failure replies carry the call_error, not an
// outcome), and no permission cancel (a typed failure is not a deny).
func assertTypedCallError(t *testing.T, caller *matrixCallerAdapter, wantCallError string) {
	t.Helper()
	results := caller.gotResults()
	if len(results) != 1 {
		t.Fatalf("results received = %d, want 1: %+v", len(results), results)
	}
	if results[0].requestID != "call-1" || results[0].callError != wantCallError || results[0].outcome != "" {
		t.Fatalf("typed reply = %+v, want request \"call-1\" call_error %q outcome empty", results[0], wantCallError)
	}
	if cancels := caller.gotCancels(); len(cancels) != 0 {
		t.Fatalf("cancels received = %d, want 0: %+v", len(cancels), cancels)
	}
}

// assertDenyCancel asserts the caller-visible result of a denied call:
// exactly one cancel for the scripted request carrying the policy reason,
// and no typed reply (a deny is not a tool_call_result).
func assertDenyCancel(t *testing.T, caller *matrixCallerAdapter, requestID, wantReason string) {
	t.Helper()
	cancels := caller.gotCancels()
	if len(cancels) != 1 {
		t.Fatalf("cancels received = %d, want 1: %+v", len(cancels), cancels)
	}
	if cancels[0].requestID != requestID {
		t.Fatalf("cancel request id = %q, want %q", cancels[0].requestID, requestID)
	}
	if cancels[0].reason != wantReason {
		t.Fatalf("cancel reason = %q, want %q", cancels[0].reason, wantReason)
	}
	if results := caller.gotResults(); len(results) != 0 {
		t.Fatalf("typed results received = %d, want 0 (a deny is not a tool_call_result): %+v", len(results), results)
	}
}

// assertCallerStepEvent asserts one event of a kind was emitted under the
// caller step carrying the expected string fields, and returns it.
func assertCallerStepEvent(t *testing.T, sink *matrixEngineSink, kind string, fields map[string]string) matrixEvent {
	t.Helper()
	ev, ok := sink.firstStepEvent(kind)
	if !ok {
		t.Fatalf("%q event not emitted under the caller step", kind)
	}
	for key, want := range fields {
		if got := matrixEventString(ev, key); got != want {
			t.Fatalf("%q event field %s = %q, want %q", kind, key, got, want)
		}
	}
	return ev
}

// assertCrashReplies asserts the caller-visible crash sequence: the crash
// typed as callee_crash with an empty outcome, then the follow-up call
// settled cleanly with the callee's outputs — proving no stream wedge.
func assertCrashReplies(t *testing.T, caller *matrixCallerAdapter) {
	t.Helper()
	results := caller.gotResults()
	if len(results) != 2 {
		t.Fatalf("results received = %d, want 2 (crash reply + follow-up success): %+v", len(results), results)
	}
	if results[0].requestID != "call-1" || results[0].callError != "callee_crash" || results[0].outcome != "" {
		t.Fatalf("first typed reply = %+v, want request \"call-1\" call_error \"callee_crash\" outcome empty", results[0])
	}
	if results[1].requestID != "call-2" || results[1].callError != "" || results[1].outcome != "success" {
		t.Fatalf("second typed reply = %+v, want request \"call-2\" outcome \"success\" (no stream wedge)", results[1])
	}
	if got, want := results[1].outputs["report"], "fast"; got != want {
		t.Fatalf("follow-up outputs[report] = %v, want %q", results[1].outputs["report"], want)
	}
	if cancels := caller.gotCancels(); len(cancels) != 0 {
		t.Fatalf("cancels received = %d, want 0: %+v", len(cancels), cancels)
	}
}

// assertCalleeCrashObservability asserts the CRI-163 observability events for
// the crash: both calls policy-allowed, no deny, and the call/result events
// carrying the call and its typed failure with no outcome on the failure.
func assertCalleeCrashObservability(t *testing.T, sink *matrixEngineSink) {
	t.Helper()
	if grants := sink.stepEventCount("permission.granted"); grants != 2 {
		t.Fatalf("permission.granted events = %d, want 2 (both calls policy-allowed)", grants)
	}
	assertNoStepEvents(t, sink, "permission.denied")
	assertCallerStepEvent(t, sink, "tool.call", map[string]string{
		"target":     matrixToolCallTarget,
		"tool":       "helper_task",
		"request_id": "call-1",
	})
	result := assertCallerStepEvent(t, sink, "tool.call_result", map[string]string{"call_error": "callee_crash"})
	if _, has := result.payload["outcome"]; has {
		t.Fatalf("tool.call_result carries outcome %v, want none on a failed call", result.payload["outcome"])
	}
}

// assertCalleeCrashExecutions asserts both scripted tasks ran in the callee's
// own session (the crash did not wedge or close it) and neither observed a
// context error.
func assertCalleeCrashExecutions(t *testing.T, callee *matrixCalleeAdapter) {
	t.Helper()
	execs := callee.executions()
	if len(execs) != 2 {
		t.Fatalf("callee executions = %d, want 2 (explode + fast): %+v", len(execs), execs)
	}
	for _, exec := range execs {
		if exec.sessionID != "callee.default" {
			t.Fatalf("callee execution session = %q, want \"callee.default\"", exec.sessionID)
		}
		if exec.ctxErr != nil {
			t.Fatalf("callee execution of %q saw context error %v, want nil", exec.task, exec.ctxErr)
		}
	}
}

// assertCalleeTimeoutExecution asserts the blocked nested execution observed
// a clean context cancellation (context.DeadlineExceeded) in the callee's
// own session.
func assertCalleeTimeoutExecution(t *testing.T, callee *matrixCalleeAdapter) {
	t.Helper()
	execs := callee.executions()
	if len(execs) != 1 {
		t.Fatalf("callee executions = %d, want 1: %+v", len(execs), execs)
	}
	if execs[0].sessionID != "callee.default" || execs[0].task != "block" {
		t.Fatalf("callee execution = session %q task %q, want session \"callee.default\" task \"block\"", execs[0].sessionID, execs[0].task)
	}
	if !errors.Is(execs[0].ctxErr, context.DeadlineExceeded) {
		t.Fatalf("callee context error = %v, want context.DeadlineExceeded (the nested context must be cancelled cleanly)", execs[0].ctxErr)
	}
}

// RunAdapterToolsFailureMatrix runs the CRI-167 adapter-tools failure matrix:
// six sub-tests, one per matrix case, each asserting the caller-visible
// result and the run-continuation invariant through the real engine.
func RunAdapterToolsFailureMatrix(t *testing.T) {
	t.Run("deny", matrixCaseDeny)
	t.Run("unknown_adapter", matrixCaseUnknownAdapter)
	t.Run("unknown_tool", matrixCaseUnknownTool)
	t.Run("callee_crash", matrixCaseCalleeCrash)
	t.Run("callee_timeout", matrixCaseCalleeTimeout)
	t.Run("capability_missing", matrixCaseCapabilityMissing)
}

// matrixCaseDeny covers the denied call: the caller's allow_tools excludes
// the target, so the permission gate cancels with the policy reason, the
// callee is never executed, and the caller routes the observed deny through
// its own "denied" outcome. Returning "success" after a denial would be
// overridden to needs_review by the host, so the caller's own outcome is the
// continuation route — never a callee-side routing.
func matrixCaseDeny(t *testing.T) {
	caller := newMatrixCaller([]string{"adapter_tools", "execute"}, "denied", matrixCall{
		requestID: "call-1",
		target:    matrixToolCallTarget,
		args:      map[string]any{"task": "do-thing"},
	})
	callee := newMatrixCallee()
	src := matrixGraph("adapter_tools_matrix_deny", "denied", "\n"+
		"  allow_tools = [\"other.family.*\"]\n"+
		"\n"+
		"  outcome \"denied\" {\n"+
		"    next = step.denied\n"+
		"  }\n"+
		"\n")
	sink, audit := runMatrixCase(t, src, caller, callee)

	assertDenyCancel(t, caller, "call-1", "no matching allow_tools entry")

	// The deny is the policy's, not a callee-side artifact: the inner
	// permission.denied event carries the request id and the policy
	// reason, and no permission.granted is emitted.
	assertCallerStepEvent(t, sink, "permission.denied", map[string]string{
		"request_id": "call-1",
		"reason":     "no matching allow_tools entry",
	})
	assertNoStepEvents(t, sink, "permission.granted")
	assertNoStepEvents(t, sink, "tool.call")
	assertNoStepEvents(t, sink, "tool.call_result")

	// Audit: the caller's own policy deny at layer 0. A policy decision
	// stays tracked on the permission state until session close, so the
	// close summary accounts for it; a gate reject would record no
	// permission decision and contribute no summary.
	denies := audit.auditDecisions("deny")
	if len(denies) != 1 || denies[0].Reason != "no matching allow_tools entry" {
		t.Fatalf("deny audit entries = %+v, want exactly one policy deny \"no matching allow_tools entry\"", denies)
	}
	if denies[0].Layer != 0 || denies[0].SessionID != "caller.default" || denies[0].RequestID != "call-1" {
		t.Fatalf("deny audit entry = layer %d session %q request %q, want layer 0 session \"caller.default\" request \"call-1\"", denies[0].Layer, denies[0].SessionID, denies[0].RequestID)
	}
	if allows := audit.auditDecisions("allow"); len(allows) != 0 {
		t.Fatalf("allow audit entries = %+v, want none (the policy denied the call)", allows)
	}
	assertSessionCloseSummary(t, audit, 1)

	assertCalleeNeverExecuted(t, callee)
	assertMatrixRunContinued(t, sink, "denied")
}

// matrixCaseUnknownAdapter covers the unknown callee adapter: the target
// names an adapter the workflow does not declare. The policy grants the call
// (the runtime glob matches the target), then the graph gate fails it typed
// as unknown_adapter. The callee is never touched and the run continues.
func matrixCaseUnknownAdapter(t *testing.T) {
	caller := newMatrixCaller([]string{"adapter_tools", "execute"}, "handled", matrixCall{
		requestID: "call-1",
		target:    "adapter.ghost.default.tools.ghost_task",
		args:      map[string]any{"task": "do-thing"},
	})
	callee := newMatrixCallee()
	src := matrixGraph("adapter_tools_matrix_unknown_adapter", "handled", "\n"+
		"  allow_tools = [\"adapter.ghost.*\"]\n"+
		"\n"+
		"  outcome \"handled\" {\n"+
		"    next = step.handled\n"+
		"  }\n"+
		"\n")
	sink, audit := runMatrixCase(t, src, caller, callee)

	assertTypedCallError(t, caller, "unknown_adapter")

	// The allow came from the caller's own policy (granted event with the
	// matched pattern); the rejection adds no permission.denied.
	assertCallerStepEvent(t, sink, "permission.granted", map[string]string{
		"request_id": "call-1",
		"pattern":    "adapter.ghost.*",
	})
	assertNoStepEvents(t, sink, "permission.denied")
	assertNoStepEvents(t, sink, "tool.call")
	assertNoStepEvents(t, sink, "tool.call_result")

	// Audit: the caller's own policy allow (the runtime glob matched the
	// target) followed by the typed graph-gate reject — both at the
	// caller's layer, plus the host's session-close summary.
	denies := audit.auditDecisions("deny")
	if len(denies) != 1 || denies[0].Reason != "adapter tool call rejected: unknown_adapter" {
		t.Fatalf("deny audit entries = %+v, want exactly one \"adapter tool call rejected: unknown_adapter\"", denies)
	}
	allows := audit.auditDecisions("allow")
	if len(allows) != 1 || allows[0].Reason != "matched: adapter.ghost.*" {
		t.Fatalf("allow audit entries = %+v, want exactly one \"matched: adapter.ghost.*\"", allows)
	}
	both := append(append([]adapterhost.DecisionLogEntry(nil), allows...), denies...)
	for i := range both {
		if both[i].Layer != 0 {
			t.Fatalf("audit entry layer = %d, want 0", both[i].Layer)
		}
	}
	assertSessionCloseSummary(t, audit, 1)

	assertCalleeNeverExecuted(t, callee)
	assertMatrixRunContinued(t, sink, "handled")
}

// matrixCaseUnknownTool covers the unknown static tool: the callee declares a
// static tool surface (helper_task); the target names a tool it does not
// declare. The policy grants the call, the graph gate fails it typed as
// unknown_tool. The callee is never touched and the run continues.
func matrixCaseUnknownTool(t *testing.T) {
	caller := newMatrixCaller([]string{"adapter_tools", "execute"}, "handled", matrixCall{
		requestID: "call-1",
		target:    "adapter.callee.default.tools.other_task",
		args:      map[string]any{"task": "do-thing"},
	})
	callee := newMatrixCallee()
	src := matrixGraph("adapter_tools_matrix_unknown_tool", "handled", "\n"+
		"  allow_tools = [\"adapter.callee.default.tools.*\"]\n"+
		"\n"+
		"  outcome \"handled\" {\n"+
		"    next = step.handled\n"+
		"  }\n"+
		"\n")
	sink, audit := runMatrixCase(t, src, caller, callee)

	assertTypedCallError(t, caller, "unknown_tool")

	assertNoStepEvents(t, sink, "permission.denied")
	assertNoStepEvents(t, sink, "tool.call")
	assertNoStepEvents(t, sink, "tool.call_result")

	denies := audit.auditDecisions("deny")
	if len(denies) != 1 || denies[0].Reason != "adapter tool call rejected: unknown_tool" {
		t.Fatalf("deny audit entries = %+v, want exactly one \"adapter tool call rejected: unknown_tool\"", denies)
	}
	allows := audit.auditDecisions("allow")
	if len(allows) != 1 || !strings.Contains(allows[0].Reason, "matched: adapter.callee.default.tools.*") {
		t.Fatalf("allow audit entries = %+v, want exactly one policy allow for the target (the caller's own policy allowed before the graph gate rejected)", allows)
	}
	assertSessionCloseSummary(t, audit, 1)

	assertCalleeNeverExecuted(t, callee)
	assertMatrixRunContinued(t, sink, "handled")
}

// matrixCaseCalleeCrash covers the callee crash: the callee's Execute fails
// and the crash surfaces to the caller as a typed call_error "callee_crash"
// per the callee's own on_crash semantics (a plain error, not a
// session-crash abort). The callee's own failure outcome must not become the
// run's routing: the caller proceeds with its script — a follow-up call
// succeeds, proving no stream wedge and that the callee session survived —
// and the run continues through the caller's own outcome routing.
func matrixCaseCalleeCrash(t *testing.T) {
	caller := newMatrixCaller([]string{"adapter_tools", "execute"}, "handled",
		matrixCall{requestID: "call-1", target: matrixToolCallTarget, args: map[string]any{"task": "explode"}},
		matrixCall{requestID: "call-2", target: matrixToolCallTarget, args: map[string]any{"task": "fast"}},
	)
	callee := newMatrixCallee()
	src := matrixGraph("adapter_tools_matrix_callee_crash", "handled", "\n"+
		"  allow_tools = [\"adapter.callee.default.tools.*\"]\n"+
		"\n"+
		"  outcome \"handled\" {\n"+
		"    next = step.handled\n"+
		"  }\n"+
		"\n")
	sink, audit := runMatrixCase(t, src, caller, callee)

	assertCrashReplies(t, caller)
	assertCalleeCrashObservability(t, sink)

	// Audit: the caller's policy allow for the call plus the nested
	// failure entry, both at layer 0 (the caller's own layer).
	denies := audit.auditDecisions("deny")
	if len(denies) != 1 || !strings.Contains(denies[0].Reason, "callee_crash") || !strings.Contains(denies[0].Reason, "callee exploded") {
		t.Fatalf("deny audit entries = %+v, want exactly one nested-failure entry naming callee_crash and the callee error", denies)
	}
	if denies[0].Layer != 0 || denies[0].SessionID != "caller.default" {
		t.Fatalf("deny audit entry = layer %d session %q, want layer 0 session \"caller.default\"", denies[0].Layer, denies[0].SessionID)
	}
	if allows := audit.auditDecisions("allow"); len(allows) != 2 {
		t.Fatalf("allow audit entries = %d, want 2 (one per scripted call)", len(allows))
	}
	assertSessionCloseSummary(t, audit, 2)

	// The callee ran both tasks in its own session (the crash did not
	// wedge or close it) and never entered the FSM.
	assertCalleeCrashExecutions(t, callee)
	assertMatrixRunContinued(t, sink, "handled")
}

// matrixCaseCalleeTimeout covers the callee timeout: the caller step's
// timeout fires while the nested callee is blocked. The nested context is
// cancelled cleanly (the callee observes context.DeadlineExceeded and
// returns), the caller receives a typed call_error "callee_timeout" — no
// stream wedge — and the run continues through the caller's own outcome
// routing.
func matrixCaseCalleeTimeout(t *testing.T) {
	caller := newMatrixCaller([]string{"adapter_tools", "execute"}, "handled", matrixCall{
		requestID: "call-1",
		target:    matrixToolCallTarget,
		args:      map[string]any{"task": "block"},
	})
	callee := newMatrixCallee()
	src := matrixGraph("adapter_tools_matrix_callee_timeout", "handled", "\n"+
		"  timeout     = \"200ms\"\n"+
		"  allow_tools = [\"adapter.callee.default.tools.*\"]\n"+
		"\n"+
		"  outcome \"handled\" {\n"+
		"    next = step.handled\n"+
		"  }\n"+
		"\n")
	sink, audit := runMatrixCase(t, src, caller, callee)

	assertTypedCallError(t, caller, "callee_timeout")

	assertNoStepEvents(t, sink, "permission.denied")
	assertCallerStepEvent(t, sink, "tool.call_result", map[string]string{"call_error": "callee_timeout"})

	denies := audit.auditDecisions("deny")
	if len(denies) != 1 || !strings.Contains(denies[0].Reason, "callee_timeout") {
		t.Fatalf("deny audit entries = %+v, want exactly one nested-failure entry naming callee_timeout", denies)
	}
	if denies[0].Layer != 0 || denies[0].SessionID != "caller.default" {
		t.Fatalf("deny audit entry = layer %d session %q, want layer 0 session \"caller.default\"", denies[0].Layer, denies[0].SessionID)
	}
	assertSessionCloseSummary(t, audit, 1)

	assertCalleeTimeoutExecution(t, callee)
	assertMatrixRunContinued(t, sink, "handled")
}

// matrixCaseCapabilityMissing covers the capability-missing caller: the
// caller adapter never declared the adapter_tools capability in its runtime
// handshake. Gate 1 rejects the call typed as capability_missing BEFORE any
// policy evaluation — the step's tools grant would have allowed this call,
// so exactly one audit entry (the deterministic reject, no allow) is the
// CRI-159 gate's fingerprint. No permission events, no dispatch, the callee
// is never touched, and the run continues through the caller's own outcome
// routing.
func matrixCaseCapabilityMissing(t *testing.T) {
	caller := newMatrixCaller([]string{"execute"}, "handled", matrixCall{
		requestID: "call-1",
		target:    matrixToolCallTarget,
		args:      map[string]any{"task": "do-thing"},
	})
	callee := newMatrixCallee()
	src := matrixGraph("adapter_tools_matrix_capability_missing", "handled", "\n"+
		"  tools = [adapter.callee.default.tools.helper_task]\n"+
		"\n"+
		"  outcome \"handled\" {\n"+
		"    next = step.handled\n"+
		"  }\n"+
		"\n")
	sink, audit := runMatrixCase(t, src, caller, callee)

	assertTypedCallError(t, caller, "capability_missing")

	// CRI-159 gate lock: the gate precedes the policy — no
	// permission.granted, no permission.denied, no dispatch.
	assertNoStepEvents(t, sink, "permission.granted")
	assertNoStepEvents(t, sink, "permission.denied")
	assertNoStepEvents(t, sink, "tool.call")
	assertNoStepEvents(t, sink, "tool.call_result")

	entries := audit.all()
	if len(entries) != 1 {
		t.Fatalf("audit entries = %d, want exactly 1 (the tools grant must never be evaluated: the gate precedes the policy, and the reject records no permission decision): %+v", len(entries), entries)
	}
	if entries[0].Decision != "deny" || entries[0].Reason != "adapter tool call rejected: capability_missing" {
		t.Fatalf("audit entry = decision %q reason %q, want deny \"adapter tool call rejected: capability_missing\"", entries[0].Decision, entries[0].Reason)
	}
	if entries[0].Layer != 0 || entries[0].SessionID != "caller.default" || entries[0].RequestID != "call-1" {
		t.Fatalf("audit entry = layer %d session %q request %q, want layer 0 session \"caller.default\" request \"call-1\"", entries[0].Layer, entries[0].SessionID, entries[0].RequestID)
	}

	assertCalleeNeverExecuted(t, callee)
	assertMatrixRunContinued(t, sink, "handled")
}
