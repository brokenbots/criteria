package adapterhost

// tool_call_exec_test.go — tests for the CRI-160 nested execution path: an
// allowed adapter tool call runs the callee in its own session through a
// nested SessionManager.Execute, governed by the callee's own environment and
// allow_tools policy, with the callee's outputs encoded back to the caller as
// the typed tool result. Covers the compiled-graph flow, lazy binding of
// verified-only callees, unknown callees, callee crashes, depth bounds,
// input validation, and abort_run propagation.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/workflow"
)

var (
	_ Handle             = (*nestedCallerAdapter)(nil)
	_ PermissionStreamer = (*nestedCallerAdapter)(nil)
	_ Handle             = (*nestedCalleeAdapter)(nil)
)

// nestedCalleeRecorder collects the sessions and steps observed by the callee
// adapter across every Execute. Loader factories hand out the same instance,
// so observations from the bound session land here directly.
type nestedCalleeRecorder struct {
	mu    sync.Mutex
	sess  []string
	steps []*workflow.StepNode
}

func (r *nestedCalleeRecorder) record(session string, step *workflow.StepNode) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sess = append(r.sess, session)
	r.steps = append(r.steps, step)
}

func (r *nestedCalleeRecorder) calleeSession() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.sess) == 0 {
		return ""
	}
	return r.sess[0]
}

func (r *nestedCalleeRecorder) calleeStep() *workflow.StepNode {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.steps) == 0 {
		return nil
	}
	return r.steps[0]
}

// nestedCalleeAdapter is the callee-side fake. It declares a schema surface
// (input requires "task"; outputs "report" string + "count" number), records
// the session/step it executed under, optionally emits plain permission
// requests, and returns configurable outputs or an error.
// CRI-161 test hooks are args-driven so one adapter instance can serve
// concurrent nested Executes: a task value of "block" blocks until the
// nested Execute context is done (recording what the callee observed),
// "slow" delays long enough for a released sibling call to complete first
// — which is what makes interleaved reply delivery observable — and "hold"
// blocks until the test releases it, so a call's reply order can be pinned
// deterministically.
type nestedCalleeAdapter struct {
	rec       *nestedCalleeRecorder
	permTools []string // plain permission.request tools to emit (distinct request ids)
	outputs   map[string]cty.Value
	execErr   error
	// holdRelease unblocks the "hold" task (CRI-169 test hook).
	holdRelease chan struct{}

	ctxErrMu sync.Mutex
	ctxErrs  []error // ctx.Err() values observed by blocking executes
}

func (a *nestedCalleeAdapter) Info(_ context.Context) (Info, error) {
	return Info{
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

func (a *nestedCalleeAdapter) OpenSession(_ context.Context, _ string, _, _ map[string]string) error {
	return nil
}
func (a *nestedCalleeAdapter) CloseSession(_ context.Context, _ string) error { return nil }
func (a *nestedCalleeAdapter) Kill()                                          {}
func (a *nestedCalleeAdapter) Pause(context.Context, string) error            { return nil }
func (a *nestedCalleeAdapter) Resume(context.Context, string) error           { return nil }
func (a *nestedCalleeAdapter) Inspect(context.Context, string) (*v2.InspectResponse, error) {
	return &v2.InspectResponse{}, nil
}
func (a *nestedCalleeAdapter) Snapshot(context.Context, string) (*v2.SnapshotResponse, error) {
	return &v2.SnapshotResponse{}, nil
}
func (a *nestedCalleeAdapter) Restore(context.Context, string, []byte, uint32) error { return nil }

func (a *nestedCalleeAdapter) Execute(ctx context.Context, sessionID string, step *workflow.StepNode, sink adapter.EventSink) (adapter.Result, error) {
	a.rec.record(sessionID, step)
	task := step.Input["task"]
	switch task {
	case "block":
		// CRI-161: hold the nested Execute open until the host's context
		// (step timeout or run cancellation) reaches it.
		<-ctx.Done()
		a.recordCtxErr(ctx.Err())
		return adapter.Result{Outcome: "failure"}, ctx.Err()
	case "hold":
		// CRI-169: hold the nested Execute open until the test releases it,
		// or the host's context reaches it first (pause drain cancel).
		select {
		case <-a.holdRelease:
		case <-ctx.Done():
			a.recordCtxErr(ctx.Err())
			return adapter.Result{Outcome: "failure"}, ctx.Err()
		}
	case "slow":
		time.Sleep(150 * time.Millisecond)
	}
	for i, tool := range a.permTools {
		sink.Adapter("permission.request", map[string]any{
			"request_id": "callee-perm-" + string(rune('0'+i)),
			"tool":       tool,
		})
	}
	if a.execErr != nil {
		return adapter.Result{Outcome: "failure"}, a.execErr
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

func (a *nestedCalleeAdapter) recordCtxErr(err error) {
	a.ctxErrMu.Lock()
	defer a.ctxErrMu.Unlock()
	a.ctxErrs = append(a.ctxErrs, err)
}

func (a *nestedCalleeAdapter) recordedCtxErr(i int) error {
	a.ctxErrMu.Lock()
	defer a.ctxErrMu.Unlock()
	if i >= len(a.ctxErrs) {
		return nil
	}
	return a.ctxErrs[i]
}

// nestedCallerAdapter is the caller-side fake. It emits one adapter tool call
// through its permission stream and blocks until the host replies with the
// typed tool_call_result, which it records.
type nestedCallerAdapter struct {
	target string
	args   map[string]any

	mu       sync.Mutex
	requests <-chan *v2.PermissionEvent
	result   *v2.ToolCallResult
}

func (a *nestedCallerAdapter) Info(_ context.Context) (Info, error) {
	return Info{Capabilities: []string{"adapter_tools", "execute"}}, nil
}

func (a *nestedCallerAdapter) OpenSession(_ context.Context, _ string, _, _ map[string]string) error {
	return nil
}
func (a *nestedCallerAdapter) CloseSession(_ context.Context, _ string) error { return nil }
func (a *nestedCallerAdapter) Kill()                                          {}
func (a *nestedCallerAdapter) Pause(context.Context, string) error            { return nil }
func (a *nestedCallerAdapter) Resume(context.Context, string) error           { return nil }
func (a *nestedCallerAdapter) Inspect(context.Context, string) (*v2.InspectResponse, error) {
	return &v2.InspectResponse{}, nil
}
func (a *nestedCallerAdapter) Snapshot(context.Context, string) (*v2.SnapshotResponse, error) {
	return &v2.SnapshotResponse{}, nil
}
func (a *nestedCallerAdapter) Restore(context.Context, string, []byte, uint32) error { return nil }

func (a *nestedCallerAdapter) StartPermissionStream(_ context.Context, _ string, requests <-chan *v2.PermissionEvent) (func(), error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.requests = requests
	return func() {}, nil
}

func (a *nestedCallerAdapter) Execute(ctx context.Context, _ string, _ *workflow.StepNode, sink adapter.EventSink) (adapter.Result, error) {
	payload := map[string]any{"request_id": "call-1", "target": a.target}
	if a.args != nil {
		payload["args"] = a.args
	}
	sink.Adapter("permission.request", payload)

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
				return adapter.Result{Outcome: "failure"}, errors.New("permission stream closed before tool_call_result")
			}
			if tcr := ev.GetToolCallResult(); tcr != nil {
				a.mu.Lock()
				a.result = tcr
				a.mu.Unlock()
				return adapter.Result{Outcome: "success"}, nil
			}
		case <-ctx.Done():
			return adapter.Result{Outcome: "failure"}, ctx.Err()
		case <-deadline:
			return adapter.Result{Outcome: "failure"}, errors.New("timed out waiting for tool_call_result")
		}
	}
}

func (a *nestedCallerAdapter) gotResult() *v2.ToolCallResult {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.result
}

// ---------- fixtures ----------

// nestedLifecycleCollector captures adapter lifecycle events.
type nestedLifecycleCollector struct {
	mu     sync.Mutex
	events [][4]string // runID, adapter, status, detail
}

func (c *nestedLifecycleCollector) OnAdapterLifecycle(runID, adapter, status, detail string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, [4]string{runID, adapter, status, detail})
}

