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
	updated := WithStepOutputs(vars, "step1", ctyStrs(map[string]string{"stdout": "hello", "exit_code": "0"}))
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
	updated2 := WithStepOutputs(updated, "step2", ctyStrs(map[string]string{"result": "ok"}))
	if !updated2["steps"].Type().HasAttribute("step1") {
		t.Error("step1 was lost after adding step2")
	}
}

func TestSerializeAndRestoreVarScope(t *testing.T) {
	vars := map[string]cty.Value{
		"var":   cty.ObjectVal(map[string]cty.Value{"greeting": cty.StringVal("hi")}),
		"steps": cty.EmptyObjectVal,
	}
	vars = WithStepOutputs(vars, "build", ctyStrs(map[string]string{"artifact": "app.bin"}))

	scopeJSON, err := SerializeVarScope(vars)
	if err != nil {
		t.Fatalf("SerializeVarScope: %v", err)
	}
	if scopeJSON == "" {
		t.Fatal("expected non-empty scope JSON")
	}

	// Validate JSON structure. Step outputs are persisted in the typed (cty-JSON)
	// form under "steps_typed" so structured/native types round-trip losslessly.
	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(scopeJSON), &raw); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	stepsTyped, ok := raw["steps_typed"].(string)
	if !ok {
		t.Fatalf("expected steps_typed string in scope JSON; got %v", raw)
	}
	var stepsDecoded map[string]map[string]interface{}
	if err := json.Unmarshal([]byte(stepsTyped), &stepsDecoded); err != nil {
		t.Fatalf("invalid steps_typed JSON: %v", err)
	}
	if stepsDecoded["build"]["artifact"] != "app.bin" {
		t.Errorf("steps.build.artifact = %v, want 'app.bin'", stepsDecoded["build"]["artifact"])
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
workflow {
  name = "test"
  version       = "0.1"
  initial_state = "s"
  target_state  = "__done__"
}

adapter "exec" "default" {}
step "s" {
  target = adapter.exec.default
  input {
    command = "${each.value}"
  }
  outcome "success" { next = step.__done__ }
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

// TestBuildEvalContext_ExposesPath verifies that BuildEvalContextWithOpts exposes
// the path namespace with workflow, root, and cwd values.
func TestBuildEvalContext_ExposesPath(t *testing.T) {
	vars := map[string]cty.Value{
		"var":   cty.EmptyObjectVal,
		"steps": cty.EmptyObjectVal,
	}
	opts := FunctionOptions{
		WorkflowDir: "/workflows",
		RootDir:     "/project",
		Cwd:         "/current",
	}
	ctx := BuildEvalContextWithOpts(vars, &opts)
	if ctx == nil {
		t.Fatal("nil eval context")
	}
	pathObj, ok := ctx.Variables["path"]
	if !ok {
		t.Fatal("'path' namespace missing from eval context")
	}
	if pathObj.GetAttr("workflow").AsString() != "/workflows" {
		t.Errorf("path.workflow = %q, want '/workflows'", pathObj.GetAttr("workflow").AsString())
	}
	if pathObj.GetAttr("root").AsString() != "/project" {
		t.Errorf("path.root = %q, want '/project'", pathObj.GetAttr("root").AsString())
	}
	if pathObj.GetAttr("cwd").AsString() != "/current" {
		t.Errorf("path.cwd = %q, want '/current'", pathObj.GetAttr("cwd").AsString())
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

	after, err := ApplyVarOverrides(g, vars, map[string]cty.Value{"name": cty.StringVal("alice")})
	if err != nil {
		t.Fatalf("ApplyVarOverrides: %v", err)
	}

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
	after, err := ApplyVarOverrides(g, vars, nil)
	if err != nil {
		t.Fatalf("ApplyVarOverrides(nil overrides): %v", err)
	}

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
	got := WithIndexedStepOutput(vars, "run", cty.NumberIntVal(0), ctyStrs(map[string]string{"result": "hello"}))
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
	got := WithIndexedStepOutput(nil, "step1", cty.NumberIntVal(0), ctyStrs(map[string]string{"x": "1"}))
	if got == nil {
		t.Fatal("WithIndexedStepOutput(nil vars) returned nil")
	}
	if _, ok := got["steps"]; !ok {
		t.Fatal("steps key missing")
	}
}

// TestVarScope_RoundTrip_WhileCursor verifies that an IterCursor with
// Total=-1 (the while sentinel) round-trips through SerializeVarScope →
// RestoreVarScope, including the Prev cty.Value that provides while._prev
// continuity across crash-resume.
func TestVarScope_RoundTrip_WhileCursor(t *testing.T) {
	g := &FSMGraph{Variables: map[string]*VariableNode{}}
	vars := SeedVarsFromGraph(g)

	prevVal := cty.ObjectVal(map[string]cty.Value{
		"result": cty.StringVal("processed"),
		"count":  cty.StringVal("7"),
	})

	stack := []IterCursor{{
		StepName:   "drain",
		Index:      3,
		Total:      -1, // while sentinel
		InProgress: true,
		AnyFailed:  false,
		OnFailure:  "continue",
		Prev:       prevVal,
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
	if c.OnFailure != "continue" {
		t.Errorf("OnFailure = %q; want \"continue\"", c.OnFailure)
	}

	// Assert Prev survived the round-trip — this is the while._prev contract.
	if c.Prev == cty.NilVal {
		t.Fatal("Prev = cty.NilVal after restore; while._prev contract broken on crash-resume")
	}
	if !c.Prev.Type().IsObjectType() {
		t.Fatalf("Prev.Type() = %s; want object type", c.Prev.Type().FriendlyName())
	}
	wantResult := "processed"
	if got := c.Prev.GetAttr("result"); got == cty.NilVal || got.AsString() != wantResult {
		t.Errorf("Prev.result = %v; want %q", got, wantResult)
	}
	wantCount := "7"
	if got := c.Prev.GetAttr("count"); got == cty.NilVal || got.AsString() != wantCount {
		t.Errorf("Prev.count = %v; want %q", got, wantCount)
	}
}

// TestApplyVarOverrides_ComplexTypes verifies that typed CLI overrides for
// list, map, and object variables are converted to the declared variable type.
func TestApplyVarOverrides_ComplexTypes(t *testing.T) {
	g := &FSMGraph{
		Variables: map[string]*VariableNode{
			"tags": {
				Name: "tags",
				Type: cty.List(cty.String),
				Default: cty.ListVal([]cty.Value{
					cty.StringVal("default"),
				}),
			},
			"labels": {
				Name: "labels",
				Type: cty.Map(cty.String),
			},
			"config": {
				Name: "config",
				Type: cty.Object(map[string]cty.Type{
					"enabled": cty.Bool,
					"retries": cty.Number,
				}),
			},
		},
	}

	overrides := map[string]cty.Value{
		"tags":   cty.TupleVal([]cty.Value{cty.StringVal("a"), cty.StringVal("b")}),
		"labels": cty.ObjectVal(map[string]cty.Value{"env": cty.StringVal("prod")}),
		"config": cty.ObjectVal(map[string]cty.Value{
			"enabled": cty.True,
			"retries": cty.NumberIntVal(3),
		}),
	}

	after, err := ApplyVarOverrides(g, SeedVarsFromGraph(g), overrides)
	if err != nil {
		t.Fatalf("ApplyVarOverrides: %v", err)
	}

	varObj := after["var"]
	if got := varObj.GetAttr("tags"); !got.Type().IsListType() {
		t.Errorf("tags type = %s; want list", got.Type().FriendlyName())
	} else if l := got.LengthInt(); l != 2 {
		t.Errorf("tags length = %d; want 2", l)
	}
	if got := varObj.GetAttr("labels"); !got.Type().IsMapType() {
		t.Errorf("labels type = %s; want map", got.Type().FriendlyName())
	} else if v := got.Index(cty.StringVal("env")); v.AsString() != "prod" {
		t.Errorf("labels.env = %q; want 'prod'", v.AsString())
	}
	if got := varObj.GetAttr("config"); !got.Type().IsObjectType() {
		t.Errorf("config type = %s; want object", got.Type().FriendlyName())
	} else if enabled := got.GetAttr("enabled"); !enabled.True() {
		t.Errorf("config.enabled = %v; want true", enabled)
	}
}

// TestApplyVarOverrides_ConversionError verifies that an incompatible override
// produces a clear error instead of silently being ignored.
func TestApplyVarOverrides_ConversionError(t *testing.T) {
	g := &FSMGraph{
		Variables: map[string]*VariableNode{
			"count": {Name: "count", Type: cty.Number},
		},
	}

	_, err := ApplyVarOverrides(g, SeedVarsFromGraph(g), map[string]cty.Value{
		"count": cty.StringVal("not-a-number"),
	})
	if err == nil {
		t.Fatal("expected error for incompatible override")
	}
	if !strings.Contains(err.Error(), `"count"`) {
		t.Errorf("error = %q, want it to name variable count", err.Error())
	}
}

// TestConvertVarOverrideValue_StringBindingToNumberRejectsGarbage verifies that
// a string value supplied to a number variable is converted strictly by
// convert.Convert on every override path (var-file, subworkflow bindings, and
// raw --var after parseOverrideString).
func TestConvertVarOverrideValue_StringBindingToNumberRejectsGarbage(t *testing.T) {
	node := &VariableNode{Name: "retries", Type: cty.Number}
	_, err := ConvertVarOverrideValue(cty.StringVal("3abc"), node)
	if err == nil {
		t.Fatal("expected error for invalid number string")
	}
}

// TestParseAndConvertVarOverride_NullAndUnknownStringReturnsError verifies that
// a null or unknown string value does not panic in ParseAndConvertVarOverride
// and instead returns a clear error from ConvertVarOverrideValue. This covers
// exported-package callers that may pass such values directly.
func TestParseAndConvertVarOverride_NullAndUnknownStringReturnsError(t *testing.T) {
	node := &VariableNode{Name: "count", Type: cty.Number}

	cases := []struct {
		name string
		val  cty.Value
	}{
		{"null string", cty.NullVal(cty.String)},
		{"unknown string", cty.UnknownVal(cty.String)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseAndConvertVarOverride(tc.val, node)
			if err == nil {
				t.Fatal("expected error for null/unknown string override")
			}
		})
	}
}

// TestApplyVarOverrides_NumberStrictParsing verifies that raw --var number
// values use strict parsing: partial input is rejected, decimals work, and
// arbitrarily large integers keep their exact precision.
func TestApplyVarOverrides_NumberStrictParsing(t *testing.T) {
	g := &FSMGraph{
		Variables: map[string]*VariableNode{
			"retries": {Name: "retries", Type: cty.Number},
			"big":     {Name: "big", Type: cty.Number},
		},
	}

	_, err := ApplyVarOverrides(g, SeedVarsFromGraph(g), map[string]cty.Value{
		"retries": cty.StringVal("3abc"),
	})
	if err == nil {
		t.Fatal("expected error for partial number override")
	}

	after, err := ApplyVarOverrides(g, SeedVarsFromGraph(g), map[string]cty.Value{
		"retries": cty.StringVal("1.5"),
		"big":     cty.StringVal("12345678901234567890"),
	})
	if err != nil {
		t.Fatalf("ApplyVarOverrides: %v", err)
	}

	retries := after["var"].GetAttr("retries").AsBigFloat()
	if retries.Text('f', -1) != "1.5" {
		t.Errorf("retries = %s, want 1.5", retries.Text('f', -1))
	}

	big := after["var"].GetAttr("big").AsBigFloat()
	if big.Text('f', -1) != "12345678901234567890" {
		t.Errorf("big = %s, want exact integer 12345678901234567890", big.Text('f', -1))
	}
}

// TestApplyVarOverrides_NumberToString verifies that a numeric override can be
// supplied to a string-typed variable and is converted to its string form,
// matching the common CLI pattern of passing an unquoted number for a string
// variable.
func TestApplyVarOverrides_NumberToString(t *testing.T) {
	g := &FSMGraph{
		Variables: map[string]*VariableNode{
			"name": {Name: "name", Type: cty.String},
		},
	}

	after, err := ApplyVarOverrides(g, SeedVarsFromGraph(g), map[string]cty.Value{
		"name": cty.NumberIntVal(42),
	})
	if err != nil {
		t.Fatalf("ApplyVarOverrides: %v", err)
	}
	if got := after["var"].GetAttr("name").AsString(); got != "42" {
		t.Errorf("var.name = %q, want %q", got, "42")
	}
}

// compileEvalTest compiles a minimal workflow from HCL for tests that need
// real type constraints (e.g. object optional defaults).
func compileEvalTest(t *testing.T, src string) *FSMGraph {
	t.Helper()
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	return g
}

// TestApplyVarOverrides_StringVariablesKeepRawText verifies that raw --var
// strings supplied for string-typed variables are preserved byte-for-byte,
// including JSON blobs, quoted strings, and leading zeros.
func TestApplyVarOverrides_StringVariablesKeepRawText(t *testing.T) {
	g := &FSMGraph{
		Variables: map[string]*VariableNode{
			"version": {Name: "version", Type: cty.String},
			"payload": {Name: "payload", Type: cty.String},
			"name":    {Name: "name", Type: cty.String},
			"id":      {Name: "id", Type: cty.String},
		},
	}

	after, err := ApplyVarOverrides(g, SeedVarsFromGraph(g), map[string]cty.Value{
		"version": cty.StringVal("1.0"),
		"payload": cty.StringVal(`{"a":1}`),
		"name":    cty.StringVal(`"quoted"`),
		"id":      cty.StringVal("007"),
	})
	if err != nil {
		t.Fatalf("ApplyVarOverrides: %v", err)
	}

	cases := map[string]string{
		"version": "1.0",
		"payload": `{"a":1}`,
		"name":    `"quoted"`,
		"id":      "007",
	}
	for name, want := range cases {
		got := after["var"].GetAttr(name).AsString()
		if got != want {
			t.Errorf("var.%s = %q, want %q", name, got, want)
		}
	}
}

// TestApplyVarOverrides_RawComplexTypesParsedAndConverted verifies that raw
// --var strings for list/map/object variables are HCL-parsed and converted to
// the declared type.
func TestApplyVarOverrides_RawComplexTypesParsedAndConverted(t *testing.T) {
	g := &FSMGraph{
		Variables: map[string]*VariableNode{
			"tags": {
				Name: "tags",
				Type: cty.List(cty.String),
			},
			"labels": {
				Name: "labels",
				Type: cty.Map(cty.String),
			},
			"config": {
				Name: "config",
				Type: cty.Object(map[string]cty.Type{
					"enabled": cty.Bool,
					"retries": cty.Number,
				}),
			},
		},
	}

	after, err := ApplyVarOverrides(g, SeedVarsFromGraph(g), map[string]cty.Value{
		"tags":   cty.StringVal(`["a","b"]`),
		"labels": cty.StringVal(`{env="prod"}`),
		"config": cty.StringVal(`{enabled=true, retries=3}`),
	})
	if err != nil {
		t.Fatalf("ApplyVarOverrides: %v", err)
	}

	tags := after["var"].GetAttr("tags")
	if !tags.Type().IsListType() {
		t.Errorf("tags type = %s; want list", tags.Type().FriendlyName())
	}
	if l := tags.LengthInt(); l != 2 {
		t.Errorf("tags length = %d; want 2", l)
	}

	labels := after["var"].GetAttr("labels")
	if !labels.Type().IsMapType() {
		t.Errorf("labels type = %s; want map", labels.Type().FriendlyName())
	}
	if got := labels.Index(cty.StringVal("env")).AsString(); got != "prod" {
		t.Errorf("labels.env = %q, want prod", got)
	}

	cfg := after["var"].GetAttr("config")
	if !cfg.Type().IsObjectType() {
		t.Errorf("config type = %s; want object", cfg.Type().FriendlyName())
	}
	if !cfg.GetAttr("enabled").True() {
		t.Errorf("config.enabled = %v, want true", cfg.GetAttr("enabled"))
	}
}

// TestApplyVarOverrides_ObjectOptionalDefaults verifies that object variables
// with optional() attributes receive their declared defaults when a CLI
// override omits those attributes.
func TestApplyVarOverrides_ObjectOptionalDefaults(t *testing.T) {
	g := compileEvalTest(t, `
workflow {
  name = "t"
  version = "0.1"
  initial_state = "s"
  target_state  = "s"
}
state "s" { terminal = true }
variable "cfg" {
  type = object({ a = string, b = optional(string, "x") })
}
`)

	after, err := ApplyVarOverrides(g, SeedVarsFromGraph(g), map[string]cty.Value{
		"cfg": cty.StringVal(`{a="1"}`),
	})
	if err != nil {
		t.Fatalf("ApplyVarOverrides: %v", err)
	}

	cfg := after["var"].GetAttr("cfg")
	if got := cfg.GetAttr("a").AsString(); got != "1" {
		t.Errorf("cfg.a = %q, want 1", got)
	}
	if got := cfg.GetAttr("b").AsString(); got != "x" {
		t.Errorf("cfg.b = %q, want x", got)
	}
}

// TestApplyVarOverrides_MalformedCollectionError verifies that a malformed
// collection literal supplied for a complex-typed variable surfaces a clear
// error naming the variable.
func TestApplyVarOverrides_MalformedCollectionError(t *testing.T) {
	g := compileEvalTest(t, `
workflow {
  name = "t"
  version = "0.1"
  initial_state = "s"
  target_state  = "s"
}
state "s" { terminal = true }
variable "tags" { type = list(string) }
`)

	_, err := ApplyVarOverrides(g, SeedVarsFromGraph(g), map[string]cty.Value{
		"tags": cty.StringVal(`["a",`),
	})
	if err == nil {
		t.Fatal("expected error for malformed list literal")
	}
	if !strings.Contains(err.Error(), `"tags"`) || !strings.Contains(err.Error(), "cannot parse") {
		t.Errorf("error = %q, want it to mention variable and parse failure", err.Error())
	}
}

// TestApplyVarOverrides_BoolStrictParsing verifies that raw --var bool values
// are parsed strictly: only true/false/1/0 are accepted, and other common
// spellings (yes, TRUE, empty) return an error naming the variable.
func TestApplyVarOverrides_BoolStrictParsing(t *testing.T) {
	g := &FSMGraph{
		Variables: map[string]*VariableNode{
			"enabled": {Name: "enabled", Type: cty.Bool},
		},
	}

	valid := map[string]bool{
		"true":  true,
		"1":     true,
		"false": false,
		"0":     false,
	}
	for raw, want := range valid {
		t.Run(raw, func(t *testing.T) {
			after, err := ApplyVarOverrides(g, SeedVarsFromGraph(g), map[string]cty.Value{
				"enabled": cty.StringVal(raw),
			})
			if err != nil {
				t.Fatalf("ApplyVarOverrides: %v", err)
			}
			got := after["var"].GetAttr("enabled").True()
			if got != want {
				t.Errorf("enabled = %v, want %v", got, want)
			}
		})
	}

	invalid := []string{"yes", "TRUE", ""}
	for _, raw := range invalid {
		t.Run("invalid_"+raw, func(t *testing.T) {
			_, err := ApplyVarOverrides(g, SeedVarsFromGraph(g), map[string]cty.Value{
				"enabled": cty.StringVal(raw),
			})
			if err == nil {
				t.Fatal("expected error for invalid bool override")
			}
			if !strings.Contains(err.Error(), `"enabled"`) {
				t.Errorf("error = %q, want it to name variable enabled", err.Error())
			}
		})
	}
}
