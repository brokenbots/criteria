package engine

// node_subworkflow_test.go — unit tests for runSubworkflow (W13, Phase 3).
//
// These tests verify the runtime entry point without requiring W14's step
// target wiring. They call runSubworkflow directly and assert on the returned
// output map (matching the workstream-specified signature).

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/plugin"
	"github.com/brokenbots/criteria/workflow"
)

// minimalSubworkflowNode builds a SubworkflowNode with the simplest possible
// callee FSMGraph: a single terminal state and no declared outputs.
func minimalSubworkflowNode(name string) *workflow.SubworkflowNode {
	body := &workflow.FSMGraph{
		InitialState: "done",
		States: map[string]*workflow.StateNode{
			"done": {Name: "done", Terminal: true, Success: true},
		},
		Variables: map[string]*workflow.VariableNode{},
	}
	return &workflow.SubworkflowNode{
		Name:         name,
		SourcePath:   "/test/" + name,
		Body:         body,
		BodyEntry:    "done",
		Inputs:       map[string]hcl.Expression{},
		DeclaredVars: map[string]*workflow.VariableNode{},
	}
}

// traversalExpr builds an hcl.Expression for a dotted traversal like "var.x"
// or "each.value" without requiring HCL text parsing.
func traversalExpr(root string, attrs ...string) hcl.Expression {
	t := hcl.Traversal{hcl.TraverseRoot{Name: root}}
	for _, a := range attrs {
		t = append(t, hcl.TraverseAttr{Name: a})
	}
	return &hclsyntax.ScopeTraversalExpr{Traversal: t}
}

func testDeps(t *testing.T) Deps {
	t.Helper()
	sessions := plugin.NewSessionManager(plugin.NewLoader())
	t.Cleanup(func() { sessions.Shutdown(context.Background()) })
	return Deps{
		Sessions: sessions,
		Sink:     &fakeSink{},
	}
}

// TestRunSubworkflow_ReachesTerminalState verifies that runSubworkflow executes
// a minimal callee FSMGraph to completion without error, returning nil outputs
// when no output blocks are declared.
func TestRunSubworkflow_ReachesTerminalState(t *testing.T) {
	node := minimalSubworkflowNode("simple")
	parentSt := &RunState{
		Vars:        map[string]cty.Value{"var": cty.EmptyObjectVal},
		WorkflowDir: t.TempDir(),
	}

	outputs, err := runSubworkflow(context.Background(), node, parentSt, testDeps(t))
	if err != nil {
		t.Fatalf("runSubworkflow: %v", err)
	}
	if len(outputs) != 0 {
		t.Errorf("expected no outputs, got %v", outputs)
	}
}

// TestRunSubworkflow_OutputsEvaluated verifies that a callee's declared output
// expressions are evaluated against the final child state and returned to the
// caller. This is the core of Step 6's runtime contract.
func TestRunSubworkflow_OutputsEvaluated(t *testing.T) {
	// Callee declares a literal output: output "status" { value = "ok" }
	body := &workflow.FSMGraph{
		InitialState: "done",
		States:       map[string]*workflow.StateNode{"done": {Name: "done", Terminal: true, Success: true}},
		Variables:    map[string]*workflow.VariableNode{},
		Outputs: map[string]*workflow.OutputNode{
			"status": {Name: "status", Value: &hclsyntax.LiteralValueExpr{Val: cty.StringVal("ok")}},
		},
		OutputOrder: []string{"status"},
	}
	node := &workflow.SubworkflowNode{
		Name:         "status-test",
		Body:         body,
		BodyEntry:    "done",
		Inputs:       map[string]hcl.Expression{},
		DeclaredVars: map[string]*workflow.VariableNode{},
	}
	parentSt := &RunState{
		Vars:        map[string]cty.Value{"var": cty.EmptyObjectVal},
		WorkflowDir: t.TempDir(),
	}

	outputs, err := runSubworkflow(context.Background(), node, parentSt, testDeps(t))
	if err != nil {
		t.Fatalf("runSubworkflow: %v", err)
	}
	got, ok := outputs["status"]
	if !ok {
		t.Fatal("output 'status' not present in returned map")
	}
	if got.AsString() != "ok" {
		t.Errorf("output 'status': want %q, got %q", "ok", got.AsString())
	}
}

