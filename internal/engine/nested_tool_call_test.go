package engine

// nested_tool_call_test.go — CRI-160 exit criterion at the engine level: a
// caller adapter tool-calls a callee adapter; the callee executes in its own
// session under its own environment and workflow-level allow_tools, and the
// callee's typed outputs reach the caller as the tool result.

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
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow"
)

const nestedToolCallWorkflowHCL = `
workflow {
  name = "nested_tool_call"
  version       = "0.1"
  initial_state = "call"
  target_state  = "done"
}

environment "shell" "prod" {
  os = "linux"
}

adapter "caller" "default" {}
adapter "callee" "default" {
  environment   = shell.prod
  dynamic_tools = true
}

step "call" {
  target = adapter.caller.default
  allow_tools = ["adapter.callee.default.tools.*", "caller.only.*"]
  outcome "success" { next = step.done }
}
state "done" { terminal = true }

permissions {
  allow_tools = ["callee.helpers.*"]
}
`

const (
	nestedEngineTarget     = "adapter.callee.default.tools.helper_task"
	nestedEngineCallerSess = "caller.default"
	nestedEngineCalleeSess = "callee.default"
)

// nestedEngineRecorder collects callee-side observations across the run.
type nestedEngineRecorder struct {
	mu       sync.Mutex
	sessions []string
	steps    []*workflow.StepNode
}

func (r *nestedEngineRecorder) record(session string, step *workflow.StepNode) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sessions = append(r.sessions, session)
	r.steps = append(r.steps, step)
}

func (r *nestedEngineRecorder) calleeSession() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.sessions) == 0 {
		return ""
	}
	return r.sessions[0]
}

func (r *nestedEngineRecorder) calleeStep() *workflow.StepNode {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.steps) == 0 {
		return nil
	}
	return r.steps[0]
}

func (r *nestedEngineRecorder) allSessions() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.sessions...)
}

// nestedEngineCallee is the callee fake: typed outputs, plain permission
// requests, and full session/step recording.
//
// CRI-161 test hooks are args-driven so one adapter instance can serve
// concurrent nested Executes: task "block" holds the nested Execute open
// until its context is done (recording what the callee observed), "slow"
// delays long enough for a faster sibling call to complete first, and outputs
// are derived per call so interleaved replies can be matched to their own
// call.
type nestedEngineCallee struct {
	rec       *nestedEngineRecorder
	permTools []string
	// outputs overrides the per-call derived outputs; set by tests that
	// assert specific output values (CRI-160 end-to-end).
	outputs map[string]cty.Value

	ctxErrMu sync.Mutex
	ctxErrs  []error
}

func (a *nestedEngineCallee) recordCtxErr(err error) {
	a.ctxErrMu.Lock()
	defer a.ctxErrMu.Unlock()
	a.ctxErrs = append(a.ctxErrs, err)
}

func (a *nestedEngineCallee) recordedCtxErr(i int) error {
	a.ctxErrMu.Lock()
	defer a.ctxErrMu.Unlock()
	if i >= len(a.ctxErrs) {
		return nil
	}
	return a.ctxErrs[i]
}

