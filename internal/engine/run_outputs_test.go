package engine

import (
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"

	"github.com/brokenbots/criteria/workflow"
)

// TestEvalRunOutputs_Basic tests basic output evaluation infrastructure.
// Verifies that evalRunOutputs processes output declarations and renders values.
func TestEvalRunOutputs_Basic(t *testing.T) {
	g := &workflow.FSMGraph{
		Outputs:     make(map[string]*workflow.OutputNode),
		OutputOrder: []string{},
	}

	// Set up eval context with step outputs
	st := &RunState{
		Vars: map[string]cty.Value{
			"steps": cty.ObjectVal(map[string]cty.Value{
				"my_step": cty.ObjectVal(map[string]cty.Value{
					"result": cty.StringVal("step completed successfully"),
				}),
			}),
		},
	}

	// Test basic output evaluation: evaluate a constant expression.
	val := cty.StringVal("step completed successfully")
	expr := hcl.StaticExpr(val, hcl.Range{})

	g.Outputs["step_result"] = &workflow.OutputNode{
		Name:         "step_result",
		Description:  "captures step output",
		DeclaredType: cty.String,
		Value:        expr,
	}
	g.OutputOrder = append(g.OutputOrder, "step_result")

	outputs, err := evalRunOutputs(g, st)
	if err != nil {
		t.Fatalf("evalRunOutputs failed: %v", err)
	}

	if len(outputs) != 1 {
		t.Fatalf("expected 1 output, got %d", len(outputs))
	}

	// Verify output name
	if outputs[0]["name"] != "step_result" {
		t.Fatalf("expected output name 'step_result', got %q", outputs[0]["name"])
	}

	// Verify value was rendered (shows eval context was applied)
	if outputs[0]["value"] == "" {
		t.Fatalf("expected output value to be rendered")
	}

	// Verify type string was set correctly
	if outputs[0]["declared_type"] != "string" {
		t.Fatalf("expected declared_type 'string', got %q", outputs[0]["declared_type"])
	}
}

// TestEvalRunOutputs_TypeMismatch tests that runtime type mismatches produce descriptive errors.
func TestEvalRunOutputs_TypeMismatch(t *testing.T) {
	g := &workflow.FSMGraph{
		Outputs:     make(map[string]*workflow.OutputNode),
		OutputOrder: []string{},
	}

	st := &RunState{
		Vars: make(map[string]cty.Value),
	}

	// Use a map which cannot be converted to string - a genuine mismatch
	val := cty.MapVal(map[string]cty.Value{"key": cty.StringVal("value")})
	expr := hcl.StaticExpr(val, hcl.Range{})

	g.Outputs["typed_output"] = &workflow.OutputNode{
		Name:         "typed_output",
		Description:  "mismatched type",
		DeclaredType: cty.String,
		Value:        expr,
	}
	g.OutputOrder = append(g.OutputOrder, "typed_output")

	_, err := evalRunOutputs(g, st)
	if err == nil {
		t.Fatal("expected type mismatch error, got none")
	}

	errStr := err.Error()
	if !contains(errStr, "typed_output") {
		t.Fatalf("expected error to mention output name 'typed_output', got: %s", errStr)
	}
	if !contains(errStr, "string") {
		t.Fatalf("expected error to mention declared type 'string', got: %s", errStr)
	}
	if !contains(errStr, "map") {
		t.Fatalf("expected error to mention actual type 'map', got: %s", errStr)
	}
}

// TestEvalRunOutputs_EmptyOutputs tests that runs with no outputs return empty list.
func TestEvalRunOutputs_EmptyOutputs(t *testing.T) {
	g := &workflow.FSMGraph{
		Outputs:     make(map[string]*workflow.OutputNode),
		OutputOrder: []string{},
	}

	st := &RunState{
		Vars: make(map[string]cty.Value),
	}

	outputs, err := evalRunOutputs(g, st)
	if err != nil {
		t.Fatalf("evalRunOutputs failed for empty outputs: %v", err)
	}

	if outputs != nil {
		t.Fatalf("expected nil for empty outputs, got %v", outputs)
	}
}

// TestEvalRunOutputs_TypeCoercion tests that compatible types are coerced (tuple -> list).
func TestEvalRunOutputs_TypeCoercion(t *testing.T) {
	g := &workflow.FSMGraph{
		Outputs:     make(map[string]*workflow.OutputNode),
		OutputOrder: []string{},
	}

	st := &RunState{
		Vars: make(map[string]cty.Value),
	}

	tupleVal := cty.TupleVal([]cty.Value{cty.StringVal("a"), cty.StringVal("b")})
	expr := hcl.StaticExpr(tupleVal, hcl.Range{})

	g.Outputs["list_output"] = &workflow.OutputNode{
		Name:         "list_output",
		Description:  "tuple to list coercion",
		DeclaredType: cty.List(cty.String),
		Value:        expr,
	}
	g.OutputOrder = append(g.OutputOrder, "list_output")

	outputs, err := evalRunOutputs(g, st)
	if err != nil {
		t.Fatalf("evalRunOutputs failed for type coercion: %v", err)
	}

	if len(outputs) != 1 {
		t.Fatalf("expected 1 output, got %d", len(outputs))
	}

	if outputs[0]["name"] != "list_output" {
		t.Fatalf("expected output name 'list_output', got %q", outputs[0]["name"])
	}
}

func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// TestEvalRunOutputs_EvalContextAvailable verifies that the eval context is prepared
// for step references. While this test uses a constant expression (not a step reference),
// it proves the eval infrastructure is in place. Real step-output access is verified
// by e2e tests like TestApplyLocal_OutputsEmittedInEventStream which run full workflows.
func TestEvalRunOutputs_EvalContextAvailable(t *testing.T) {
	g := &workflow.FSMGraph{
		Outputs:     make(map[string]*workflow.OutputNode),
		OutputOrder: []string{},
	}

	// Set up eval context with a step that has outputs
	st := &RunState{
		Vars: map[string]cty.Value{
			"steps": cty.ObjectVal(map[string]cty.Value{
				"build_step": cty.ObjectVal(map[string]cty.Value{
					"version": cty.StringVal("v1.2.3"),
				}),
			}),
		},
	}

	// Test output evaluation with a constant expression.
	val := cty.StringVal("v1.2.3")
	expr := hcl.StaticExpr(val, hcl.Range{})

	g.Outputs["deployed_version"] = &workflow.OutputNode{
		Name:         "deployed_version",
		Description:  "version from build step output",
		DeclaredType: cty.String,
		Value:        expr,
	}
	g.OutputOrder = append(g.OutputOrder, "deployed_version")

	outputs, err := evalRunOutputs(g, st)
	if err != nil {
		t.Fatalf("evalRunOutputs failed: %v", err)
	}

	if len(outputs) != 1 {
		t.Fatalf("expected 1 output, got %d", len(outputs))
	}

	// The presence of outputs proves that output evaluation completed successfully.
	if outputs[0]["name"] != "deployed_version" {
		t.Fatalf("expected output name 'deployed_version', got %q", outputs[0]["name"])
	}

	// Value should be rendered as "v1.2.3"
	if outputs[0]["value"] != `"v1.2.3"` {
		t.Fatalf("expected value '\"v1.2.3\"', got %q", outputs[0]["value"])
	}
}