func (c *nestedLifecycleCollector) saw(adapter, status string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, ev := range c.events {
		if ev[1] == adapter && ev[2] == status {
			return true
		}
	}
	return false
}

// nestedToolCallGraph builds a hand-built graph with the caller/callee pair.
// The callee declares dynamic tools and the "prod" environment so the
// synthetic-step environment is observable.
func nestedToolCallGraph() *workflow.FSMGraph {
	return &workflow.FSMGraph{
		Adapters: map[string]*workflow.AdapterNode{
			"caller.instance": {Type: "caller", Name: "instance"},
			"callee.helper":   {Type: "callee", Name: "helper", Environment: "prod", DynamicTools: true},
		},
	}
}

// nestedToolCallWorkflowHCL is the compiled-graph workflow: the callee runs in
// the "prod" environment, the caller step's allow_tools covers both the call
// itself and a plain tool the callee must NOT inherit, and the workflow-level
// permissions grant the callee's own tool surface.
const nestedToolCallWorkflowHCL = `
workflow {
  name = "x"
  version       = "0.1"
  initial_state = "call"
  target_state  = "done"
}

environment "shell" "prod" {
  os = "linux"
}

adapter "caller" "instance" {}
adapter "callee" "helper" {
  environment   = shell.prod
  dynamic_tools = true
}

step "call" {
  target = adapter.caller.instance
  allow_tools = ["adapter.callee.helper.tools.*", "caller.only.*"]
  outcome "success" { next = step.done }
}
state "done" { terminal = true }

permissions {
  allow_tools = ["callee.helpers.*"]
}
`

func compileNestedToolCallGraph(t *testing.T) *workflow.FSMGraph {
	t.Helper()
	spec, diags := workflow.Parse("nested.hcl", []byte(nestedToolCallWorkflowHCL))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := workflow.Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	return g
}

// newNestedToolCallManager builds a SessionManager with builtin caller/callee
// adapters wired to the given fakes.
func newNestedToolCallManager(t *testing.T, caller *nestedCallerAdapter, callee *nestedCalleeAdapter) *SessionManager {
	t.Helper()
	loader := NewLoaderWithDiscovery(func(string) (string, error) { return "", nil })
	loader.RegisterBuiltin("caller", func() Handle { return caller })
	loader.RegisterBuiltin("callee", func() Handle { return callee })
	return NewSessionManager(loader)
}

// nestedCallerStep is the caller's step: its allow_tools covers the adapter
// tool call and a plain tool family ("caller.only.*") that the callee must
// not inherit.
func nestedCallerStep() *workflow.StepNode {
	return &workflow.StepNode{
		Name:       "call",
		AdapterRef: "caller.instance",
		AllowTools: []string{"adapter.callee.helper.tools.*", "caller.only.*"},
		Outcomes: map[string]*workflow.CompiledOutcome{
			"success": {Name: "success"},
			"failure": {Name: "failure"},
		},
	}
}

const (
	nestedCallerSession = "caller.instance"
	nestedCalleeSession = "callee.helper"
	nestedCallTarget    = "adapter.callee.helper.tools.helper_task"
)

func nestedToolCallArgs() map[string]any {
	return map[string]any{"task": "do-thing"}
}

// ---------- tests ----------

