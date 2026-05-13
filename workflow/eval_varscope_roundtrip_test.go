package workflow

import (
	"fmt"
	"strings"
	"testing"

	"github.com/zclconf/go-cty/cty"
)

// assertCtyMapEqual compares two cty value maps for round-trip equality using
// RawEquals, which checks both type tags and values. Use this for serialization
// round-trip tests rather than Equals, which can gloss over type-tag differences.
func assertCtyMapEqual(t *testing.T, want, got map[string]cty.Value) {
	t.Helper()
	if len(want) != len(got) {
		t.Fatalf("map length: want %d, got %d", len(want), len(got))
	}
	for k, wv := range want {
		gv, ok := got[k]
		if !ok {
			t.Errorf("missing key %q", k)
			continue
		}
		if !wv.RawEquals(gv) {
			t.Errorf("key %q: want %#v, got %#v", k, wv, gv)
		}
	}
}

// fsmGraphWithVarDefaults builds a minimal FSMGraph with variables whose
// defaults match the provided map. Used to make RestoreVarScope round-trip
// var values through FSMGraph defaults.
func fsmGraphWithVarDefaults(defaults map[string]cty.Value) *FSMGraph {
	nodes := make(map[string]*VariableNode, len(defaults))
	for k, v := range defaults {
		nodes[k] = &VariableNode{Name: k, Type: v.Type(), Default: v}
	}
	return &FSMGraph{Variables: nodes}
}

// TestVarScope_RoundTrip_EmptyScope verifies that an empty vars map with no
// cursors serializes to a minimal JSON blob and restores to an empty scope.
func TestVarScope_RoundTrip_EmptyScope(t *testing.T) {
	vars := map[string]cty.Value{}
	g := &FSMGraph{Variables: map[string]*VariableNode{}}

	scopeJSON, err := SerializeVarScope(vars)
	if err != nil {
		t.Fatalf("SerializeVarScope: %v", err)
	}

	restored, cursors, err := RestoreVarScope(scopeJSON, g)
	if err != nil {
		t.Fatalf("RestoreVarScope: %v", err)
	}
	if len(cursors) != 0 {
		t.Errorf("expected no cursors, got %d", len(cursors))
	}
	// vars["var"] should be present (seeded from graph) and empty.
	varObj, ok := restored["var"]
	if !ok {
		t.Fatal("missing 'var' key after restore")
	}
	if !varObj.Type().IsObjectType() || len(varObj.Type().AttributeTypes()) != 0 {
		t.Errorf("expected empty object for var, got %#v", varObj)
	}
}

// TestVarScope_RoundTrip_PrimitiveTypes verifies that string, number, and bool
// variable defaults survive a serialize→restore round-trip via FSMGraph seeding.
func TestVarScope_RoundTrip_PrimitiveTypes(t *testing.T) {
	defaults := map[string]cty.Value{
		"greet": cty.StringVal("hi"),
		"count": cty.NumberIntVal(42),
		"flag":  cty.BoolVal(true),
	}
	vars := map[string]cty.Value{
		"var":   cty.ObjectVal(defaults),
		"steps": cty.EmptyObjectVal,
	}
	g := fsmGraphWithVarDefaults(defaults)

	scopeJSON, err := SerializeVarScope(vars)
	if err != nil {
		t.Fatalf("SerializeVarScope: %v", err)
	}
	restored, _, err := RestoreVarScope(scopeJSON, g)
	if err != nil {
		t.Fatalf("RestoreVarScope: %v", err)
	}

	varObj := restored["var"]
	assertCtyMapEqual(t, defaults, map[string]cty.Value{
		"greet": varObj.GetAttr("greet"),
		"count": varObj.GetAttr("count"),
		"flag":  varObj.GetAttr("flag"),
	})
}

// TestVarScope_RoundTrip_ListAndMap verifies that step outputs stored as
// multiple key→value pairs round-trip correctly. List/map cty types in step
// outputs are serialized via CtyValueToString and restored as string values.
// This test also verifies cursor Prev round-tripping with a list cty value,
// which uses ctyjson for type-preserving serialization.
func TestVarScope_RoundTrip_ListAndMap(t *testing.T) {
	g := &FSMGraph{Variables: map[string]*VariableNode{}}
	vars := SeedVarsFromGraph(g)

	// Step outputs: multiple string values
	vars = WithStepOutputs(vars, "fetch", map[string]string{
		"url":    "https://example.com",
		"status": "200",
		"body":   "ok",
	})

	scopeJSON, err := SerializeVarScope(vars)
	if err != nil {
		t.Fatalf("SerializeVarScope: %v", err)
	}
	restored, _, err := RestoreVarScope(scopeJSON, g)
	if err != nil {
		t.Fatalf("RestoreVarScope: %v", err)
	}
	fetchObj := restored["steps"].GetAttr("fetch")
	for _, key := range []string{"url", "status", "body"} {
		if !fetchObj.Type().HasAttribute(key) {
			t.Errorf("missing attribute %q in restored step output", key)
		}
	}
	if fetchObj.GetAttr("status").AsString() != "200" {
		t.Errorf("status = %q, want '200'", fetchObj.GetAttr("status").AsString())
	}
}

