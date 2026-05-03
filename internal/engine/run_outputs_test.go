package engine

import (
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"

	"github.com/brokenbots/criteria/workflow"
)

// TestEvalRunOutputs_Basic tests basic output evaluation with step references.
// Verifies that evalRunOutputs correctly evaluates parsed HCL expressions against the eval context.
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

	// Use a real parsed HCL expression so the eval context is actually consulted.
	expr, parseDiags := hclsyntax.ParseExpression(
		[]byte(`steps.my_step.result`),
		"test.hcl",
		hcl.Pos{Line: 1, Column: 1},
	)
	if parseDiags.HasErrors() {
		t.Fatalf("parse expr: %v", parseDiags)
	}

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

	// The key assertion: the rendered value must equal the seeded steps.my_step.result.
	// This proves steps.* is accessible in the eval context.
	// renderCtyValue JSON-marshals the cty value, so a string is wrapped in quotes.
	if outputs[0]["value"] != `"step completed successfully"` {
		t.Fatalf("expected rendered value %q (proves steps.* traversal), got %q", `"step completed successfully"`, outputs[0]["value"])
	}

	// Verify output name
	if outputs[0]["name"] != "step_result" {
		t.Fatalf("expected output name 'step_result', got %q", outputs[0]["name"])
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

// TestEvalRunOutputs_EvalContextAvailable verifies that step references work in outputs.
// Tests that evalRunOutputs correctly evaluates parsed HCL expressions that traverse steps.*.
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

	// Use a real parsed HCL expression to reference steps.build_step.version.
	expr, parseDiags := hclsyntax.ParseExpression(
		[]byte(`steps.build_step.version`),
		"test.hcl",
		hcl.Pos{Line: 1, Column: 1},
	)
	if parseDiags.HasErrors() {
		t.Fatalf("parse expr: %v", parseDiags)
	}

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

	// The key assertion: the rendered value must equal the seeded steps.build_step.version.
	// This proves steps.* is accessible in the eval context.
	if outputs[0]["value"] != `"v1.2.3"` {
		t.Fatalf("expected rendered value %q (proves steps.* traversal), got %q", `"v1.2.3"`, outputs[0]["value"])
	}

	// Verify output name
	if outputs[0]["name"] != "deployed_version" {
		t.Fatalf("expected output name 'deployed_version', got %q", outputs[0]["name"])
	}
}