// TestRunSubworkflow_InputBoundToOutput verifies the full data-flow path:
// parent input expression → callee var.* → callee output → returned output map.
func TestRunSubworkflow_InputBoundToOutput(t *testing.T) {
	// Callee: variable "greeting" (no default) + output "result" = var.greeting
	body := &workflow.FSMGraph{
		InitialState: "done",
		States:       map[string]*workflow.StateNode{"done": {Name: "done", Terminal: true, Success: true}},
		Variables:    map[string]*workflow.VariableNode{"greeting": {Name: "greeting", Type: cty.String}},
		Outputs: map[string]*workflow.OutputNode{
			"result": {Name: "result", Value: traversalExpr("var", "greeting")},
		},
		OutputOrder: []string{"result"},
	}
	node := &workflow.SubworkflowNode{
		Name:         "greeter",
		Body:         body,
		BodyEntry:    "done",
		Inputs:       map[string]hcl.Expression{"greeting": &hclsyntax.LiteralValueExpr{Val: cty.StringVal("hello")}},
		DeclaredVars: map[string]*workflow.VariableNode{"greeting": {Name: "greeting", Type: cty.String}},
	}
	parentSt := &RunState{
		Vars:        map[string]cty.Value{"var": cty.EmptyObjectVal},
		WorkflowDir: t.TempDir(),
	}

	outputs, err := runSubworkflow(context.Background(), node, parentSt, testDeps(t))
	if err != nil {
		t.Fatalf("runSubworkflow: %v", err)
	}
	got, ok := outputs["result"]
	if !ok {
		t.Fatal("output 'result' not present")
	}
	if got.AsString() != "hello" {
		t.Errorf("output 'result': want %q, got %q", "hello", got.AsString())
	}
}

// TestRunSubworkflow_EachThreadedToOutput verifies that each.* from the parent
// scope is visible inside the subworkflow and can be captured via an output.
func TestRunSubworkflow_EachThreadedToOutput(t *testing.T) {
	// Callee has output "item" = each.value
	body := &workflow.FSMGraph{
		InitialState: "done",
		States:       map[string]*workflow.StateNode{"done": {Name: "done", Terminal: true, Success: true}},
		Variables:    map[string]*workflow.VariableNode{},
		Outputs: map[string]*workflow.OutputNode{
			"item": {Name: "item", Value: traversalExpr("each", "value")},
		},
		OutputOrder: []string{"item"},
	}
	node := &workflow.SubworkflowNode{
		Name:         "each-test",
		Body:         body,
		BodyEntry:    "done",
		Inputs:       map[string]hcl.Expression{},
		DeclaredVars: map[string]*workflow.VariableNode{},
	}
	parentSt := &RunState{
		Vars: map[string]cty.Value{
			"var": cty.EmptyObjectVal,
			"each": cty.ObjectVal(map[string]cty.Value{
				"value": cty.StringVal("item-x"),
				"_idx":  cty.NumberIntVal(0),
			}),
		},
		WorkflowDir: t.TempDir(),
	}

	outputs, err := runSubworkflow(context.Background(), node, parentSt, testDeps(t))
	if err != nil {
		t.Fatalf("runSubworkflow: %v", err)
	}
	got, ok := outputs["item"]
	if !ok {
		t.Fatal("output 'item' not present")
	}
	if got.AsString() != "item-x" {
		t.Errorf("output 'item': want %q, got %q", "item-x", got.AsString())
	}
}

// TestRunSubworkflow_MissingRequiredInput verifies that a missing required input
// variable produces a descriptive error (not a panic).
func TestRunSubworkflow_MissingRequiredInput(t *testing.T) {
	body := &workflow.FSMGraph{
		InitialState: "done",
		States:       map[string]*workflow.StateNode{"done": {Name: "done", Terminal: true, Success: true}},
		Variables:    map[string]*workflow.VariableNode{"required_var": {Name: "required_var", Type: cty.String}},
	}
	node := &workflow.SubworkflowNode{
		Name:         "missing-input",
		Body:         body,
		BodyEntry:    "done",
		Inputs:       map[string]hcl.Expression{},
		DeclaredVars: map[string]*workflow.VariableNode{"required_var": {Name: "required_var", Type: cty.String}},
	}
	parentSt := &RunState{
		Vars:        map[string]cty.Value{"var": cty.EmptyObjectVal},
		WorkflowDir: t.TempDir(),
	}

	_, err := runSubworkflow(context.Background(), node, parentSt, testDeps(t))
	if err == nil {
		t.Fatal("expected error for missing required input, got none")
	}
	if !strings.Contains(err.Error(), "required") {
		t.Errorf("error should mention 'required', got: %v", err)
	}
}