func (a *nestedEngineCallee) Info(context.Context) (adapterhost.Info, error) {
	return adapterhost.Info{
		Capabilities: []string{"execute"},
		AdapterInfo: workflow.AdapterInfo{
			InputSchema: map[string]workflow.ConfigField{
				"task": {Required: true},
			},
			OutputSchema: map[string]workflow.ConfigField{
				"report": {CtyType: cty.String},
				"count":  {CtyType: cty.Number},
			},
		},
	}, nil
}
func (a *nestedEngineCallee) OpenSession(context.Context, string, map[string]string, map[string]string) error {
	return nil
}
func (a *nestedEngineCallee) Execute(ctx context.Context, sessionID string, step *workflow.StepNode, sink adapter.EventSink) (adapter.Result, error) {
	a.rec.record(sessionID, step)
	task := step.Input["task"]
	switch task {
	case "block":
		// CRI-161: hold the nested Execute open until the host's context
		// (the caller step's timeout or run cancellation) reaches it.
		<-ctx.Done()
		a.recordCtxErr(ctx.Err())
		return adapter.Result{Outcome: "failure"}, ctx.Err()
	case "slow":
		time.Sleep(150 * time.Millisecond)
	}
	for i, tool := range a.permTools {
		sink.Adapter("permission.request", map[string]any{
			"request_id": "callee-perm-" + string(rune('0'+i)),
			"tool":       tool,
		})
	}
	outputs := a.outputs
	if outputs == nil {
		// Derive per-call outputs from the input so interleaved replies can
		// be matched to their own call.
		outputs = map[string]cty.Value{
			"report": cty.StringVal(task),
			"count":  cty.NumberIntVal(int64(len(task))),
		}
	}
	return adapter.Result{Outcome: "success", Outputs: outputs}, nil
}
func (a *nestedEngineCallee) CloseSession(context.Context, string) error { return nil }
func (a *nestedEngineCallee) Kill()                                      {}
func (a *nestedEngineCallee) Pause(context.Context, string) error        { return nil }
func (a *nestedEngineCallee) Resume(context.Context, string) error       { return nil }
func (a *nestedEngineCallee) Inspect(context.Context, string) (*v2.InspectResponse, error) {
	return &v2.InspectResponse{}, nil
}
func (a *nestedEngineCallee) Snapshot(context.Context, string) (*v2.SnapshotResponse, error) {
	return &v2.SnapshotResponse{}, nil
}
func (a *nestedEngineCallee) Restore(context.Context, string, []byte, uint32) error { return nil }

// nestedEngineCaller is the caller fake: it emits one adapter tool call and
// blocks until the host replies with the typed tool_call_result, which it
// records and re-exports as its own step outputs.
type nestedEngineCaller struct {
	mu       sync.Mutex
	requests <-chan *v2.PermissionEvent
	result   *v2.ToolCallResult
}

func (a *nestedEngineCaller) Info(context.Context) (adapterhost.Info, error) {
	return adapterhost.Info{Capabilities: []string{"adapter_tools", "execute"}}, nil
}
func (a *nestedEngineCaller) OpenSession(context.Context, string, map[string]string, map[string]string) error {
	return nil
}
func (a *nestedEngineCaller) StartPermissionStream(_ context.Context, _ string, requests <-chan *v2.PermissionEvent) (func(), error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.requests = requests
	return func() {}, nil
}
func (a *nestedEngineCaller) Execute(ctx context.Context, _ string, _ *workflow.StepNode, sink adapter.EventSink) (adapter.Result, error) {
	sink.Adapter("permission.request", map[string]any{
		"request_id": "call-1",
		"target":     nestedEngineTarget,
		"args":       map[string]any{"task": "do-thing"},
	})

	a.mu.Lock()
	requests := a.requests
	a.mu.Unlock()
	if requests == nil {
		return adapter.Result{Outcome: "failure"}, errors.New("permission stream not started")
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-requests:
			if !ok {
				return adapter.Result{Outcome: "failure"}, errors.New("permission stream closed")
			}
			if tcr := ev.GetToolCallResult(); tcr != nil {
				a.mu.Lock()
				a.result = tcr
				a.mu.Unlock()
				// Re-emit the decoded outputs as the caller's own step
				// outputs so the engine's typed-output path validates them.
				outputs := map[string]cty.Value{}
				if len(tcr.OutputsJson) > 0 {
					typed, err := ctyjson.Unmarshal(tcr.OutputsJson, cty.Object(map[string]cty.Type{
						"report": cty.String,
						"count":  cty.Number,
					}))
					if err != nil {
						return adapter.Result{Outcome: "failure"}, err
					}
					for k := range typed.Type().AttributeTypes() {
						outputs[k] = typed.GetAttr(k)
					}
				}
				return adapter.Result{Outcome: "success", Outputs: outputs}, nil
			}
		case <-ctx.Done():
			return adapter.Result{Outcome: "failure"}, ctx.Err()
		case <-deadline:
			return adapter.Result{Outcome: "failure"}, errors.New("timed out waiting for tool_call_result")
		}
	}
}
func (a *nestedEngineCaller) CloseSession(context.Context, string) error { return nil }
func (a *nestedEngineCaller) Kill()                                      {}
func (a *nestedEngineCaller) Pause(context.Context, string) error        { return nil }
func (a *nestedEngineCaller) Resume(context.Context, string) error       { return nil }
func (a *nestedEngineCaller) Inspect(context.Context, string) (*v2.InspectResponse, error) {
	return &v2.InspectResponse{}, nil
}
func (a *nestedEngineCaller) Snapshot(context.Context, string) (*v2.SnapshotResponse, error) {
	return &v2.SnapshotResponse{}, nil
}
func (a *nestedEngineCaller) Restore(context.Context, string, []byte, uint32) error { return nil }

