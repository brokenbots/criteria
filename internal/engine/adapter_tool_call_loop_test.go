package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

// CRI-166: the first adapter-to-adapter tool call loop, end to end through
// the real engine. The workflow fixture (testdata/adapter_tool_call_loop.hcl)
// declares two noop adapters: the caller step carries a step-level tools
// grant and tool-calls the callee mid-step; the callee runs in its own
// session (own environment, own allow_tools) and never enters the FSM; its
// outputs are typed on the caller side under callee.* keys and the caller's
// own outcome routing finishes the run. This is the demoable M5 loop and the
// template for the M6 matrix (CRI-167..170).
const (
	adapterToolCallLoopFixture = "testdata/adapter_tool_call_loop.hcl"

	toolCallLoopTarget     = "adapter.callee.default.tools.helper_task"
	toolCallLoopCallerSess = "caller.default"
	toolCallLoopCalleeSess = "callee.default"
)

// toolCallLoopOutputs is the callee's declared/returned typed outputs; the
// caller re-exports them under callee.* keys, and the fixture's run outputs
// project them out of the run.
var toolCallLoopOutputs = map[string]cty.Value{
	"report": cty.StringVal("done"),
	"count":  cty.NumberIntVal(3),
	"meta": cty.ObjectVal(map[string]cty.Value{
		"name": cty.StringVal("widget"),
		"id":   cty.NumberIntVal(7),
	}),
}

// toolCallLoopOutputTypes is the cty object type the caller decodes the tool
// result's outputs_json with — a real caller adapter knows its callee's tool
// contract (ADR-0004 §5: the tool result is data for the caller).
func toolCallLoopOutputTypes() map[string]cty.Type {
	types := make(map[string]cty.Type, len(toolCallLoopOutputs))
	for k, v := range toolCallLoopOutputs {
		types[k] = v.Type()
	}
	return types
}

// loopCalleeAdapter is the callee fake: typed outputs (string, number, and a
// nested object), one plain permission request answered by the callee's own
// allow_tools, and full session/step recording.
type loopCalleeAdapter struct {
	rec *nestedEngineRecorder
}

func (a *loopCalleeAdapter) Info(context.Context) (adapterhost.Info, error) {
	return adapterhost.Info{
		Capabilities: []string{"execute"},
		AdapterInfo: workflow.AdapterInfo{
			InputSchema: map[string]workflow.ConfigField{
				"task": {Required: true},
			},
			OutputSchema: map[string]workflow.ConfigField{
				"report": {CtyType: cty.String},
				"count":  {CtyType: cty.Number},
				"meta":   {CtyType: toolCallLoopOutputs["meta"].Type()},
			},
		},
	}, nil
}
func (a *loopCalleeAdapter) OpenSession(context.Context, string, map[string]string, map[string]string) error {
	return nil
}
func (a *loopCalleeAdapter) Execute(_ context.Context, sessionID string, step *workflow.StepNode, sink adapter.EventSink) (adapter.Result, error) {
	a.rec.record(sessionID, step)
	// A plain permission request from inside the callee's own session: the
	// callee's own allow_tools policy ("callee.helpers.*") must answer it —
	// not anything the caller's step carried.
	sink.Adapter("permission.request", map[string]any{
		"request_id": "callee-perm-0",
		"tool":       "callee.helpers.read_file",
	})
	return adapter.Result{Outcome: "success", Outputs: toolCallLoopOutputs}, nil
}
func (a *loopCalleeAdapter) CloseSession(context.Context, string) error { return nil }
func (a *loopCalleeAdapter) Kill()                                      {}
func (a *loopCalleeAdapter) Pause(context.Context, string) error        { return nil }
func (a *loopCalleeAdapter) Resume(context.Context, string) error       { return nil }
func (a *loopCalleeAdapter) Inspect(context.Context, string) (*v2.InspectResponse, error) {
	return &v2.InspectResponse{}, nil
}
func (a *loopCalleeAdapter) Snapshot(context.Context, string) (*v2.SnapshotResponse, error) {
	return &v2.SnapshotResponse{}, nil
}
func (a *loopCalleeAdapter) Restore(context.Context, string, []byte, uint32) error { return nil }