// TestRunSubworkflow_FileFromCalleeDir is a regression test that verifies the
// callee's runtime functions resolve relative paths against the subworkflow's
// source directory (node.SourcePath), not the parent workflow directory.
//
// A subworkflow with output "msg" { value = file("msg.txt") } should succeed
// when msg.txt exists in the subworkflow directory even if the parent workflow
// lives in a completely different directory.
func TestRunSubworkflow_FileFromCalleeDir(t *testing.T) {
	calleeDir := t.TempDir()
	parentDir := t.TempDir()

	// Write msg.txt only in the callee directory, not in the parent directory.
	msgPath := filepath.Join(calleeDir, "msg.txt")
	if err := os.WriteFile(msgPath, []byte("hello from callee"), 0o600); err != nil {
		t.Fatalf("write msg.txt: %v", err)
	}

	// Build a file("msg.txt") expression via HCL parsing.
	fileExpr, diags := hclsyntax.ParseExpression([]byte(`file("msg.txt")`), "test", hcl.Pos{Line: 1, Column: 1})
	if diags.HasErrors() {
		t.Fatalf("parse file expr: %s", diags.Error())
	}

	body := &workflow.FSMGraph{
		InitialState: "done",
		States:       map[string]*workflow.StateNode{"done": {Name: "done", Terminal: true, Success: true}},
		Variables:    map[string]*workflow.VariableNode{},
		Outputs: map[string]*workflow.OutputNode{
			"msg": {Name: "msg", Value: fileExpr},
		},
		OutputOrder: []string{"msg"},
	}
	node := &workflow.SubworkflowNode{
		Name:         "file-test",
		SourcePath:   calleeDir,
		Body:         body,
		BodyEntry:    "done",
		Inputs:       map[string]hcl.Expression{},
		DeclaredVars: map[string]*workflow.VariableNode{},
	}
	// Parent lives in a separate directory — msg.txt does NOT exist there.
	parentSt := &RunState{
		Vars:        map[string]cty.Value{"var": cty.EmptyObjectVal},
		WorkflowDir: parentDir,
	}

	outputs, err := runSubworkflow(context.Background(), node, parentSt, testDeps(t))
	if err != nil {
		t.Fatalf("runSubworkflow: %v", err)
	}
	got, ok := outputs["msg"]
	if !ok {
		t.Fatal("output 'msg' not present")
	}
	if got.AsString() != "hello from callee" {
		t.Errorf("output 'msg': want %q, got %q", "hello from callee", got.AsString())
	}
}

// ctxCheckPlugin is a test plugin whose Execute returns ctx.Err() immediately
// when the context is already cancelled, allowing deterministic cancellation tests.
type ctxCheckPlugin struct {
	fakePlugin
}

func (p *ctxCheckPlugin) Execute(ctx context.Context, _ string, _ *workflow.StepNode, _ adapter.EventSink) (adapter.Result, error) {
	if err := ctx.Err(); err != nil {
		return adapter.Result{}, err
	}
	return adapter.Result{Outcome: "success"}, nil
}

// calleeBodyWithAdapter builds a callee FSMGraph that declares a single adapter
// and has an immediate terminal state. Adapter lifecycle (open/close) happens
// regardless of whether any step uses the adapter.
func calleeBodyWithAdapter(adapterType string) *workflow.FSMGraph {
	instanceID := adapterType + ".default"
	return &workflow.FSMGraph{
		InitialState: "done",
		States:       map[string]*workflow.StateNode{"done": {Name: "done", Terminal: true, Success: true}},
		Variables:    map[string]*workflow.VariableNode{},
		Adapters:     map[string]*workflow.AdapterNode{instanceID: {Type: adapterType, Name: "default"}},
		AdapterOrder: []string{instanceID},
		Policy:       workflow.DefaultPolicy,
	}
}

// calleeBodyWithStep builds a callee FSMGraph with a single step that uses an
// adapter. The step transitions to terminal state on "success" outcome.
func calleeBodyWithStep(adapterType string) *workflow.FSMGraph {
	instanceID := adapterType + ".default"
	return &workflow.FSMGraph{
		InitialState: "work",
		Steps: map[string]*workflow.StepNode{
			"work": {
				Name:    "work",
				Adapter: instanceID,
				Outcomes: map[string]string{
					"success": "done",
				},
			},
		},
		States:       map[string]*workflow.StateNode{"done": {Name: "done", Terminal: true, Success: true}},
		Variables:    map[string]*workflow.VariableNode{},
		Adapters:     map[string]*workflow.AdapterNode{instanceID: {Type: adapterType, Name: "default"}},
		AdapterOrder: []string{instanceID},
		Policy:       workflow.DefaultPolicy,
	}
}