// TestNestedToolCall_Success drives the full nested path against a compiled
// graph: the caller tool-calls the callee, the callee executes in its own
// session under its own environment and workflow-level allow_tools, and the
// caller receives the callee's typed outputs as the tool result.
func TestNestedToolCall_Success(t *testing.T) {
	audit := &sliceAuditWriter{}
	lifecycle := &nestedLifecycleCollector{}
	calleeRec := &nestedCalleeRecorder{}
	caller := &nestedCallerAdapter{target: nestedCallTarget, args: nestedToolCallArgs()}
	callee := &nestedCalleeAdapter{rec: calleeRec, outputs: map[string]cty.Value{
		"report": cty.StringVal("done"),
		"count":  cty.NumberIntVal(3),
	}}
	sm := newNestedToolCallManager(t, caller, callee)
	sm.Audit = audit
	sm.LifecycleSink = lifecycle

	graph := compileNestedToolCallGraph(t)
	sm.SetGraph(graph)
	ctx := context.Background()
	if err := sm.VerifyGraph(ctx, graph, nil); err != nil {
		t.Fatalf("VerifyGraph: %v", err)
	}
	if err := sm.Open(ctx, nestedCallerSession, "caller", "", nil, nil); err != nil {
		t.Fatalf("Open caller: %v", err)
	}
	defer func() { _ = sm.Close(ctx, nestedCallerSession) }()
	if err := sm.Open(ctx, nestedCalleeSession, "callee", "", nil, nil); err != nil {
		t.Fatalf("Open callee: %v", err)
	}
	defer func() { _ = sm.Close(ctx, nestedCalleeSession) }()

	inner := &adapterEventCollector{}
	res, err := sm.Execute(ctx, nestedCallerSession, nestedCallerStep(), inner)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Outcome != "success" {
		t.Errorf("caller outcome = %q, want success (caller routing unaffected)", res.Outcome)
	}

	// The caller received the callee's result as the typed tool result.
	tcr := caller.gotResult()
	if tcr == nil {
		t.Fatal("caller never received tool_call_result")
	}
	if tcr.RequestId != "call-1" || tcr.CallError != "" || tcr.Outcome != "success" {
		t.Errorf("tool_call_result = %+v, want req-1/success/no error", tcr)
	}
	// Outputs are typed: decode against the declared object shape.
	objType := cty.Object(map[string]cty.Type{"report": cty.String, "count": cty.Number})
	vals, decErr := ctyjson.Unmarshal(tcr.OutputsJson, objType)
	if decErr != nil {
		t.Fatalf("decode outputs_json %q: %v", tcr.OutputsJson, decErr)
	}
	if got := vals.GetAttr("report").AsString(); got != "done" {
		t.Errorf("outputs.report = %q, want done", got)
	}
	count, _ := vals.GetAttr("count").AsBigFloat().Int64()
	if count != 3 {
		t.Errorf("outputs.count = %d, want 3", count)
	}

	// The callee executed in its own session with the synthetic step carrying
	// its own environment, workflow-level allow_tools, validated input, and
	// declared output schema.
	sessions := calleeRec.calleeSession()
	if sessions != nestedCalleeSession {
		t.Fatalf("callee executed in session %q, want %q (caller: %q)", sessions, nestedCalleeSession, nestedCallerSession)
	}
	step := calleeRec.calleeStep()
	if step == nil {
		t.Fatal("callee never recorded a step")
	}
	if step.AdapterRef != nestedCalleeSession || step.TargetKind != workflow.StepTargetAdapter {
		t.Errorf("synthetic step ref/kind = %q/%v", step.AdapterRef, step.TargetKind)
	}
	if step.Environment != "shell.prod" {
		t.Errorf("callee step environment = %q, want shell.prod (callee's own)", step.Environment)
	}
	if len(step.AllowTools) != 1 || step.AllowTools[0] != "callee.helpers.*" {
		t.Errorf("callee step allow_tools = %v, want [callee.helpers.*] (workflow-level, not the caller's)", step.AllowTools)
	}
	if got := step.Input["task"]; got != "do-thing" {
		t.Errorf("callee step input task = %q, want do-thing", got)
	}
	if len(step.Input) != 1 {
		t.Errorf("callee step input keys = %v, want only task", step.Input)
	}
	if step.OutputSchema["report"].CtyType != cty.String || step.OutputSchema["count"].CtyType != cty.Number {
		t.Errorf("callee step output schema = %+v, want declared report/count types", step.OutputSchema)
	}
	if step.OnCrash != "" {
		t.Errorf("callee step on_crash = %q, want empty (adapter-level policy governs)", step.OnCrash)
	}

	// The tool-call grant flowed to the caller's stream.
	grant, ok := inner.first("permission.granted")
	if !ok || grant["request_id"] != "call-1" {
		t.Errorf("permission.granted = %+v, want call-1 grant", grant)
	}
	if inner.saw("permission.denied") {
		t.Error("expected no permission.denied in the success path")
	}

	// Audit recorded the caller's allow decision for the call.
	var callAllow bool
	for _, entry := range audit.all() {
		if entry.SessionID == nestedCallerSession && entry.RequestID == "call-1" && entry.Decision == "allow" {
			callAllow = true
		}
	}
	if !callAllow {
		t.Errorf("audit missing allow entry for caller tool call; entries = %+v", audit.all())
	}

}

