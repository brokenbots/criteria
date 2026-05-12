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
	restored, _, err := RestoreVarScope(scopeJSON, g)
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
	vars, _, err := RestoreVarScope("", g)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := vars["var"]; !ok {
		t.Error("missing 'var' key")
	}
}

func TestResolveInputExprs_EachProducesPlannedMessage(t *testing.T) {
	// W08: each.* outside a for_each iteration body is caught at compile time.
	// This test was originally written to test runtime behavior (ResolveInputExprs
	// returning "each is only valid inside for_each"), but compile-time validation
	// is the correct enforcement point.
	src := `
workflow "test" {
  version       = "0.1"
  initial_state = "s"
  target_state  = "__done__"
}

adapter "shell" "default" {}
step "s" {
  target = adapter.shell.default
  input {
    command = "${each.value}"
  }
  outcome "success" { next = "__done__" }
}
state "__done__" { terminal = true }
`
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags)
	}
	_, diags = Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for each.value outside for_each, got none")
	}
	if !strings.Contains(diags.Error(), "for_each, count, or parallel") {
		t.Errorf("compile error = %q, want message about each.* scope", diags.Error())
	}
}

// TestSerializeVarScope_WithIterCursor verifies that an IterCursor round-trips
// through SerializeVarScope → RestoreVarScope. Items must NOT be persisted
// (they are re-evaluated from the workflow expression on re-entry, W07/W10).
func TestSerializeVarScope_WithIterCursor(t *testing.T) {
	g := &FSMGraph{
		Variables: map[string]*VariableNode{},
	}
	vars := SeedVarsFromGraph(g)

	stack := []IterCursor{{
		StepName:   "each_item",
		Index:      2,
		AnyFailed:  true,
		InProgress: true,
		Items:      nil, // never set — intentionally omitted from serialization
	}}

	scopeJSON, err := SerializeVarScope(vars, stack)
	if err != nil {
		t.Fatalf("SerializeVarScope: %v", err)
	}
	if scopeJSON == "" {
		t.Fatal("expected non-empty scope JSON")
	}

	restoredVars, restoredStack, err := RestoreVarScope(scopeJSON, g)
	if err != nil {
		t.Fatalf("RestoreVarScope: %v", err)
	}
	if restoredVars == nil {
		t.Fatal("expected non-nil vars")
	}
	if len(restoredStack) == 0 {
		t.Fatal("expected non-empty cursor stack after restore")
	}
	c := restoredStack[0]
	if c.StepName != "each_item" {
		t.Errorf("StepName = %q; want \"each_item\"", c.StepName)
	}
	if c.Index != 2 {
		t.Errorf("Index = %d; want 2", c.Index)
	}
	if !c.AnyFailed {
		t.Error("AnyFailed = false; want true")
	}
	if !c.InProgress {
		t.Error("InProgress = false; want true")
	}
	// Items must NOT be persisted — always nil after restore.
	if c.Items != nil {
		t.Errorf("Items = %v; want nil (Items are re-evaluated on re-entry)", c.Items)
	}
}

// TestBuildEvalContext_ExposesLocals verifies that BuildEvalContextWithOpts
// makes compiled local values accessible via the "local" namespace.
func TestBuildEvalContext_ExposesLocals(t *testing.T) {
	vars := map[string]cty.Value{
		"var":   cty.EmptyObjectVal,
		"steps": cty.EmptyObjectVal,
		"local": cty.ObjectVal(map[string]cty.Value{
			"greeting": cty.StringVal("Hello, world!"),
		}),
	}
	ctx := BuildEvalContextWithOpts(vars, DefaultFunctionOptions(""))
	if ctx == nil {
		t.Fatal("nil eval context")
	}
	localObj, ok := ctx.Variables["local"]
	if !ok {
		t.Fatal("'local' namespace missing from eval context")
	}
	if !localObj.Type().HasAttribute("greeting") {
		t.Fatal("local.greeting not in eval context")
	}
	if localObj.GetAttr("greeting").AsString() != "Hello, world!" {
		t.Errorf("local.greeting = %q, want 'Hello, world!'", localObj.GetAttr("greeting").AsString())
	}
}