func (a *nestedEngineCaller) gotResult() *v2.ToolCallResult {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.result
}

// engineAuditCollector implements adapterhost.AuditWriter, collecting entries.
type engineAuditCollector struct {
	mu      sync.Mutex
	entries []*adapterhost.DecisionLogEntry
}

func (w *engineAuditCollector) Write(e *adapterhost.DecisionLogEntry) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.entries = append(w.entries, e)
}

func (w *engineAuditCollector) all() []*adapterhost.DecisionLogEntry {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]*adapterhost.DecisionLogEntry(nil), w.entries...)
}

// nestedEngineSink captures lifecycle events on top of fakeSink.
type nestedEngineSink struct {
	fakeSink
	mu   sync.Mutex
	life [][4]string // stepName, adapterName, status, detail
}

func (s *nestedEngineSink) OnAdapterLifecycle(stepName, adapterName, status, detail string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.life = append(s.life, [4]string{stepName, adapterName, status, detail})
}

func (s *nestedEngineSink) lifecycleSaw(adapterName, status string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ev := range s.life {
		if ev[1] == adapterName && ev[2] == status {
			return true
		}
	}
	return false
}

// TestNestedToolCall_EngineEndToEnd runs the compiled workflow through the
// real engine: the caller tool-calls the callee, the callee executes in its
// own session with its own environment and allow_tools, and the outputs are
// typed on the caller side.
func TestNestedToolCall_EngineEndToEnd(t *testing.T) {
	spec, diags := workflow.Parse("nested.hcl", []byte(nestedToolCallWorkflowHCL))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags)
	}
	g, diags := workflow.Compile(spec, map[string]workflow.AdapterInfo{
		"caller.default": {InputSchema: map[string]workflow.ConfigField{}, OutputSchema: map[string]workflow.ConfigField{}},
		"callee.default": {
			InputSchema: map[string]workflow.ConfigField{
				"task": {Required: true},
			},
			OutputSchema: map[string]workflow.ConfigField{
				"report": {CtyType: cty.String},
				"count":  {CtyType: cty.Number},
			},
		},
	})
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags)
	}

	rec := &nestedEngineRecorder{}
	callee := &nestedEngineCallee{
		rec: rec,
		permTools: []string{
			"callee.helpers.read_file", // allowed by the callee's own policy
			"caller.only.tool",         // allowed by the CALLER's step policy, denied on the callee
		},
		outputs: map[string]cty.Value{
			"report": cty.StringVal("done"),
			"count":  cty.NumberIntVal(3),
		},
	}
	caller := &nestedEngineCaller{}

	audit := &engineAuditCollector{}
	sink := &nestedEngineSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"caller": caller,
		"callee": callee,
	}}

	if err := New(g, loader, sink, WithAuditWriter(audit)).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !sink.terminalOK {
		t.Fatalf("run did not complete successfully: terminal=%q failure=%q", sink.terminal, sink.failure)
	}

	// The caller received the callee's typed outputs as the tool result.
	tcr := caller.gotResult()
	if tcr == nil {
		t.Fatal("caller never received tool_call_result")
	}
	if tcr.RequestId != "call-1" || tcr.CallError != "" {
		t.Errorf("tool_call_result = %+v, want call-1 with no call_error", tcr)
	}
	// The callee's own policy denied "caller.only.tool", which overrides the
	// callee's outcome to needs_review (ADR-0004 §5: denials ride as data).
	// The caller's own outcome routing is unaffected — asserted below via
	// terminalOK.
	if tcr.Outcome != "needs_review" {
		t.Errorf("tool_call_result outcome = %q, want needs_review (callee denial as data)", tcr.Outcome)
	}
	typed, err := ctyjson.Unmarshal(tcr.OutputsJson, cty.Object(map[string]cty.Type{
		"report": cty.String,
		"count":  cty.Number,
	}))
	if err != nil {
		t.Fatalf("decode outputs_json %q: %v", tcr.OutputsJson, err)
	}
	if got := typed.GetAttr("report").AsString(); got != "done" {
		t.Errorf("outputs.report = %q, want done", got)
	}
	if count, _ := typed.GetAttr("count").AsBigFloat().Int64(); count != 3 {
		t.Errorf("outputs.count = %d, want 3", count)
	}

	// The callee executed in its own session (not the caller's).
	if got := rec.calleeSession(); got != nestedEngineCalleeSess {
		t.Fatalf("callee executed in session %q, want %q (caller: %q)", got, nestedEngineCalleeSess, nestedEngineCallerSess)
	}

	// The synthetic step carried the callee's OWN environment and the
	// workflow-level allow_tools — not the caller's step policy.
	step := rec.calleeStep()
	if step == nil {
		t.Fatal("callee never recorded a step")
	}
	if step.AdapterRef != nestedEngineCalleeSess {
		t.Errorf("callee step adapter ref = %q, want %q", step.AdapterRef, nestedEngineCalleeSess)
	}
	if step.Environment != "shell.prod" {
		t.Errorf("callee step environment = %q, want shell.prod (callee's own)", step.Environment)
	}
	if len(step.AllowTools) != 1 || step.AllowTools[0] != "callee.helpers.*" {
		t.Errorf("callee step allow_tools = %v, want [callee.helpers.*]", step.AllowTools)
	}
	if got := step.Input["task"]; got != "do-thing" {
		t.Errorf("callee step input task = %q, want do-thing", got)
	}

	// Audit proves the policy split: the caller's call was allowed under its
	// step policy, and the callee's "caller.only.tool" request was denied
	// under the callee's own policy (the caller's "caller.only.*" grant does
	// not cross the session boundary).
	var sawCallerAllow, sawCalleeDeny bool
	for _, entry := range audit.all() {
		if entry.SessionID == nestedEngineCallerSess && entry.Tool == nestedEngineTarget && entry.Decision == "allow" {
			sawCallerAllow = true
		}
		if entry.SessionID == nestedEngineCalleeSess && entry.Tool == "caller.only.tool" && entry.Decision == "deny" {
			sawCalleeDeny = true
		}
	}
	if !sawCallerAllow {
		t.Errorf("audit missing caller allow entry; entries = %+v", audit.all())
	}
	if !sawCalleeDeny {
		t.Errorf("audit missing callee-session deny entry for caller.only.tool; entries = %+v", audit.all())
	}

	// The callee's session was opened lazily on first call (lifecycle event).
	if !sink.lifecycleSaw(nestedEngineCalleeSess, "opened") {
		t.Error("expected lifecycle 'opened' event for the callee session")
	}
}