// TestNestedToolCall_CalleeOwnAllowTools proves the callee's permission
// requests are evaluated under the callee's OWN policy (workflow-level
// allow_tools), not the caller's step policy: the caller's step policy allows
// "caller.only.*", but the callee's inherited allow_tools does not, so the
// callee's plain request for that tool is denied. The denial rides back as
// data (needs_review) without changing the caller's outcome routing.
func TestNestedToolCall_CalleeOwnAllowTools(t *testing.T) {
	audit := &sliceAuditWriter{}
	calleeRec := &nestedCalleeRecorder{}
	caller := &nestedCallerAdapter{target: nestedCallTarget, args: nestedToolCallArgs()}
	callee := &nestedCalleeAdapter{
		rec: calleeRec,
		permTools: []string{
			"callee.helpers.read_file", // matches the callee's workflow-level allow_tools
			"caller.only.tool",         // allowed by the CALLER's step policy, not the callee's
		},
		outputs: map[string]cty.Value{"report": cty.StringVal("partial")},
	}
	sm := newNestedToolCallManager(t, caller, callee)
	sm.Audit = audit

	sm.SetGraph(compileNestedToolCallGraph(t))
	ctx := context.Background()
	if err := sm.VerifyGraph(ctx, sm.graph, nil); err != nil {
		t.Fatalf("VerifyGraph: %v", err)
	}
	if err := sm.Open(ctx, nestedCallerSession, "caller", "", nil, nil); err != nil {
		t.Fatalf("Open caller: %v", err)
	}
	defer func() { _ = sm.Close(ctx, nestedCallerSession) }()
	if err := sm.Open(ctx, nestedCalleeSession, "callee", "", nil, nil); err != nil {
		t.Fatalf("Open callee: %v", err)
	}
	defer func() { _ = sm.Close(ctx, nestedCalleeSession) }()

	inner := &adapterEventCollector{}
	res, err := sm.Execute(ctx, nestedCallerSession, nestedCallerStep(), inner)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Outcome != "success" {
		t.Errorf("caller outcome = %q, want success (denials ride as data)", res.Outcome)
	}

	tcr := caller.gotResult()
	if tcr == nil {
		t.Fatal("caller never received tool_call_result")
	}
	// The callee's denial overrides its own outcome to needs_review (ADR-0004
	// §5: results are data; the caller decides what to do with them).
	if tcr.Outcome != "needs_review" {
		t.Errorf("tool_call_result outcome = %q, want needs_review", tcr.Outcome)
	}
	if tcr.CallError != "" {
		t.Errorf("tool_call_result call_error = %q, want empty", tcr.CallError)
	}

	// The callee's requests were evaluated under the callee's own policy.
	// The inner collector also sees the caller's grant for the tool call
	// itself, so match by tool.
	grant, ok := inner.firstWhere("permission.granted", func(d map[string]any) bool {
		return d["tool"] == "callee.helpers.read_file"
	})
	if !ok {
		t.Error("expected permission.granted for callee.helpers.read_file under the callee's own policy")
	} else if grant["request_id"] != "callee-perm-0" {
		t.Errorf("grant request_id = %v, want callee-perm-0", grant["request_id"])
	}
	denied, ok := inner.firstWhere("permission.denied", func(d map[string]any) bool {
		return d["tool"] == "caller.only.tool"
	})
	if !ok {
		t.Error("expected permission.denied for caller.only.tool (callee policy, not the caller's)")
	} else if denied["request_id"] != "callee-perm-1" {
		t.Errorf("deny request_id = %v, want callee-perm-1", denied["request_id"])
	}

	// Audit shows the callee-session denial for the inherited-forbidden tool.
	var denyEntry bool
	for _, entry := range audit.all() {
		if entry.SessionID == nestedCalleeSession && entry.Tool == "caller.only.tool" && entry.Decision == "deny" {
			denyEntry = true
		}
	}
	if !denyEntry {
		t.Errorf("audit missing callee-session deny entry for caller.only.tool; entries = %+v", audit.all())
	}
}

// TestNestedToolCall_LazyBind_VerifiedOnlyCallee drives the lazy-bind path: a
// callee that VerifyGraph-style verification recorded (via Verify) but that
// was never step-targeted or opened is bound and opened on first call.
func TestNestedToolCall_LazyBind_VerifiedOnlyCallee(t *testing.T) {
	audit := &sliceAuditWriter{}
	lifecycle := &nestedLifecycleCollector{}
	calleeRec := &nestedCalleeRecorder{}
	caller := &nestedCallerAdapter{target: nestedCallTarget, args: nestedToolCallArgs()}
	callee := &nestedCalleeAdapter{rec: calleeRec, outputs: map[string]cty.Value{
		"report": cty.StringVal("lazy"),
		"count":  cty.NumberIntVal(1),
	}}
	sm := newNestedToolCallManager(t, caller, callee)
	sm.Audit = audit
	sm.LifecycleSink = lifecycle

	sm.SetGraph(nestedToolCallGraph())
	ctx := context.Background()
	// The callee is verified (recorded for lazy binding) but never opened.
	if err := sm.Verify(ctx, nestedCalleeSession, "callee", "", nil, nil, nil, "", "wf", "inst-1"); err != nil {
		t.Fatalf("Verify callee: %v", err)
	}
	if err := sm.Open(ctx, nestedCallerSession, "caller", "", nil, nil); err != nil {
		t.Fatalf("Open caller: %v", err)
	}
	defer func() { _ = sm.Close(ctx, nestedCallerSession) }()

	callerStep := nestedCallerStep()
	inner := &adapterEventCollector{}
	res, err := sm.Execute(ctx, nestedCallerSession, callerStep, inner)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Outcome != "success" {
		t.Errorf("caller outcome = %q, want success", res.Outcome)
	}

	tcr := caller.gotResult()
	if tcr == nil || tcr.CallError != "" || tcr.Outcome != "success" {
		t.Fatalf("tool_call_result = %+v, want success", tcr)
	}
	objType := cty.Object(map[string]cty.Type{"report": cty.String, "count": cty.Number})
	vals, decErr := ctyjson.Unmarshal(tcr.OutputsJson, objType)
	if decErr != nil {
		t.Fatalf("decode outputs: %v", decErr)
	}
	if got := vals.GetAttr("report").AsString(); got != "lazy" {
		t.Errorf("outputs.report = %q, want lazy", got)
	}

	// The callee ran in its own (lazily bound) session.
	if got := calleeRec.calleeSession(); got != nestedCalleeSession {
		t.Fatalf("callee executed in session %q, want %q", got, nestedCalleeSession)
	}
	// Lifecycle reported the lazy open of the callee session.
	if !lifecycle.saw(nestedCalleeSession, "opened") {
		t.Error("expected lifecycle 'opened' event for lazily bound callee session")
	}
}