// TestApplyVarOverrides_PreservesLocals verifies that applying CLI var overrides
// does not drop the compiled locals namespace.
func TestApplyVarOverrides_PreservesLocals(t *testing.T) {
	g := &FSMGraph{
		Variables: map[string]*VariableNode{
			"name": {Name: "name", Type: cty.String, Default: cty.StringVal("world")},
		},
		Locals: map[string]*LocalNode{
			"greeting": {Name: "greeting", Type: cty.String, Value: cty.StringVal("Hello, world!")},
		},
	}
	vars := SeedVarsFromGraph(g)
	vars["local"] = SeedLocalsFromGraph(g)

	after := ApplyVarOverrides(g, vars, map[string]string{"name": "alice"})

	if _, ok := after["local"]; !ok {
		t.Fatal("ApplyVarOverrides dropped vars[\"local\"]; expected it to be preserved")
	}
	localObj := after["local"]
	if !localObj.Type().HasAttribute("greeting") {
		t.Fatal("local.greeting not present after ApplyVarOverrides")
	}
	if localObj.GetAttr("greeting").AsString() != "Hello, world!" {
		t.Errorf("local.greeting = %q, want 'Hello, world!'", localObj.GetAttr("greeting").AsString())
	}
	// Var override must still have been applied.
	varObj := after["var"]
	if varObj.GetAttr("name").AsString() != "alice" {
		t.Errorf("var.name = %q, want 'alice'", varObj.GetAttr("name").AsString())
	}
}

// TestApplyVarOverrides_NoOverrides_PreservesLocals verifies that calling
// ApplyVarOverrides with an empty overrides map also preserves locals.
func TestApplyVarOverrides_NoOverrides_PreservesLocals(t *testing.T) {
	g := &FSMGraph{
		Variables: map[string]*VariableNode{},
		Locals: map[string]*LocalNode{
			"x": {Name: "x", Type: cty.String, Value: cty.StringVal("42")},
		},
	}
	vars := SeedVarsFromGraph(g)
	vars["local"] = SeedLocalsFromGraph(g)

	// No overrides — the function short-circuits and returns vars unchanged.
	after := ApplyVarOverrides(g, vars, nil)

	if _, ok := after["local"]; !ok {
		t.Fatal("ApplyVarOverrides(nil overrides) dropped vars[\"local\"]")
	}
}

// TestWithEachBinding_SetsFields verifies that WithEachBinding correctly
// populates all each.* fields from the provided EachBinding.
func TestWithEachBinding_SetsFields(t *testing.T) {
	base := map[string]cty.Value{
		"var":   cty.EmptyObjectVal,
		"steps": cty.EmptyObjectVal,
	}
	b := &EachBinding{
		Value: cty.StringVal("item"),
		Key:   cty.StringVal("k"),
		Index: 1,
		Total: 3,
		First: false,
		Last:  false,
		Prev:  cty.NilVal,
	}
	got := WithEachBinding(base, b)

	each, ok := got["each"]
	if !ok {
		t.Fatal("WithEachBinding: each not set")
	}
	if v := each.GetAttr("value").AsString(); v != "item" {
		t.Errorf("each.value: want 'item', got %q", v)
	}
	if k := each.GetAttr("key").AsString(); k != "k" {
		t.Errorf("each.key: want 'k', got %q", k)
	}
	idx, _ := each.GetAttr("_idx").AsBigFloat().Int64()
	if idx != 1 {
		t.Errorf("each._idx: want 1, got %d", idx)
	}
	total, _ := each.GetAttr("_total").AsBigFloat().Int64()
	if total != 3 {
		t.Errorf("each._total: want 3, got %d", total)
	}
}

// TestWithEachBinding_NilKey uses a nil key and verifies the fallback index
// string is used as each.key.
func TestWithEachBinding_NilKey(t *testing.T) {
	base := map[string]cty.Value{"var": cty.EmptyObjectVal}
	b := &EachBinding{
		Value: cty.StringVal("x"),
		Key:   cty.NilVal, // should fall back to "0"
		Index: 0,
		Total: 1,
		First: true,
		Last:  true,
		Prev:  cty.NilVal,
	}
	got := WithEachBinding(base, b)
	each := got["each"]
	if k := each.GetAttr("key").AsString(); k != "0" {
		t.Errorf("each.key fallback: want '0', got %q", k)
	}
}