// nestedToolCallTimeoutWorkflowHCL is the CRI-161 exit-criterion fixture: the
// same nested tool-call workflow with a 200ms timeout on the caller step so a
// blocked nested Execute is torn down by the step deadline, not by the callee.
const nestedToolCallTimeoutWorkflowHCL = `
workflow {
  name = "nested_tool_call_timeout"
  version       = "0.1"
  initial_state = "call"
  target_state  = "done"
}

environment "shell" "prod" {
  os = "linux"
}

adapter "caller" "default" {}
adapter "callee" "default" {
  environment   = shell.prod
  dynamic_tools = true
}

step "call" {
  target = adapter.caller.default
  allow_tools = ["adapter.callee.default.tools.*"]
  timeout = "200ms"
  outcome "success" { next = step.done }
}
state "done" { terminal = true }
`

// nestedEngineInterleavedCaller issues two tool calls back-to-back on one
// caller session and collects the replies as they arrive, in arrival order.
// It never assumes the Permissions stream delivers replies in request order.
type nestedEngineInterleavedCaller struct {
	mu       sync.Mutex
	requests <-chan *v2.PermissionEvent
	results  map[string]*v2.ToolCallResult
	order    []string
}

func (a *nestedEngineInterleavedCaller) Info(context.Context) (adapterhost.Info, error) {
	return adapterhost.Info{Capabilities: []string{"adapter_tools", "execute"}}, nil
}
func (a *nestedEngineInterleavedCaller) OpenSession(context.Context, string, map[string]string, map[string]string) error {
	return nil
}
func (a *nestedEngineInterleavedCaller) StartPermissionStream(_ context.Context, _ string, requests <-chan *v2.PermissionEvent) (func(), error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.requests = requests
	return func() {}, nil
}
func (a *nestedEngineInterleavedCaller) recordLocked(tcr *v2.ToolCallResult) {
	if a.results == nil {
		a.results = map[string]*v2.ToolCallResult{}
	}
	a.results[tcr.RequestId] = tcr
	a.order = append(a.order, tcr.RequestId)
}
func (a *nestedEngineInterleavedCaller) Execute(ctx context.Context, _ string, _ *workflow.StepNode, sink adapter.EventSink) (adapter.Result, error) {
	sink.Adapter("permission.request", map[string]any{
		"request_id": "call-1",
		"target":     nestedEngineTarget,
		"args":       map[string]any{"task": "slow"},
	})
	sink.Adapter("permission.request", map[string]any{
		"request_id": "call-2",
		"target":     nestedEngineTarget,
		"args":       map[string]any{"task": "fast"},
	})

	a.mu.Lock()
	requests := a.requests
	a.mu.Unlock()
	if requests == nil {
		return adapter.Result{Outcome: "failure"}, errors.New("permission stream not started")
	}
	// A single absolute deadline outside the receive loop: an empty stream
	// with two pending calls must not wedge the run.
	deadline := time.After(5 * time.Second)
	for len(a.snapshotResults()) < 2 {
		select {
		case ev, ok := <-requests:
			if !ok {
				return adapter.Result{Outcome: "failure"}, errors.New("permission stream closed")
			}
			if tcr := ev.GetToolCallResult(); tcr != nil {
				a.mu.Lock()
				a.recordLocked(tcr)
				a.mu.Unlock()
			}
		case <-ctx.Done():
			return adapter.Result{Outcome: "failure"}, ctx.Err()
		case <-deadline:
			return adapter.Result{Outcome: "failure"}, errors.New("timed out waiting for tool_call_results")
		}
	}
	return adapter.Result{Outcome: "success"}, nil
}
func (a *nestedEngineInterleavedCaller) CloseSession(context.Context, string) error { return nil }
func (a *nestedEngineInterleavedCaller) Kill()                                      {}
func (a *nestedEngineInterleavedCaller) Pause(context.Context, string) error        { return nil }
func (a *nestedEngineInterleavedCaller) Resume(context.Context, string) error       { return nil }
func (a *nestedEngineInterleavedCaller) Inspect(context.Context, string) (*v2.InspectResponse, error) {
	return &v2.InspectResponse{}, nil
}
func (a *nestedEngineInterleavedCaller) Snapshot(context.Context, string) (*v2.SnapshotResponse, error) {
	return &v2.SnapshotResponse{}, nil
}
func (a *nestedEngineInterleavedCaller) Restore(context.Context, string, []byte, uint32) error {
	return nil
}