// directToolCallSink builds an intercept sink wired to a real manager for the
// direct-dispatch tests (unknown callee, crash, depth, invalid args). It opens
// the caller session with a background context: session lifetime is the run
// scope. Tests that need to drive a step timeout or run cancellation into the
// nested dispatch assign sink.execCtx after construction (withExecCtx).
//
// toolDepth seeds the sink's per-call nesting state at the given depth with a
// call chain containing only the caller session's own ref (nestedCallerSession,
// which is the caller's adapter ref) — the caller's baseline, with no simulated
// caller above it. Tests that need a specific call chain (runtime cycle
// detection) assign sink.nesting directly after construction, like withExecCtx.
func directToolCallSink(t *testing.T, sm *SessionManager, audit *sliceAuditWriter, step *workflow.StepNode, graph *workflow.FSMGraph, toolDepth int) (*permissionInterceptSink, *permissionState) {
	t.Helper()
	if err := sm.Open(context.Background(), nestedCallerSession, "caller", "", nil, nil); err != nil {
		t.Fatalf("Open caller: %v", err)
	}
	sess, err := sm.lookup(nestedCallerSession)
	if err != nil {
		t.Fatalf("lookup caller: %v", err)
	}
	ps := NewPermissionState(nestedCallerSession, audit)
	ps.SetStreamCancel(func() {})
	ps.SetPolicy(&spyPolicy{allow: true, reason: "matched: adapter.callee.helper.tools.*"})
	sink := &permissionInterceptSink{
		inner:     &adapterEventCollector{},
		permState: ps,
		session:   sess,
		step:      step,
		graph:     graph,
		mgr:       sm,
		nesting: toolCallNesting{
			depth: toolDepth,
			chain: []string{nestedCallerSession},
		},
	}
	return sink, ps
}

// TestNestedToolCall_UnknownAdapter: a callee that is neither in the verified
// set nor boundable resolves to a typed unknown_adapter failure.
func TestNestedToolCall_UnknownAdapter(t *testing.T) {
	audit := &sliceAuditWriter{}
	calleeRec := &nestedCalleeRecorder{}
	sm := newNestedToolCallManager(t,
		&nestedCallerAdapter{target: nestedCallTarget},
		&nestedCalleeAdapter{rec: calleeRec},
	)
	graph := nestedToolCallGraph()
	sink, ps := directToolCallSink(t, sm, audit, nestedCallerStep(), graph, 0)

	sink.Adapter("permission.request", map[string]any{
		"request_id": "call-1",
		"target":     nestedCallTarget,
	})
	tcr := readToolCallResult(t, ps)
	if tcr.CallError != callErrorUnknownAdapter {
		t.Errorf("call_error = %q, want %q", tcr.CallError, callErrorUnknownAdapter)
	}
	if calleeRec.calleeSession() != "" {
		t.Errorf("callee unexpectedly executed in session %q", calleeRec.calleeSession())
	}
}

// TestNestedToolCall_CalleeCrash: a plain callee failure surfaces to the
// caller as a typed callee_crash with an audit trail, leaving the caller's
// outcome routing untouched.
func TestNestedToolCall_CalleeCrash(t *testing.T) {
	audit := &sliceAuditWriter{}
	calleeRec := &nestedCalleeRecorder{}
	sm := newNestedToolCallManager(t,
		&nestedCallerAdapter{target: nestedCallTarget},
		&nestedCalleeAdapter{rec: calleeRec, execErr: errors.New("callee exploded")},
	)
	ctx := context.Background()
	if err := sm.Verify(ctx, nestedCalleeSession, "callee", "", nil, nil, nil, "", "wf", "inst-1"); err != nil {
		t.Fatalf("Verify callee: %v", err)
	}
	sink, ps := directToolCallSink(t, sm, audit, nestedCallerStep(), nestedToolCallGraph(), 0)

	sink.Adapter("permission.request", map[string]any{
		"request_id": "call-1",
		"target":     nestedCallTarget,
		"args":       nestedToolCallArgs(),
	})
	tcr := readToolCallResult(t, ps)
	if tcr.CallError != callErrorCalleeCrash {
		t.Errorf("call_error = %q, want %q", tcr.CallError, callErrorCalleeCrash)
	}
	var crashEntry bool
	for _, entry := range audit.all() {
		if entry.Decision == "deny" && entry.SessionID == nestedCallerSession &&
			entry.Reason == "nested callee execution failed: callee_crash: callee exploded" {
			crashEntry = true
		}
	}
	if !crashEntry {
		t.Errorf("audit missing callee_crash deny entry; entries = %+v", audit.all())
	}
}

