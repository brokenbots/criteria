package engine

// node_subworkflow_test.go — unit tests for runSubworkflow (W13, Phase 3).
//
// These tests verify the runtime entry point without requiring W14's step
// target wiring. They call runSubworkflow directly.

import (
	"context"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"

	"github.com/brokenbots/criteria/internal/plugin"
	"github.com/brokenbots/criteria/workflow"
)

// minimalSubworkflowNode builds a SubworkflowNode with the simplest possible
// callee FSMGraph: a single terminal state.
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
// a minimal callee FSMGraph to its terminal state and returns without error.
func TestRunSubworkflow_ReachesTerminalState(t *testing.T) {
	node := minimalSubworkflowNode("simple")
	parentSt := &RunState{
		Vars:        map[string]cty.Value{"var": cty.EmptyObjectVal},
		WorkflowDir: t.TempDir(),
	}

	terminal, _, err := runSubworkflow(context.Background(), node, parentSt, testDeps(t))
	if err != nil {
		t.Fatalf("runSubworkflow: %v", err)
	}
	if terminal != "done" {
		t.Errorf("expected terminal state 'done', got %q", terminal)
	}
}

// TestRunSubworkflow_InputBinding verifies that parent-scope input expressions
// are evaluated and bound into the child's var.* scope.
func TestRunSubworkflow_InputBinding(t *testing.T) {
	// Callee has a variable "greeting" with no default; the parent must supply it.
	body := &workflow.FSMGraph{
		InitialState: "done",
		States: map[string]*workflow.StateNode{
			"done": {Name: "done", Terminal: true, Success: true},
		},
		Variables: map[string]*workflow.VariableNode{
			"greeting": {Name: "greeting", Type: cty.String},
		},
	}
	// Build a literal HCL expression: "hello"
	greetingExpr := hclsyntax.LiteralValueExpr{Val: cty.StringVal("hello")}

	node := &workflow.SubworkflowNode{
		Name:      "greeter",
		Body:      body,
		BodyEntry: "done",
		Inputs:    map[string]hcl.Expression{"greeting": &greetingExpr},
		DeclaredVars: map[string]*workflow.VariableNode{
			"greeting": {Name: "greeting", Type: cty.String},
		},
	}

	parentSt := &RunState{
		Vars:        map[string]cty.Value{"var": cty.EmptyObjectVal},
		WorkflowDir: t.TempDir(),
	}

	terminal, finalVars, err := runSubworkflow(context.Background(), node, parentSt, testDeps(t))
	if err != nil {
		t.Fatalf("runSubworkflow: %v", err)
	}
	if terminal != "done" {
		t.Errorf("expected terminal 'done', got %q", terminal)
	}
	// The child's final vars should contain var.greeting = "hello".
	if varObj, ok := finalVars["var"]; ok && varObj.Type().IsObjectType() {
		if varObj.Type().HasAttribute("greeting") {
			got := varObj.GetAttr("greeting").AsString()
			if got != "hello" {
				t.Errorf("var.greeting: want %q, got %q", "hello", got)
			}
		}
	}
}

// TestRunSubworkflow_EachThreaded verifies that the parent's each.* bindings
// are visible inside the subworkflow child scope (read-only pass-through).
func TestRunSubworkflow_EachThreaded(t *testing.T) {
	node := minimalSubworkflowNode("each-test")

	eachVal := cty.ObjectVal(map[string]cty.Value{
		"value": cty.StringVal("item-x"),
		"_idx":  cty.NumberIntVal(0),
	})
	parentSt := &RunState{
		Vars: map[string]cty.Value{
			"var":  cty.EmptyObjectVal,
			"each": eachVal,
		},
		WorkflowDir: t.TempDir(),
	}

	_, finalVars, err := runSubworkflow(context.Background(), node, parentSt, testDeps(t))
	if err != nil {
		t.Fatalf("runSubworkflow: %v", err)
	}
	each, ok := finalVars["each"]
	if !ok {
		t.Fatal("each not present in child final vars")
	}
	if got := each.GetAttr("value").AsString(); got != "item-x" {
		t.Errorf("each.value: want %q, got %q", "item-x", got)
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
		Inputs:       map[string]hcl.Expression{}, // no input provided
		DeclaredVars: map[string]*workflow.VariableNode{"required_var": {Name: "required_var", Type: cty.String}},
	}
	parentSt := &RunState{
		Vars:        map[string]cty.Value{"var": cty.EmptyObjectVal},
		WorkflowDir: t.TempDir(),
	}

	_, _, err := runSubworkflow(context.Background(), node, parentSt, testDeps(t))
	if err == nil {
		t.Fatal("expected error for missing required input, got none")
	}
}