// TestClearEachBinding_RemovesEach verifies that ClearEachBinding drops the
// each key from the vars map and preserves all other keys.
func TestClearEachBinding_RemovesEach(t *testing.T) {
	vars := map[string]cty.Value{
		"var":  cty.EmptyObjectVal,
		"each": cty.EmptyObjectVal,
	}
	got := ClearEachBinding(vars)
	if _, ok := got["each"]; ok {
		t.Fatal("ClearEachBinding: each still present")
	}
	if _, ok := got["var"]; !ok {
		t.Fatal("ClearEachBinding: var was dropped")
	}
}

// TestClearEachBinding_NoEach verifies that ClearEachBinding is a no-op when
// the each key is absent.
func TestClearEachBinding_NoEach(t *testing.T) {
	vars := map[string]cty.Value{"var": cty.EmptyObjectVal}
	got := ClearEachBinding(vars)
	if got == nil {
		t.Fatal("ClearEachBinding(no each) returned nil")
	}
	if _, ok := got["var"]; !ok {
		t.Fatal("ClearEachBinding: var was dropped when each was absent")
	}
}

// TestWithIndexedStepOutput_SingleIteration verifies that WithIndexedStepOutput
// stores the first iteration output under steps[stepName]["0"].
func TestWithIndexedStepOutput_SingleIteration(t *testing.T) {
	vars := map[string]cty.Value{
		"var":   cty.EmptyObjectVal,
		"steps": cty.EmptyObjectVal,
	}
	got := WithIndexedStepOutput(vars, "run", cty.NumberIntVal(0), map[string]string{"result": "hello"})
	steps, ok := got["steps"]
	if !ok {
		t.Fatal("steps key missing after WithIndexedStepOutput")
	}
	if !steps.Type().IsObjectType() {
		t.Fatalf("steps is not an object type: %s", steps.Type().FriendlyName())
	}
	if !steps.Type().HasAttribute("run") {
		t.Fatal("steps.run missing")
	}
	runEntry := steps.GetAttr("run")
	if !runEntry.Type().HasAttribute("0") {
		t.Fatal("steps.run[0] missing")
	}
	if v := runEntry.GetAttr("0").GetAttr("result").AsString(); v != "hello" {
		t.Errorf("steps.run[0].result: want 'hello', got %q", v)
	}
}

// TestWithIndexedStepOutput_NilVarsInitializes verifies that a nil vars map is
// treated as empty rather than panicking.
func TestWithIndexedStepOutput_NilVarsInitializes(t *testing.T) {
	got := WithIndexedStepOutput(nil, "step1", cty.NumberIntVal(0), map[string]string{"x": "1"})
	if got == nil {
		t.Fatal("WithIndexedStepOutput(nil vars) returned nil")
	}
	if _, ok := got["steps"]; !ok {
		t.Fatal("steps key missing")
	}
}

// TestVarScope_RoundTrip_WhileCursor verifies that an IterCursor with
// Total=-1 (the while sentinel) round-trips through SerializeVarScope →
// RestoreVarScope and is correctly identified by IsWhile() after restore.
func TestVarScope_RoundTrip_WhileCursor(t *testing.T) {
	g := &FSMGraph{Variables: map[string]*VariableNode{}}
	vars := SeedVarsFromGraph(g)

	stack := []IterCursor{{
		StepName:   "drain",
		Index:      3,
		Total:      -1, // while sentinel
		InProgress: true,
	}}

	scopeJSON, err := SerializeVarScope(vars, stack)
	if err != nil {
		t.Fatalf("SerializeVarScope: %v", err)
	}
	if scopeJSON == "" {
		t.Fatal("expected non-empty scope JSON")
	}

	_, restoredStack, err := RestoreVarScope(scopeJSON, g)
	if err != nil {
		t.Fatalf("RestoreVarScope: %v", err)
	}
	if len(restoredStack) == 0 {
		t.Fatal("expected non-empty cursor stack after restore")
	}
	c := restoredStack[0]
	if c.Total != -1 {
		t.Errorf("Total = %d; want -1 (while sentinel)", c.Total)
	}
	if !c.IsWhile() {
		t.Error("IsWhile() = false; want true for Total=-1")
	}
	if c.StepName != "drain" {
		t.Errorf("StepName = %q; want \"drain\"", c.StepName)
	}
	if c.Index != 3 {
		t.Errorf("Index = %d; want 3", c.Index)
	}
	if !c.InProgress {
		t.Error("InProgress = false; want true")
	}
}