// TestNestedToolCall_DepthExceeded: a call that would exceed
// policy.max_tool_depth is a typed depth_exceeded failure.
func TestNestedToolCall_DepthExceeded(t *testing.T) {
	audit := &sliceAuditWriter{}
	sm := newNestedToolCallManager(t,
		&nestedCallerAdapter{target: nestedCallTarget},
		&nestedCalleeAdapter{rec: &nestedCalleeRecorder{}},
	)
	graph := nestedToolCallGraph()
	graph.Policy = workflow.Policy{MaxToolDepth: 1}
	// Nesting depth 1 means this call would nest at depth 2 > 1.
	sink, ps := directToolCallSink(t, sm, audit, nestedCallerStep(), graph, 1)

	sink.Adapter("permission.request", map[string]any{
		"request_id": "call-1",
		"target":     nestedCallTarget,
	})
	tcr := readToolCallResult(t, ps)
	if tcr.CallError != callErrorDepthExceeded {
		t.Errorf("call_error = %q, want %q", tcr.CallError, callErrorDepthExceeded)
	}
}

// TestNestedToolCall_InvalidArgs: call args are validated against the
// callee's declared input schema before execution.
func TestNestedToolCall_InvalidArgs(t *testing.T) {
	audit := &sliceAuditWriter{}
	calleeRec := &nestedCalleeRecorder{}
	sm := newNestedToolCallManager(t,
		&nestedCallerAdapter{target: nestedCallTarget},
		&nestedCalleeAdapter{rec: calleeRec},
	)
	ctx := context.Background()
	if err := sm.Verify(ctx, nestedCalleeSession, "callee", "", nil, nil, nil, "", "wf", "inst-1"); err != nil {
		t.Fatalf("Verify callee: %v", err)
	}
	sink, ps := directToolCallSink(t, sm, audit, nestedCallerStep(), nestedToolCallGraph(), 0)

	sink.Adapter("permission.request", map[string]any{
		"request_id": "call-1",
		"target":     nestedCallTarget,
		"args":       map[string]any{}, // required "task" missing
	})
	tcr := readToolCallResult(t, ps)
	if tcr.CallError != callErrorInvalidArgs {
		t.Errorf("call_error = %q, want %q", tcr.CallError, callErrorInvalidArgs)
	}
	if calleeRec.calleeSession() != "" {
		t.Errorf("callee unexpectedly executed for invalid args")
	}
}

// TestNestedToolCall_CalleeAbortRunPropagates: a crash-classified callee
// failure under the callee's own on_crash=abort_run aborts the run — the
// fatal error propagates past the caller's Execute to the engine — while the
// caller still receives a typed callee_crash reply so its Execute unblocks.
func TestNestedToolCall_CalleeAbortRunPropagates(t *testing.T) {
	audit := &sliceAuditWriter{}
	calleeRec := &nestedCalleeRecorder{}
	caller := &nestedCallerAdapter{target: nestedCallTarget, args: nestedToolCallArgs()}
	callee := &nestedCalleeAdapter{rec: calleeRec, execErr: errors.New("connection terminated")}
	sm := newNestedToolCallManager(t, caller, callee)
	sm.Audit = audit

	sm.SetGraph(nestedToolCallGraph())
	ctx := context.Background()
	if err := sm.Open(ctx, nestedCallerSession, "caller", "", nil, nil); err != nil {
		t.Fatalf("Open caller: %v", err)
	}
	defer func() { _ = sm.Close(ctx, nestedCallerSession) }()
	if err := sm.Open(ctx, nestedCalleeSession, "callee", "abort_run", nil, nil); err != nil {
		t.Fatalf("Open callee: %v", err)
	}
	defer func() { _ = sm.Close(ctx, nestedCalleeSession) }()

	inner := &adapterEventCollector{}
	_, err := sm.Execute(ctx, nestedCallerSession, nestedCallerStep(), inner)
	var fatal *FatalRunError
	if !errors.As(err, &fatal) {
		t.Fatalf("Execute error = %v, want *FatalRunError (abort_run)", err)
	}

	// The caller still got its typed reply (unblocked, not hung).
	tcr := caller.gotResult()
	if tcr == nil || tcr.CallError != callErrorCalleeCrash {
		t.Fatalf("tool_call_result = %+v, want callee_crash", tcr)
	}
	// The callee's own on_crash governed its session: the crash event was
	// emitted on the callee's stream.
	if !inner.saw("session.crash") {
		t.Error("expected session.crash event from the callee's abort_run handling")
	}
}

// ---------- CRI-161: async dispatch, reply correlation, timeout/cancel ----------

// withExecCtx configures the sink to serve the given Execute context. The
// production path assigns execCtx in newPermissionInterceptSink; tests set it
// explicitly to drive step timeouts and run cancellation into the nested
// dispatch (CRI-161).
func withExecCtx(sink *permissionInterceptSink, execCtx context.Context) *permissionInterceptSink {
	sink.execCtx = execCtx
	return sink
}

// readToolCallResults drains stream events until n tool_call_result events
// arrive, returning them in arrival order (any interleaved grant/cancel
// events are skipped). Arrival order is the observable for interleaved
// in-flight calls: ordered replies are explicitly NOT assumed by the
// Permissions stream contract, but the hold/slow fixture makes the released
// sibling deterministic first.
func readToolCallResults(t *testing.T, ps *permissionState, n int) []*v2.ToolCallResult {
	t.Helper()
	var results []*v2.ToolCallResult
	for len(results) < n {
		ev := readStreamEvent(t, ps)
		if tcr := ev.GetToolCallResult(); tcr != nil {
			results = append(results, tcr)
		}
	}
	return results
}

// pendingCount snapshots the caller session's pending tool-call registry.
func pendingCount(t *testing.T, ps *permissionState) int {
	t.Helper()
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return len(ps.pendingToolCalls)
}

