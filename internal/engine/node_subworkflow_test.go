package engine

// node_subworkflow_test.go — unit tests for runSubworkflow (W13, Phase 3).
//
// These tests verify the runtime entry point without requiring W14's step
// target wiring. They call runSubworkflow directly and assert on the returned
// output map (matching the workstream-specified signature).

import (
	"context"
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"

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