func (a *nestedEngineInterleavedCaller) snapshotResults() map[string]*v2.ToolCallResult {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[string]*v2.ToolCallResult, len(a.results))
	for id, tcr := range a.results {
		out[id] = tcr
	}
	return out
}
func (a *nestedEngineInterleavedCaller) arrivalOrder() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.order...)
}

// nestedEngineTimeoutCaller issues one blocking tool call under a caller-step
// timeout and waits only on the reply stream: the typed call_error reply is
// the expected unblock, so it must not race the (already expired) step ctx.
type nestedEngineTimeoutCaller struct {
	mu       sync.Mutex
	requests <-chan *v2.PermissionEvent
	result   *v2.ToolCallResult
}

func (a *nestedEngineTimeoutCaller) Info(context.Context) (adapterhost.Info, error) {
	return adapterhost.Info{Capabilities: []string{"adapter_tools", "execute"}}, nil
}
func (a *nestedEngineTimeoutCaller) OpenSession(context.Context, string, map[string]string, map[string]string) error {
	return nil
}
func (a *nestedEngineTimeoutCaller) StartPermissionStream(_ context.Context, _ string, requests <-chan *v2.PermissionEvent) (func(), error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.requests = requests
	return func() {}, nil
}
func (a *nestedEngineTimeoutCaller) Execute(ctx context.Context, _ string, _ *workflow.StepNode, sink adapter.EventSink) (adapter.Result, error) {
	sink.Adapter("permission.request", map[string]any{
		"request_id": "call-1",
		"target":     nestedEngineTarget,
		"args":       map[string]any{"task": "block"},
	})

	a.mu.Lock()
	requests := a.requests
	a.mu.Unlock()
	if requests == nil {
		return adapter.Result{Outcome: "failure"}, errors.New("permission stream not started")
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-requests:
			if !ok {
				return adapter.Result{Outcome: "failure"}, errors.New("permission stream closed")
			}
			if tcr := ev.GetToolCallResult(); tcr != nil {
				a.mu.Lock()
				a.result = tcr
				a.mu.Unlock()
				// The typed failure is the caller's data; the step itself
				// completes so the run continues past the timeout.
				return adapter.Result{Outcome: "success"}, nil
			}
		case <-deadline:
			// Wedged: the host never delivered the typed failure.
			return adapter.Result{Outcome: "failure"}, errors.New("wedged: no tool_call_result after step timeout")
		}
	}
}
func (a *nestedEngineTimeoutCaller) CloseSession(context.Context, string) error { return nil }
func (a *nestedEngineTimeoutCaller) Kill()                                      {}
func (a *nestedEngineTimeoutCaller) Pause(context.Context, string) error        { return nil }
func (a *nestedEngineTimeoutCaller) Resume(context.Context, string) error       { return nil }
func (a *nestedEngineTimeoutCaller) Inspect(context.Context, string) (*v2.InspectResponse, error) {
	return &v2.InspectResponse{}, nil
}
func (a *nestedEngineTimeoutCaller) Snapshot(context.Context, string) (*v2.SnapshotResponse, error) {
	return &v2.SnapshotResponse{}, nil
}
func (a *nestedEngineTimeoutCaller) Restore(context.Context, string, []byte, uint32) error {
	return nil
}

