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
type nestedCalleeAdapter struct {
	rec       *nestedCalleeRecorder
	permTools []string // plain permission.request tools to emit (distinct request ids)
	outputs   map[string]cty.Value
	execErr   error
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
	for i, tool := range a.permTools {
		sink.Adapter("permission.request", map[string]any{
			"request_id": "callee-perm-" + string(rune('0'+i)),
			"tool":       tool,
		})
	}
	if a.execErr != nil {
		return adapter.Result{Outcome: "failure"}, a.execErr
	}
	return adapter.Result{Outcome: "success", Outputs: a.outputs}, nil
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
// direct-dispatch tests (unknown callee, crash, depth, invalid args).
func directToolCallSink(t *testing.T, sm *SessionManager, audit *sliceAuditWriter, step *workflow.StepNode, graph *workflow.FSMGraph, toolDepth int) (*permissionInterceptSink, *permissionState) {
	t.Helper()
	ctx := context.Background()
	if err := sm.Open(ctx, nestedCallerSession, "caller", "", nil, nil); err != nil {
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
		toolDepth: toolDepth,
		execCtx:   ctx,
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
	// toolDepth 1 means this call would nest at depth 2 > 1.
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