// subworkflowNodeFor wraps a body FSMGraph in a SubworkflowNode.
func subworkflowNodeFor(name string, body *workflow.FSMGraph) *workflow.SubworkflowNode {
	return &workflow.SubworkflowNode{
		Name:         name,
		SourcePath:   "/test/" + name,
		Body:         body,
		BodyEntry:    body.InitialState,
		Inputs:       map[string]hcl.Expression{},
		DeclaredVars: map[string]*workflow.VariableNode{},
	}
}

// depsWithLoader builds a Deps whose SessionManager uses the given loader.
func depsWithLoader(t *testing.T, loader plugin.Loader) Deps {
	t.Helper()
	sessions := plugin.NewSessionManager(loader)
	t.Cleanup(func() { sessions.Shutdown(context.Background()) })
	return Deps{Sessions: sessions, Sink: &fakeSink{}}
}

// TestRunSubworkflow_AdaptersIsolatedFromParent verifies that a callee-declared
// adapter is opened at the start of the subworkflow scope and closed when it
// returns — proving that adapter lifecycle is fully contained within the
// subworkflow and does not leak into the parent scope.
//
// A broken teardown (missing deferred tearDownScopeAdapters) would leave
// closes==0 after runSubworkflow returns, failing the test.
func TestRunSubworkflow_AdaptersIsolatedFromParent(t *testing.T) {
	tracker := &lifecycleTrackingPlugin{fakePlugin: fakePlugin{name: "noop"}}
	loader := &fakeLoader{plugins: map[string]plugin.Plugin{"noop": tracker}}

	node := subworkflowNodeFor("isolated", calleeBodyWithAdapter("noop"))
	parentSt := &RunState{
		Vars:        map[string]cty.Value{"var": cty.EmptyObjectVal},
		WorkflowDir: t.TempDir(),
	}

	_, err := runSubworkflow(context.Background(), node, parentSt, depsWithLoader(t, loader))
	if err != nil {
		t.Fatalf("runSubworkflow: %v", err)
	}

	tracker.mu.Lock()
	opens := tracker.opensCount
	closes := tracker.closesCount
	tracker.mu.Unlock()

	if opens != 1 {
		t.Errorf("callee adapter opens: want 1, got %d", opens)
	}
	if closes != 1 {
		t.Errorf("callee adapter closes: want 1, got %d (adapter leaked past subworkflow boundary)", closes)
	}
}

// TestRunSubworkflow_ErrorPropagatesToParent verifies that a runtime failure
// inside the callee (adapter Execute returning an error) propagates back to
// the caller of runSubworkflow rather than being silently swallowed.
//
// A broken implementation that converts callee errors to empty/nil outputs
// without returning an error would fail this test.
func TestRunSubworkflow_ErrorPropagatesToParent(t *testing.T) {
	errPlugin := &fakePlugin{name: "noop", err: fmt.Errorf("simulated step failure")}
	loader := &fakeLoader{plugins: map[string]plugin.Plugin{"noop": errPlugin}}

	node := subworkflowNodeFor("fail-test", calleeBodyWithStep("noop"))
	parentSt := &RunState{
		Vars:        map[string]cty.Value{"var": cty.EmptyObjectVal},
		WorkflowDir: t.TempDir(),
	}

	_, err := runSubworkflow(context.Background(), node, parentSt, depsWithLoader(t, loader))
	if err == nil {
		t.Fatal("expected error from failing callee step, got nil")
	}
	if !strings.Contains(err.Error(), "simulated step failure") {
		t.Errorf("error should contain step failure message, got: %v", err)
	}
}

// TestRunSubworkflow_CalleeCancellation verifies that cancelling the context
// while the callee is executing causes runSubworkflow to return a
// context-related error rather than completing normally.
//
// A broken implementation that ignored ctx would execute to completion and
// return nil error, failing this test.
func TestRunSubworkflow_CalleeCancellation(t *testing.T) {
	checkPlugin := &ctxCheckPlugin{fakePlugin: fakePlugin{name: "noop"}}
	loader := &fakeLoader{plugins: map[string]plugin.Plugin{"noop": checkPlugin}}

	node := subworkflowNodeFor("cancel-test", calleeBodyWithStep("noop"))
	parentSt := &RunState{
		Vars:        map[string]cty.Value{"var": cty.EmptyObjectVal},
		WorkflowDir: t.TempDir(),
	}

	// Pre-cancel the context: ctxCheckPlugin.Execute returns ctx.Err() immediately,
	// which propagates up through runWorkflowBody as a non-terminal error.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := runSubworkflow(ctx, node, parentSt, depsWithLoader(t, loader))
	if err == nil {
		t.Fatal("expected error after context cancellation, got nil")
	}
	if !strings.Contains(err.Error(), context.Canceled.Error()) {
		t.Errorf("error should mention context cancellation, got: %v", err)
	}
}