func (a *nestedEngineTimeoutCaller) gotResult() *v2.ToolCallResult {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.result
}

// TestNestedToolCall_EngineInterleavedReplies (CRI-161 exit criterion 1): two
// concurrent tool calls from one caller session, with the fast call's reply
// overtaking the slow call's on the Permissions stream. Both callers' results
// must arrive correctly correlated by request_id.
func TestNestedToolCall_EngineInterleavedReplies(t *testing.T) {
	g := compileNestedToolCallGraph(t, nestedToolCallWorkflowHCL)

	rec := &nestedEngineRecorder{}
	callee := &nestedEngineCallee{rec: rec}
	caller := &nestedEngineInterleavedCaller{}

	sink := &nestedEngineSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"caller": caller,
		"callee": callee,
	}}
	if err := New(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !sink.terminalOK {
		t.Fatalf("run did not complete successfully: terminal=%q failure=%q", sink.terminal, sink.failure)
	}

	results := caller.snapshotResults()
	if len(results) != 2 {
		t.Fatalf("caller received %d tool_call_results, want 2", len(results))
	}
	for _, id := range []string{"call-1", "call-2"} {
		tcr, ok := results[id]
		if !ok {
			t.Fatalf("caller never received a result for %s (got %v)", id, results)
		}
		if tcr.CallError != "" || tcr.Outcome != "success" {
			t.Errorf("%s result = %q/%q, want clean success", id, tcr.Outcome, tcr.CallError)
		}
	}
	// Each reply carries its own call's derived outputs.
	if got := outputsField(t, results["call-1"], "report"); got != "slow" {
		t.Errorf("call-1 outputs.report = %q, want slow", got)
	}
	if got := outputsField(t, results["call-2"], "report"); got != "fast" {
		t.Errorf("call-2 outputs.report = %q, want fast", got)
	}

	// The fast reply overtook the slow reply: ordered delivery is NOT
	// assumed on the Permissions stream.
	order := caller.arrivalOrder()
	if len(order) != 2 || order[0] != "call-2" || order[1] != "call-1" {
		t.Errorf("arrival order = %v, want [call-2 call-1] (interleaved)", order)
	}

	// Both nested Executes ran in the callee's own session.
	sessions := rec.allSessions()
	if len(sessions) != 2 {
		t.Fatalf("callee executed %d times, want 2", len(sessions))
	}
	for i, sess := range sessions {
		if sess != nestedEngineCalleeSess {
			t.Errorf("callee execution %d ran in session %q, want %q", i, sess, nestedEngineCalleeSess)
		}
	}
}