// TestNestedToolCall_InterleavedReplies (CRI-161): two tool calls issued from
// one caller session, the second held open until the first is confirmed
// in flight. The calls run concurrently, the released call's reply arrives
// first, and each reply carries its own call's outputs — correlation is by
// request_id, not by reply order.
func TestNestedToolCall_InterleavedReplies(t *testing.T) {
	audit := &sliceAuditWriter{}
	calleeRec := &nestedCalleeRecorder{}
	callee := &nestedCalleeAdapter{rec: calleeRec, holdRelease: make(chan struct{})}
	sm := newNestedToolCallManager(t,
		&nestedCallerAdapter{target: nestedCallTarget},
		callee,
	)
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

	// directToolCallSink opens the caller session itself.
	sink, ps := directToolCallSink(t, sm, audit, nestedCallerStep(), sm.graph, 0)

	// Issue both calls back-to-back: dispatch is asynchronous, so both are
	// in flight before either completes. The first call is slow; the second
	// holds until the test releases it below, so it cannot complete — and
	// clear its pending entry — before the in-flight assertion observes both
	// registrations. After the release, the held call finishes while the slow
	// sibling still sleeps, so its reply deterministically arrives first.
	sink.Adapter("permission.request", map[string]any{
		"request_id": "call-1",
		"target":     nestedCallTarget,
		"args":       map[string]any{"task": "slow"},
	})
	sink.Adapter("permission.request", map[string]any{
		"request_id": "call-2",
		"target":     nestedCallTarget,
		"args":       map[string]any{"task": "hold"},
	})
	if got := pendingCount(t, ps); got != 2 {
		t.Fatalf("pending registry = %d entries, want 2 in-flight calls", got)
	}
	close(callee.holdRelease)

	results := readToolCallResults(t, ps, 2)
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	if id := results[0].RequestId; id != "call-2" {
		t.Errorf("first result = %q, want call-2 (released call completed first)", id)
	}
	if id := results[1].RequestId; id != "call-1" {
		t.Errorf("second result = %q, want call-1", id)
	}
	for _, tcr := range results {
		if tcr.CallError != "" || tcr.Outcome != "success" {
			t.Errorf("result %s = %q/%q, want success with no error", tcr.RequestId, tcr.Outcome, tcr.CallError)
		}
	}
	// Each reply carries its own call's outputs.
	var report1, report2 string
	for _, tcr := range results {
		typed, err := ctyjson.Unmarshal(tcr.OutputsJson, cty.Object(map[string]cty.Type{"report": cty.String, "count": cty.Number}))
		if err != nil {
			t.Fatalf("decode outputs for %s: %v", tcr.RequestId, err)
		}
		switch tcr.RequestId {
		case "call-1":
			report1 = typed.GetAttr("report").AsString()
		case "call-2":
			report2 = typed.GetAttr("report").AsString()
		}
	}
	if report1 != "slow" || report2 != "hold" {
		t.Errorf("outputs reports = call-1:%q call-2:%q, want slow/hold (correlated by request_id)", report1, report2)
	}

	// Both nested executes ran the callee in its own session and settled.
	sink.waitPending()
	if got := pendingCount(t, ps); got != 0 {
		t.Errorf("pending registry = %d entries after settle, want 0", got)
	}
	calleeRec.mu.Lock()
	sessions := append([]string(nil), calleeRec.sess...)
	calleeRec.mu.Unlock()
	if len(sessions) != 2 {
		t.Fatalf("callee executed %d times, want 2", len(sessions))
	}
	for _, got := range sessions {
		if got != nestedCalleeSession {
			t.Errorf("callee executed in session %q, want %q", got, nestedCalleeSession)
		}
	}
}

// TestNestedToolCall_StepTimeoutMidCall (CRI-161): the caller step's deadline
// expires while a nested tool call is in flight. The caller receives the
// typed callee_timeout failure, the nested Execute context is cancelled so
// the callee observes the deadline, and the correlation machinery stays live
// — a follow-up call still gets its reply (no stream wedge).
func TestNestedToolCall_StepTimeoutMidCall(t *testing.T) {
	audit := &sliceAuditWriter{}
	calleeRec := &nestedCalleeRecorder{}
	callee := &nestedCalleeAdapter{rec: calleeRec}
	sm := newNestedToolCallManager(t,
		&nestedCallerAdapter{target: nestedCallTarget},
		callee,
	)
	sm.Audit = audit
	sm.SetGraph(nestedToolCallGraph())
	execCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := sm.Open(context.Background(), nestedCalleeSession, "callee", "", nil, nil); err != nil {
		t.Fatalf("Open callee: %v", err)
	}
	defer func() { _ = sm.Close(context.Background(), nestedCalleeSession) }()

	sink, ps := directToolCallSink(t, sm, audit, nestedCallerStep(), sm.graph, 0)
	sink = withExecCtx(sink, execCtx)

	sink.Adapter("permission.request", map[string]any{
		"request_id": "call-1",
		"target":     nestedCallTarget,
		"args":       map[string]any{"task": "block"},
	})

	tcr := readToolCallResult(t, ps)
	if tcr.CallError != callErrorCalleeTimeout {
		t.Errorf("call_error = %q, want %q", tcr.CallError, callErrorCalleeTimeout)
	}
	if tcr.Outcome != "" {
		t.Errorf("outcome = %q, want empty (failure replies carry call_error only)", tcr.Outcome)
	}
	// The deadline reached the nested Execute: the callee observed the
	// cancelled context, so it unblocked cleanly.
	sink.waitPending()
	if got := callee.recordedCtxErr(0); !errors.Is(got, context.DeadlineExceeded) {
		t.Errorf("callee ctx err = %v, want context.DeadlineExceeded", got)
	}

	// No wedge: after the timeout the correlation machinery still delivers
	// replies for follow-up calls.
	sink.Adapter("permission.request", map[string]any{
		"request_id": "call-2",
		"target":     nestedCallTarget,
		"args":       map[string]any{"task": "fast"},
	})
	tcr2 := readToolCallResult(t, ps)
	if tcr2.RequestId != "call-2" || tcr2.CallError != "" || tcr2.Outcome != "success" {
		t.Errorf("follow-up result = %+v, want call-2 success (no wedge)", tcr2)
	}

	// The audit recorded the typed timeout for the abandoned call.
	var timeoutEntry bool
	for _, entry := range audit.all() {
		if entry.SessionID == nestedCallerSession && entry.RequestID == "call-1" &&
			entry.Decision == "deny" && strings.Contains(entry.Reason, callErrorCalleeTimeout) {
			timeoutEntry = true
		}
	}
	if !timeoutEntry {
		t.Errorf("audit missing callee_timeout deny entry; entries = %+v", audit.all())
	}
}