// loopCallerAdapter is the caller fake: it emits one adapter tool call and
// blocks until the host replies with the typed tool_call_result, which it
// re-exports as its own step outputs under callee.* keys — the namespacing
// the fixture's run outputs project out of the run.
type loopCallerAdapter struct {
	mu       sync.Mutex
	requests <-chan *v2.PermissionEvent
	result   *v2.ToolCallResult
}

func (a *loopCallerAdapter) Info(context.Context) (adapterhost.Info, error) {
	return adapterhost.Info{Capabilities: []string{"adapter_tools", "execute"}}, nil
}
func (a *loopCallerAdapter) OpenSession(context.Context, string, map[string]string, map[string]string) error {
	return nil
}
func (a *loopCallerAdapter) StartPermissionStream(_ context.Context, _ string, requests <-chan *v2.PermissionEvent) (func(), error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.requests = requests
	return func() {}, nil
}
func (a *loopCallerAdapter) Execute(ctx context.Context, _ string, _ *workflow.StepNode, sink adapter.EventSink) (adapter.Result, error) {
	sink.Adapter("permission.request", map[string]any{
		"request_id": "call-1",
		"target":     toolCallLoopTarget,
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
				// Re-export the callee's decoded outputs as the caller's own
				// step outputs, namespaced under callee.* (ADR-0004 §5).
				outputs := map[string]cty.Value{}
				if len(tcr.OutputsJson) > 0 {
					typed, err := ctyjson.Unmarshal(tcr.OutputsJson, cty.Object(toolCallLoopOutputTypes()))
					if err != nil {
						return adapter.Result{Outcome: "failure"}, err
					}
					for k := range typed.Type().AttributeTypes() {
						outputs["callee."+k] = typed.GetAttr(k)
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
func (a *loopCallerAdapter) CloseSession(context.Context, string) error { return nil }
func (a *loopCallerAdapter) Kill()                                      {}
func (a *loopCallerAdapter) Pause(context.Context, string) error        { return nil }
func (a *loopCallerAdapter) Resume(context.Context, string) error       { return nil }
func (a *loopCallerAdapter) Inspect(context.Context, string) (*v2.InspectResponse, error) {
	return &v2.InspectResponse{}, nil
}
func (a *loopCallerAdapter) Snapshot(context.Context, string) (*v2.SnapshotResponse, error) {
	return &v2.SnapshotResponse{}, nil
}
func (a *loopCallerAdapter) Restore(context.Context, string, []byte, uint32) error { return nil }

// gotResult returns the tool_call_result the caller received.
func (a *loopCallerAdapter) gotResult() *v2.ToolCallResult {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.result
}

// loopOutputSink captures the caller step's captured outputs, the run's
// declared outputs, and adapter lifecycle events on top of fakeSink.
type loopOutputSink struct {
	fakeSink
	mu         sync.Mutex
	stepOut    map[string]map[string]string
	runOutputs []map[string]string
	life       [][4]string // stepName, adapterName, status, detail
}

func (s *loopOutputSink) OnStepOutputCaptured(step string, outputs map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stepOut == nil {
		s.stepOut = make(map[string]map[string]string)
	}
	cp := make(map[string]string, len(outputs))
	for k, v := range outputs {
		cp[k] = v
	}
	s.stepOut[step] = cp
}

func (s *loopOutputSink) OnRunOutputs(outputs []map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runOutputs = append(s.runOutputs, outputs...)
}

func (s *loopOutputSink) OnAdapterLifecycle(stepName, adapterName, status, detail string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.life = append(s.life, [4]string{stepName, adapterName, status, detail})
}

func (s *loopOutputSink) lifecycleSaw(adapterName, status string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ev := range s.life {
		if ev[1] == adapterName && ev[2] == status {
			return true
		}
	}
	return false
}

// TestAdapterToolCallLoop_FixtureCompiles proves the committed fixture is
// standalone-valid: it parses and compiles with no adapter schemas at all
// (the tools grammar validation is spec-driven), so CRI-167..170 can reuse
// it as-is.
func TestAdapterToolCallLoop_FixtureCompiles(t *testing.T) {
	g := compileFile(t, adapterToolCallLoopFixture)
	step := g.Steps["call"]
	if step == nil {
		t.Fatal("fixture has no call step")
	}
	if len(step.Tools) != 1 || step.Tools[0].CalleeRef != toolCallLoopCalleeSess || step.Tools[0].Tool != "helper_task" {
		t.Errorf("call step tools grants = %+v, want callee.default/helper_task", step.Tools)
	}
}

// readFixtureSource reads a workflow fixture relative to this package, for
// tests that compile the file with adapter schemas (compileFile compiles
// without schemas).
func readFixtureSource(t *testing.T, rel string) (src []byte, srcPath string) {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve caller")
	}
	srcPath = filepath.Join(filepath.Dir(file), rel)
	src, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatalf("read workflow fixture: %v", err)
	}
	return src, srcPath
}

// TestAdapterToolCallLoop_EngineEndToEnd runs the fixture through the real
// engine: the caller tool-calls the callee, the callee executes in its own
// session with its own environment and allow_tools, its outputs land typed on
// the caller side under callee.* keys, and the callee never enters the FSM.
func TestAdapterToolCallLoop_EngineEndToEnd(t *testing.T) {
	src, srcPath := readFixtureSource(t, adapterToolCallLoopFixture)
	spec, diags := workflow.Parse(srcPath, src)
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags)
	}
	g, diags := workflow.Compile(spec, map[string]workflow.AdapterInfo{
		"caller.default": {
			InputSchema:  map[string]workflow.ConfigField{},
			OutputSchema: map[string]workflow.ConfigField{},
			Capabilities: []string{"adapter_tools"},
		},
		"callee.default": {
			InputSchema: map[string]workflow.ConfigField{
				"task": {Required: true},
			},
			OutputSchema: map[string]workflow.ConfigField{
				"report": {CtyType: cty.String},
				"count":  {CtyType: cty.Number},
				"meta":   {CtyType: toolCallLoopOutputs["meta"].Type()},
			},
		},
	})
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags)
	}

	rec := &nestedEngineRecorder{}
	callee := &loopCalleeAdapter{rec: rec}
	caller := &loopCallerAdapter{}

	audit := &engineAuditCollector{}
	sink := &loopOutputSink{}
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
	if sink.terminal != "done" {
		t.Errorf("terminal state = %q, want done", sink.terminal)
	}

	// Outcome routing fired normally and the callee never entered the FSM:
	// only the caller step ran, with the single call->done transition.
	if got := sink.stepsRun; len(got) != 1 || got[0] != "call" {
		t.Errorf("steps run = %v, want [call]", got)
	}
	if got := sink.transitions; len(got) != 1 || got[0] != "call->done" {
		t.Errorf("transitions = %v, want [call->done] (callee step node must not appear)", got)
	}

	// The caller received the callee's typed outputs as the tool result.
	tcr := caller.gotResult()
	if tcr == nil {
		t.Fatal("caller never received tool_call_result")
	}
	if tcr.RequestId != "call-1" || tcr.CallError != "" || tcr.Outcome != "success" {
		t.Errorf("tool_call_result = %+v, want call-1 clean success", tcr)
	}
	typed, err := ctyjson.Unmarshal(tcr.OutputsJson, cty.Object(toolCallLoopOutputTypes()))
	if err != nil {
		t.Fatalf("decode outputs_json %q: %v", tcr.OutputsJson, err)
	}
	if got := typed.GetAttr("report").AsString(); got != "done" {
		t.Errorf("outputs.report = %q, want done", got)
	}
	if count, _ := typed.GetAttr("count").AsBigFloat().Int64(); count != 3 {
		t.Errorf("outputs.count = %d, want 3", count)
	}
	if id, _ := typed.GetAttr("meta").GetAttr("id").AsBigFloat().Int64(); id != 7 {
		t.Errorf("outputs.meta.id = %d, want 7", id)
	}

	// The callee.* keys are typed on the caller side: the caller step's
	// captured outputs carry them namespaced, and the fixture's run outputs
	// project the typed values out of the run — callee_count renders as the
	// unquoted JSON number 3 and callee_meta_id keeps nested attribute access
	// on the callee's object output, both of which only work when the stored
	// values are typed.
	out := sink.stepOut["call"]
	if out == nil {
		t.Fatal("call step outputs were never captured")
	}
	if out["callee.report"] != "done" {
		t.Errorf("call step output callee.report = %q, want done", out["callee.report"])
	}
	if out["callee.count"] != "3" {
		t.Errorf("call step output callee.count = %q, want 3", out["callee.count"])
	}
	if _, ok := out["report"]; ok {
		t.Error("call step outputs contain a plain \"report\" key; callee outputs must be namespaced under callee.*")
	}
	wantRunOutputs := map[string]string{
		"callee_report":  `"done"`,
		"callee_count":   `3`,
		"callee_meta_id": `7`,
	}
	gotRunOutputs := make(map[string]string, len(sink.runOutputs))
	for _, o := range sink.runOutputs {
		gotRunOutputs[o["name"]] = o["value"]
	}
	for name, want := range wantRunOutputs {
		if got := gotRunOutputs[name]; got != want {
			t.Errorf("run output %s = %q, want %q", name, got, want)
		}
	}

	// The callee executed in its own session (not the caller's).
	if got := rec.calleeSession(); got != toolCallLoopCalleeSess {
		t.Fatalf("callee executed in session %q, want %q (caller: %q)", got, toolCallLoopCalleeSess, toolCallLoopCallerSess)
	}

	// The synthetic step carried the callee's OWN environment and the
	// workflow-level allow_tools — not the caller's step policy.
	step := rec.calleeStep()
	if step == nil {
		t.Fatal("callee never recorded a step")
	}
	if step.AdapterRef != toolCallLoopCalleeSess {
		t.Errorf("callee step adapter ref = %q, want %q", step.AdapterRef, toolCallLoopCalleeSess)
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

	// Audit proves both halves of the loop ran under their own policy: the
	// caller's call was allowed by its step-level tools grant, and the
	// callee's own permission request was allowed by the callee's own
	// workflow-level allow_tools at the callee session (layer 1).
	var sawCallerGrant, sawCalleeAllow bool
	for _, entry := range audit.all() {
		if entry.SessionID == toolCallLoopCallerSess && entry.Tool == toolCallLoopTarget && entry.Decision == "allow" {
			if entry.Layer == 0 && strings.Contains(entry.Reason, "tools entry") {
				sawCallerGrant = true
			}
		}
		if entry.SessionID == toolCallLoopCalleeSess && entry.Tool == "callee.helpers.read_file" && entry.Decision == "allow" && entry.Layer == 1 {
			sawCalleeAllow = true
		}
	}
	if !sawCallerGrant {
		t.Errorf("audit missing caller allow entry for the step-level tools grant; entries = %+v", audit.all())
	}
	if !sawCalleeAllow {
		t.Errorf("audit missing callee-session allow entry under the callee's own allow_tools; entries = %+v", audit.all())
	}

	// The callee's session was opened lazily on first call (lifecycle event).
	if !sink.lifecycleSaw(toolCallLoopCalleeSess, "opened") {
		t.Error("expected lifecycle 'opened' event for the callee session")
	}
}