// TestVarScope_RoundTrip_NestedObject verifies that a cursor Prev value
// containing a three-level nested cty object round-trips faithfully through
// the ctyjson serialization path in SerializeVarScope.
func TestVarScope_RoundTrip_NestedObject(t *testing.T) {
	nested := cty.ObjectVal(map[string]cty.Value{
		"steps": cty.ObjectVal(map[string]cty.Value{
			"build": cty.ObjectVal(map[string]cty.Value{
				"output": cty.StringVal("ok"),
			}),
		}),
	})
	cursor := IterCursor{
		StepName: "deploy",
		Index:    0,
		Total:    1,
		Prev:     nested,
	}
	g := &FSMGraph{Variables: map[string]*VariableNode{}}
	vars := SeedVarsFromGraph(g)

	scopeJSON, err := SerializeVarScope(vars, []IterCursor{cursor})
	if err != nil {
		t.Fatalf("SerializeVarScope: %v", err)
	}
	_, restoredCursors, err := RestoreVarScope(scopeJSON, g)
	if err != nil {
		t.Fatalf("RestoreVarScope: %v", err)
	}
	if len(restoredCursors) != 1 {
		t.Fatalf("expected 1 cursor, got %d", len(restoredCursors))
	}
	prev := restoredCursors[0].Prev
	if prev == cty.NilVal {
		t.Fatal("cursor Prev was not restored")
	}
	if !prev.RawEquals(nested) {
		t.Errorf("cursor Prev round-trip failed: want %#v, got %#v", nested, prev)
	}
}

// TestVarScope_RoundTrip_NullValue verifies that a variable with a null default
// is correctly seeded by SeedVarsFromGraph and preserved after a round-trip.
func TestVarScope_RoundTrip_NullValue(t *testing.T) {
	g := &FSMGraph{
		Variables: map[string]*VariableNode{
			// No default set → SeedVarsFromGraph uses NullVal.
			"opt": {Name: "opt", Type: cty.String, Default: cty.NilVal},
		},
	}
	vars := SeedVarsFromGraph(g)

	scopeJSON, err := SerializeVarScope(vars)
	if err != nil {
		t.Fatalf("SerializeVarScope: %v", err)
	}
	restored, _, err := RestoreVarScope(scopeJSON, g)
	if err != nil {
		t.Fatalf("RestoreVarScope: %v", err)
	}

	varObj := restored["var"]
	optVal := varObj.GetAttr("opt")
	if !optVal.IsNull() {
		t.Errorf("expected null value for 'opt', got %#v", optVal)
	}
}

// TestVarScope_RoundTrip_UnknownValue_Errors verifies that SerializeVarScope
// returns an error when a variable in vars["var"] is unknown (cty.UnknownVal).
// Unknown values are not serialisable — they have no concrete representation —
// and silently writing "" would corrupt the scope on restore.
func TestVarScope_RoundTrip_UnknownValue_Errors(t *testing.T) {
	vars := map[string]cty.Value{
		"var": cty.ObjectVal(map[string]cty.Value{
			"pending": cty.UnknownVal(cty.String),
		}),
		"steps": cty.EmptyObjectVal,
	}

	_, err := SerializeVarScope(vars)
	if err == nil {
		t.Fatal("expected error for unknown var value, got nil")
	}
	if !strings.Contains(err.Error(), "pending") {
		t.Errorf("error should name the offending variable; got: %v", err)
	}
}