// TestNestedToolCall_RunCanceledMidCall (CRI-161): the run is cancelled while
// a nested tool call is in flight. The caller receives the typed canceled
// failure and the callee observes the cancelled context; a follow-up call
// still gets its reply (no stream wedge).
func TestNestedToolCall_RunCanceledMidCall(t *testing.T) {
	audit := &sliceAuditWriter{}
	calleeRec := &nestedCalleeRecorder{}
	callee := &nestedCalleeAdapter{rec: calleeRec}
	sm := newNestedToolCallManager(t,
		&nestedCallerAdapter{target: nestedCallTarget},
		callee,
	)
	sm.Audit = audit
	sm.SetGraph(nestedToolCallGraph())
	execCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := sm.Open(context.Background(), nestedCalleeSession, "callee", "", nil, nil); err != nil {
		t.Fatalf("Open callee: %v", err)
	}
	defer func() { _ = sm.Close(context.Background(), nestedCalleeSession) }()

	sink, ps := directToolCallSink(t, sm, audit, nestedCallerStep(), sm.graph, 0)
	sink = withExecCtx(sink, execCtx)

	sink.Adapter("permission.request", map[string]any{
		"request_id": "call-1",
		"target":     nestedCallTarget,
		"args":       map[string]any{"task": "block"},
	})
	// Let the callee reach its blocking point, then cancel the run.
	time.Sleep(50 * time.Millisecond)
	cancel()

	tcr := readToolCallResult(t, ps)
	if tcr.CallError != callErrorCanceled {
		t.Errorf("call_error = %q, want %q", tcr.CallError, callErrorCanceled)
	}
	sink.waitPending()
	if got := callee.recordedCtxErr(0); !errors.Is(got, context.Canceled) {
		t.Errorf("callee ctx err = %v, want context.Canceled", got)
	}

	// No wedge: the machinery still delivers replies after cancellation.
	sink.Adapter("permission.request", map[string]any{
		"request_id": "call-2",
		"target":     nestedCallTarget,
		"args":       map[string]any{"task": "fast"},
	})
	tcr2 := readToolCallResult(t, ps)
	if tcr2.RequestId != "call-2" || tcr2.CallError != "" || tcr2.Outcome != "success" {
		t.Errorf("follow-up result = %+v, want call-2 success (no wedge)", tcr2)
	}

	var cancelEntry bool
	for _, entry := range audit.all() {
		if entry.SessionID == nestedCallerSession && entry.RequestID == "call-1" &&
			entry.Decision == "deny" && strings.Contains(entry.Reason, callErrorCanceled) {
			cancelEntry = true
		}
	}
	if !cancelEntry {
		t.Errorf("audit missing canceled deny entry; entries = %+v", audit.all())
	}
}

// TestNestedToolCall_SessionCloseDrainsPending (CRI-161): a tool call still
// in flight when the caller session closes is drained from the pending
// registry and audited as abandoned; the completing goroutine still settles
// without wedging or delivering onto the closed stream.
func TestNestedToolCall_SessionCloseDrainsPending(t *testing.T) {
	audit := &sliceAuditWriter{}
	calleeRec := &nestedCalleeRecorder{}
	sm := newNestedToolCallManager(t,
		&nestedCallerAdapter{target: nestedCallTarget},
		&nestedCalleeAdapter{rec: calleeRec},
	)
	sm.Audit = audit
	sm.SetGraph(nestedToolCallGraph())
	if err := sm.Verify(context.Background(), nestedCalleeSession, "callee", "", nil, nil, nil, "", "wf", "inst-1"); err != nil {
		t.Fatalf("Verify callee: %v", err)
	}
	execCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sink, ps := directToolCallSink(t, sm, audit, nestedCallerStep(), sm.graph, 0)
	sink = withExecCtx(sink, execCtx)

	sink.Adapter("permission.request", map[string]any{
		"request_id": "call-1",
		"target":     nestedCallTarget,
		"args":       map[string]any{"task": "block"},
	})
	if got := pendingCount(t, ps); got != 1 {
		t.Fatalf("pending registry = %d entries, want 1 in-flight call", got)
	}

	// Session close: the stream is torn down and the pending call audited.
	ps.Stop()
	if got := pendingCount(t, ps); got != 0 {
		t.Errorf("pending registry = %d entries after Stop, want 0 (drained)", got)
	}
	if ps.Requests() != nil {
		t.Error("expected the requests channel to be closed/nil after Stop")
	}
	var abandonment bool
	for _, entry := range audit.all() {
		if entry.RequestID == "call-1" && entry.Decision == "cancelled" &&
			strings.Contains(entry.Reason, "session closed while in flight") {
			abandonment = true
		}
	}
	if !abandonment {
		t.Errorf("audit missing tool-call abandonment entry; entries = %+v", audit.all())
	}

	// The completing goroutine still settles: its delivery is dropped (the
	// stream is dead) and nothing wedges.
	cancel()
	sink.waitPending()
}