// TestNestedToolCall_EngineStepTimeout (CRI-161 exit criterion 2): a caller
// step timeout mid-call delivers the typed callee_timeout failure to the
// caller, cancels the nested context cleanly, and the run continues — the
// pending map unblocks instead of wedging the Permissions stream.
func TestNestedToolCall_EngineStepTimeout(t *testing.T) {
	g, err := compileNestedToolCallGraphErr(nestedToolCallTimeoutWorkflowHCL)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	callee := &nestedEngineCallee{rec: &nestedEngineRecorder{}}
	caller := &nestedEngineTimeoutCaller{}

	sink := &nestedEngineSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"caller": caller,
		"callee": callee,
	}}
	if err := New(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// The run continued past the timed-out call: the caller treated the typed
	// failure as data and completed its step.
	if !sink.terminalOK {
		t.Fatalf("run did not continue after callee timeout: terminal=%q failure=%q", sink.terminal, sink.failure)
	}

	// The caller received the typed failure instead of wedging.
	tcr := caller.gotResult()
	if tcr == nil {
		t.Fatal("caller never received tool_call_result after step timeout")
	}
	if tcr.RequestId != "call-1" || tcr.CallError != "callee_timeout" {
		t.Errorf("tool_call_result = req %q err %q, want call-1/callee_timeout", tcr.RequestId, tcr.CallError)
	}
	if tcr.Outcome != "" {
		t.Errorf("tool_call_result outcome = %q, want empty on failure reply", tcr.Outcome)
	}

	// The nested context was cancelled cleanly inside the callee.
	if got := callee.recordedCtxErr(0); !errors.Is(got, context.DeadlineExceeded) {
		t.Errorf("callee observed ctx error %v, want %v", got, context.DeadlineExceeded)
	}
}

func compileNestedToolCallGraph(t *testing.T, hcl string) *workflow.FSMGraph {
	t.Helper()
	g, err := compileNestedToolCallGraphErr(hcl)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return g
}

func compileNestedToolCallGraphErr(hcl string) (*workflow.FSMGraph, error) {
	spec, diags := workflow.Parse("nested.hcl", []byte(hcl))
	if diags.HasErrors() {
		return nil, errors.New("parse: " + diags.Error())
	}
	g, diags := workflow.Compile(spec, map[string]workflow.AdapterInfo{
		"caller.default": {InputSchema: map[string]workflow.ConfigField{}, OutputSchema: map[string]workflow.ConfigField{}},
		"callee.default": {
			InputSchema: map[string]workflow.ConfigField{
				"task": {Required: true},
			},
			OutputSchema: map[string]workflow.ConfigField{
				"report": {CtyType: cty.String},
				"count":  {CtyType: cty.Number},
			},
		},
	})
	if diags.HasErrors() {
		return nil, errors.New("compile: " + diags.Error())
	}
	return g, nil
}

// outputsField decodes a tool_call_result's outputs_json and returns one
// string attribute for assertions.
func outputsField(t *testing.T, tcr *v2.ToolCallResult, field string) string {
	t.Helper()
	typed, err := ctyjson.Unmarshal(tcr.OutputsJson, cty.Object(map[string]cty.Type{
		"report": cty.String,
		"count":  cty.Number,
	}))
	if err != nil {
		t.Fatalf("decode outputs_json %q: %v", tcr.OutputsJson, err)
	}
	return typed.GetAttr(field).AsString()
}