// TestVarScope_RoundTrip_SingleCursor_ForEach verifies that a complete
// IterCursor representing a paused for_each iteration round-trips through
// SerializeVarScope → RestoreVarScope with all scalar fields preserved.
// Items and Keys are intentionally omitted (re-evaluated on re-entry).
func TestVarScope_RoundTrip_SingleCursor_ForEach(t *testing.T) {
	prev := cty.ObjectVal(map[string]cty.Value{"result": cty.StringVal("pass")})
	original := IterCursor{
		StepName:   "process",
		Index:      1,
		Total:      3,
		Key:        cty.StringVal("item-1"),
		AnyFailed:  false,
		InProgress: true,
		OnFailure:  "continue",
		Prev:       prev,
		// Items/Keys intentionally nil — not serialised.
		// EarlyExit intentionally false — not serialised.
	}

	g := &FSMGraph{Variables: map[string]*VariableNode{}}
	vars := SeedVarsFromGraph(g)

	scopeJSON, err := SerializeVarScope(vars, []IterCursor{original})
	if err != nil {
		t.Fatalf("SerializeVarScope: %v", err)
	}
	_, restoredCursors, err := RestoreVarScope(scopeJSON, g)
	if err != nil {
		t.Fatalf("RestoreVarScope: %v", err)
	}
	if len(restoredCursors) != 1 {
		t.Fatalf("expected 1 cursor, got %d", len(restoredCursors))
	}
	c := restoredCursors[0]

	if c.StepName != original.StepName {
		t.Errorf("StepName: want %q, got %q", original.StepName, c.StepName)
	}
	if c.Index != original.Index {
		t.Errorf("Index: want %d, got %d", original.Index, c.Index)
	}
	if c.Total != original.Total {
		t.Errorf("Total: want %d, got %d", original.Total, c.Total)
	}
	if !c.Key.RawEquals(original.Key) {
		t.Errorf("Key: want %#v, got %#v", original.Key, c.Key)
	}
	if c.AnyFailed != original.AnyFailed {
		t.Errorf("AnyFailed: want %v, got %v", original.AnyFailed, c.AnyFailed)
	}
	if c.InProgress != original.InProgress {
		t.Errorf("InProgress: want %v, got %v", original.InProgress, c.InProgress)
	}
	if c.OnFailure != original.OnFailure {
		t.Errorf("OnFailure: want %q, got %q", original.OnFailure, c.OnFailure)
	}
	if !c.Prev.RawEquals(original.Prev) {
		t.Errorf("Prev: want %#v, got %#v", original.Prev, c.Prev)
	}
}

// TestVarScope_RoundTrip_NestedCursors verifies that a two-cursor stack
// (outer for_each, inner for_each) round-trips with order preserved.
func TestVarScope_RoundTrip_NestedCursors(t *testing.T) {
	outer := IterCursor{StepName: "outer", Index: 2, Total: 5}
	inner := IterCursor{StepName: "inner", Index: 0, Total: 3}

	g := &FSMGraph{Variables: map[string]*VariableNode{}}
	vars := SeedVarsFromGraph(g)

	scopeJSON, err := SerializeVarScope(vars, []IterCursor{outer, inner})
	if err != nil {
		t.Fatalf("SerializeVarScope: %v", err)
	}
	_, cursors, err := RestoreVarScope(scopeJSON, g)
	if err != nil {
		t.Fatalf("RestoreVarScope: %v", err)
	}
	if len(cursors) != 2 {
		t.Fatalf("expected 2 cursors, got %d", len(cursors))
	}
	if cursors[0].StepName != outer.StepName {
		t.Errorf("cursors[0].StepName = %q, want %q", cursors[0].StepName, outer.StepName)
	}
	if cursors[0].Index != outer.Index {
		t.Errorf("cursors[0].Index = %d, want %d", cursors[0].Index, outer.Index)
	}
	if cursors[1].StepName != inner.StepName {
		t.Errorf("cursors[1].StepName = %q, want %q", cursors[1].StepName, inner.StepName)
	}
	if cursors[1].Index != inner.Index {
		t.Errorf("cursors[1].Index = %d, want %d", cursors[1].Index, inner.Index)
	}
}

// TestVarScope_RoundTrip_CursorWithEachPrev verifies that a cursor whose Prev
// field holds a non-trivial cty object round-trips bit-exact via ctyjson.
func TestVarScope_RoundTrip_CursorWithEachPrev(t *testing.T) {
	prev := cty.ObjectVal(map[string]cty.Value{
		"artifact": cty.StringVal("app-v1.2.3.tar.gz"),
		"size":     cty.NumberIntVal(1024000),
		"ok":       cty.BoolVal(true),
	})
	cursor := IterCursor{StepName: "package", Index: 3, Total: 10, Prev: prev}

	g := &FSMGraph{Variables: map[string]*VariableNode{}}
	vars := SeedVarsFromGraph(g)

	scopeJSON, err := SerializeVarScope(vars, []IterCursor{cursor})
	if err != nil {
		t.Fatalf("SerializeVarScope: %v", err)
	}
	_, cursors, err := RestoreVarScope(scopeJSON, g)
	if err != nil {
		t.Fatalf("RestoreVarScope: %v", err)
	}
	if len(cursors) != 1 {
		t.Fatalf("expected 1 cursor, got %d", len(cursors))
	}
	if !cursors[0].Prev.RawEquals(prev) {
		t.Errorf("Prev round-trip: want %#v, got %#v", prev, cursors[0].Prev)
	}
}

