package workflow

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/zclconf/go-cty/cty"
)

func TestBuildEvalContext_Empty(t *testing.T) {
	ctx := BuildEvalContext(map[string]cty.Value{})
	if ctx == nil {
		t.Fatal("nil eval context")
	}
	if _, ok := ctx.Variables["var"]; !ok {
		t.Error("missing 'var' in eval context")
	}
	if _, ok := ctx.Variables["steps"]; !ok {
		t.Error("missing 'steps' in eval context")
	}
}

func TestBuildEvalContext_WithVars(t *testing.T) {
	vars := map[string]cty.Value{
		"var":   cty.ObjectVal(map[string]cty.Value{"name": cty.StringVal("alice")}),
		"steps": cty.EmptyObjectVal,
	}
	ctx := BuildEvalContext(vars)
	varObj := ctx.Variables["var"]
	if !varObj.Type().HasAttribute("name") {
		t.Error("expected 'name' attribute in var object")
	}
	if varObj.GetAttr("name").AsString() != "alice" {
		t.Errorf("var.name = %q, want 'alice'", varObj.GetAttr("name").AsString())
	}
}

func TestCtyValueToString(t *testing.T) {
	cases := []struct {
		val  cty.Value
		want string
	}{
		{cty.StringVal("hello"), "hello"},
		{cty.NumberIntVal(42), "42"},
		{cty.True, "true"},
		{cty.False, "false"},
		{cty.NilVal, ""},
		{cty.NullVal(cty.String), ""},
	}
	for _, tc := range cases {
		got := CtyValueToString(tc.val)
		if got != tc.want {
			t.Errorf("CtyValueToString(%v) = %q, want %q", tc.val, got, tc.want)
		}
	}
}

func TestSeedVarsFromGraph_Defaults(t *testing.T) {
	g := &FSMGraph{
		Variables: map[string]*VariableNode{
			"x": {Name: "x", Type: cty.String, Default: cty.StringVal("foo")},
			"y": {Name: "y", Type: cty.String, Default: cty.NilVal},
		},
	}
	vars := SeedVarsFromGraph(g)
	varObj, ok := vars["var"]
	if !ok {
		t.Fatal("missing 'var' key")
	}
	if !varObj.Type().IsObjectType() {
		t.Fatal("'var' is not an object")
	}
	xVal := varObj.GetAttr("x")
	if xVal.AsString() != "foo" {
		t.Errorf("x = %q, want 'foo'", xVal.AsString())
	}
	// y has no default; should be NullVal
	yVal := varObj.GetAttr("y")
	if !yVal.IsNull() {
		t.Errorf("y should be null, got %v", yVal)
	}
}

func TestWithStepOutputs(t *testing.T) {
	vars := map[string]cty.Value{
		"var":   cty.EmptyObjectVal,
		"steps": cty.EmptyObjectVal,
	}
	updated := WithStepOutputs(vars, "step1", map[string]string{"stdout": "hello", "exit_code": "0"})
	stepsObj := updated["steps"]
	if !stepsObj.Type().IsObjectType() {
		t.Fatal("steps not an object")
	}
	step1Obj := stepsObj.GetAttr("step1")
	if !step1Obj.Type().IsObjectType() {
		t.Fatal("step1 not an object")
	}
	if step1Obj.GetAttr("stdout").AsString() != "hello" {
		t.Error("expected stdout='hello'")
	}
	// Add a second step and ensure step1 is preserved.
	updated2 := WithStepOutputs(updated, "step2", map[string]string{"result": "ok"})
	if !updated2["steps"].Type().HasAttribute("step1") {
		t.Error("step1 was lost after adding step2")
	}
}

func TestSerializeAndRestoreVarScope(t *testing.T) {
	vars := map[string]cty.Value{
		"var":   cty.ObjectVal(map[string]cty.Value{"greeting": cty.StringVal("hi")}),
		"steps": cty.EmptyObjectVal,
	}
	vars = WithStepOutputs(vars, "build", map[string]string{"artifact": "app.bin"})

	scopeJSON, err := SerializeVarScope(vars)
	if err != nil {
		t.Fatalf("SerializeVarScope: %v", err)
	}
	if scopeJSON == "" {
		t.Fatal("expected non-empty scope JSON")
	}

	// Validate JSON structure.
	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(scopeJSON), &raw); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	steps, _ := raw["steps"].(map[string]interface{})
	build, _ := steps["build"].(map[string]interface{})
	if build["artifact"] != "app.bin" {
		t.Errorf("steps.build.artifact = %v, want 'app.bin'", build["artifact"])
	}

	g := &FSMGraph{
		Variables: map[string]*VariableNode{
			"greeting": {Name: "greeting", Type: cty.String, Default: cty.StringVal("hi")},
		},
	}
	restored, err := RestoreVarScope(scopeJSON, g)
	if err != nil {
		t.Fatalf("RestoreVarScope: %v", err)
	}
	stepsObj := restored["steps"]
	if !stepsObj.Type().HasAttribute("build") {
		t.Error("missing 'build' in restored steps")
	}
	artifact := stepsObj.GetAttr("build").GetAttr("artifact").AsString()
	if artifact != "app.bin" {
		t.Errorf("restored artifact = %q, want 'app.bin'", artifact)
	}
}

func TestRestoreVarScope_Empty(t *testing.T) {
	g := &FSMGraph{Variables: map[string]*VariableNode{}}
	vars, err := RestoreVarScope("", g)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := vars["var"]; !ok {
		t.Error("missing 'var' key")
	}
}

func TestResolveInputExprs_EachProducesPlannedMessage(t *testing.T) {
	// Parse a small HCL snippet that references each.value so we have a live
	// hcl.Expression to feed to ResolveInputExprs.
	src := `
workflow "test" {
  version       = "0.1"
  initial_state = "s"
  target_state  = "__done__"
  step "s" {
    adapter = "shell"
    input {
      command = "${each.value}"
    }
    outcome "success" { transition_to = "__done__" }
  }
  state "__done__" { terminal = true }
}
`
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags)
	}
	g, diags := Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags)
	}
	step, ok := g.Steps["s"]
	if !ok || len(step.InputExprs) == 0 {
		t.Fatal("expected InputExprs for step 's'")
	}

	vars := SeedVarsFromGraph(g)
	_, err := ResolveInputExprs(step.InputExprs, vars)
	if err == nil {
		t.Fatal("expected error for each.value outside for_each, got nil")
	}
	if !strings.Contains(err.Error(), "each is only valid inside for_each") {
		t.Errorf("error = %q, want 'each is only valid inside for_each'", err.Error())
	}
}