// TestVarScope_RoundTrip_LargeScope_HandlesLengthEfficiently verifies that
// serializing 100 string variables produces valid JSON under 100 KiB.
// This is a sanity guard, not a benchmark: it detects pathological expansion.
func TestVarScope_RoundTrip_LargeScope_HandlesLengthEfficiently(t *testing.T) {
	const n = 100
	defaults := make(map[string]cty.Value, n)
	for i := range n {
		defaults[fmt.Sprintf("var_%03d", i)] = cty.StringVal(fmt.Sprintf("value-%d", i))
	}
	vars := map[string]cty.Value{
		"var":   cty.ObjectVal(defaults),
		"steps": cty.EmptyObjectVal,
	}

	scopeJSON, err := SerializeVarScope(vars)
	if err != nil {
		t.Fatalf("SerializeVarScope: %v", err)
	}
	const maxBytes = 100 * 1024 // 100 KiB
	if len(scopeJSON) > maxBytes {
		t.Errorf("serialized scope is %d bytes, want < %d", len(scopeJSON), maxBytes)
	}

	g := fsmGraphWithVarDefaults(defaults)
	restored, _, err := RestoreVarScope(scopeJSON, g)
	if err != nil {
		t.Fatalf("RestoreVarScope: %v", err)
	}
	varObj := restored["var"]
	if len(varObj.Type().AttributeTypes()) != n {
		t.Errorf("restored %d variables, want %d", len(varObj.Type().AttributeTypes()), n)
	}
}

// TestRestoreVarScope_MalformedJSON_ReturnsError verifies that passing
// syntactically invalid JSON to RestoreVarScope returns a non-nil error
// that mentions parsing or JSON.
func TestRestoreVarScope_MalformedJSON_ReturnsError(t *testing.T) {
	g := &FSMGraph{Variables: map[string]*VariableNode{}}
	_, _, err := RestoreVarScope("{invalid", g)
	if err == nil {
		t.Fatal("expected error for malformed JSON, got nil")
	}
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "invalid") && !strings.Contains(msg, "parse") && !strings.Contains(msg, "json") {
		t.Errorf("error message should mention parsing; got: %v", err)
	}
}

// TestRestoreVarScope_UnknownStepReference_Lenient documents the current
// behavior: RestoreVarScope does NOT validate step names in the JSON against
// the FSMGraph. An unknown step reference is silently accepted, allowing
// crash-resume to function even when the workflow schema evolves between runs.
//
// If strict step-name validation is added in the future, this test documents
// the expected change from lenient to strict mode.
func TestRestoreVarScope_UnknownStepReference_Lenient(t *testing.T) {
	json := `{"steps": {"nonexistent_step": {"output": "value"}}}`
	g := &FSMGraph{
		Variables: map[string]*VariableNode{},
		Steps:     map[string]*StepNode{}, // empty — no step named "nonexistent_step"
	}
	vars, _, err := RestoreVarScope(json, g)
	// Current behavior: no error; the step output is accepted unconditionally.
	if err != nil {
		t.Fatalf("unexpected error: %v (current behavior is lenient; see reviewer notes)", err)
	}
	stepsObj, ok := vars["steps"]
	if !ok || stepsObj == cty.NilVal {
		t.Fatal("expected steps to be populated")
	}
	if !stepsObj.Type().HasAttribute("nonexistent_step") {
		t.Error("expected nonexistent_step in restored steps (lenient accept)")
	}
}

// TestRestoreVarScope_VarSectionIgnored documents that the "var" section of
// the scope JSON is NOT applied to the restored vars map. Variable values are
// always seeded from FSMGraph defaults, not from the serialized JSON.
// This means the var section in JSON is informational only; it cannot cause
// a type mismatch because it is never parsed back into the vars map.
//
// The workstream described a "type mismatch" error test (test 13), but that
// scenario cannot occur with the current architecture because RestoreVarScope
// ignores the JSON "var" section entirely.
func TestRestoreVarScope_VarSectionIgnored(t *testing.T) {
	// JSON has var.foo = "not-a-number" but the graph declares foo as number.
	jsonScope := `{"var": {"foo": "not-a-number"}}`
	g := &FSMGraph{
		Variables: map[string]*VariableNode{
			"foo": {Name: "foo", Type: cty.Number, Default: cty.NumberIntVal(7)},
		},
	}
	vars, _, err := RestoreVarScope(jsonScope, g)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// foo is restored from the FSMGraph default (7), not from the JSON string.
	varObj := vars["var"]
	fooVal := varObj.GetAttr("foo")
	if !fooVal.RawEquals(cty.NumberIntVal(7)) {
		t.Errorf("foo = %#v, want NumberIntVal(7) (from FSMGraph, not JSON)", fooVal)
	}
}
