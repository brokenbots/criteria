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
// variable values survive a serialize→restore cycle. FSMGraph defaults are
// intentionally different from the runtime values to prove the JSON overlay
// wins over graph seeding, not just that types are preserved.
func TestVarScope_RoundTrip_PrimitiveTypes(t *testing.T) {
	vars := map[string]cty.Value{
		"var": cty.ObjectVal(map[string]cty.Value{
			"greet": cty.StringVal("hello world"),
			"count": cty.NumberFloatVal(99.0),
			"flag":  cty.BoolVal(false),
		}),
		"steps": cty.EmptyObjectVal,
	}
	// Defaults differ from runtime to prove overlay wins on restore.
	g := fsmGraphWithVarDefaults(map[string]cty.Value{
		"greet": cty.StringVal("default"),
		"count": cty.NumberFloatVal(0.0),
		"flag":  cty.BoolVal(true),
	})

	scopeJSON, err := SerializeVarScope(vars)
	if err != nil {
		t.Fatalf("SerializeVarScope: %v", err)
	}
	restored, _, err := RestoreVarScope(scopeJSON, g)
	if err != nil {
		t.Fatalf("RestoreVarScope: %v", err)
	}

	varObj := restored["var"]
	assertCtyMapEqual(t, map[string]cty.Value{
		"greet": cty.StringVal("hello world"),
		"count": cty.NumberFloatVal(99.0),
		"flag":  cty.BoolVal(false),
	}, map[string]cty.Value{
		"greet": varObj.GetAttr("greet"),
		"count": varObj.GetAttr("count"),
		"flag":  varObj.GetAttr("flag"),
	})
}

// TestVarScope_RoundTrip_ListAndMap exercises two scenarios:
//  1. Step outputs stored as multiple string key→value pairs (round-trips correctly).
//  2. List/map vars in vars["var"] — demonstrates the known limitation that
//     non-primitive cty types cannot be restored from the string serialization.
func TestVarScope_RoundTrip_ListAndMap(t *testing.T) {
	t.Run("step_outputs_round_trip", func(t *testing.T) {
		g := &FSMGraph{Variables: map[string]*VariableNode{}}
		vars := SeedVarsFromGraph(g)
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
	})

	t.Run("list_var_override_not_restored", func(t *testing.T) {
		// Known limitation: non-primitive (list, map, object) vars are not
		// restored from the JSON scope because CtyValueToString is lossy for
		// these types. A list var serializes to a comma-joined string and cannot
		// be recovered as a cty.List on restore. Complex-type vars always fall
		// back to the FSMGraph default value.
		//
		// This means CLI var overrides for list/map/object types (even if
		// ApplyVarOverrides were extended to support them) would be silently lost
		// on crash-resume. See [ARCH-REVIEW] in workstreams/test-02-hcl-parsing-eval-coverage.md.
		t.Skip("known limitation: list/map/object vars fall back to FSMGraph defaults on restore; " +
			"CtyValueToString is lossy for non-primitive types and overrides would be silently dropped. " +
			"Tracked as [ARCH-REVIEW] in workstreams/test-02-hcl-parsing-eval-coverage.md.")
	})
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
//
// Items and Keys are intentionally NOT serialized — they are re-evaluated from
// the workflow expression on re-entry, so the restored cursor has nil slices.
// EarlyExit is also not serialized — it is only meaningful during live execution
// and resets to false on resume. These omissions are by design; see the comment
// in SerializeVarScope.
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
		// Items/Keys intentionally set to demonstrate they are NOT preserved.
		Items:     []cty.Value{cty.StringVal("item-0"), cty.StringVal("item-1")},
		EarlyExit: true,
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

	// Items, Keys, and EarlyExit are intentionally NOT serialized.
	// Items is re-evaluated from the workflow expression on re-entry.
	// Keys is similarly re-evaluated. EarlyExit resets to false on resume.
	if len(c.Items) != 0 {
		t.Errorf("Items: want nil (not serialized by design), got %d items", len(c.Items))
	}
	if len(c.Keys) != 0 {
		t.Errorf("Keys: want nil (not serialized by design), got %d keys", len(c.Keys))
	}
	if c.EarlyExit {
		t.Errorf("EarlyExit: want false (not serialized by design, resets on resume), got true")
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
// serializing 100 string variables produces valid JSON under 100 KiB and that
// the restored scope has the correct number of variables. A spot-check confirms
// that the overlay wins over FSMGraph defaults.
// This is a sanity guard, not a benchmark: it detects pathological expansion.
func TestVarScope_RoundTrip_LargeScope_HandlesLengthEfficiently(t *testing.T) {
	const n = 100
	runtimeVals := make(map[string]cty.Value, n)
	fsmDefaults := make(map[string]cty.Value, n)
	for i := range n {
		key := fmt.Sprintf("var_%03d", i)
		runtimeVals[key] = cty.StringVal(fmt.Sprintf("runtime-%d", i))
		fsmDefaults[key] = cty.StringVal(fmt.Sprintf("default-%d", i))
	}
	vars := map[string]cty.Value{
		"var":   cty.ObjectVal(runtimeVals),
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

	g := fsmGraphWithVarDefaults(fsmDefaults)
	restored, _, err := RestoreVarScope(scopeJSON, g)
	if err != nil {
		t.Fatalf("RestoreVarScope: %v", err)
	}
	varObj := restored["var"]
	if len(varObj.Type().AttributeTypes()) != n {
		t.Errorf("restored %d variables, want %d", len(varObj.Type().AttributeTypes()), n)
	}
	// Spot-check: overlay must win over FSMGraph default.
	const spotIdx = 42
	spotKey := fmt.Sprintf("var_%03d", spotIdx)
	want := fmt.Sprintf("runtime-%d", spotIdx)
	got := varObj.GetAttr(spotKey).AsString()
	if got != want {
		t.Errorf("spot-check %s = %q, want %q (overlay must win over FSMGraph default)", spotKey, got, want)
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

// TestRestoreVarScope_UnknownStepReference_UnknownStepContract documents that
// the step-name validation contract for RestoreVarScope is unresolved. The
// workstream requires rejection of JSON step references absent from *FSMGraph,
// but the current implementation accepts them to tolerate crash-resume across
// schema drift. The architecture decision is tracked as
// [ARCH-REVIEW][major] Unknown-step restore contract in
// workstreams/test-02-hcl-parsing-eval-coverage.md.
func TestRestoreVarScope_UnknownStepReference_UnknownStepContract(t *testing.T) {
	t.Skip("step-name validation contract unresolved; " +
		"see [ARCH-REVIEW][major] Unknown-step restore contract in " +
		"workstreams/test-02-hcl-parsing-eval-coverage.md")
}

// TestRestoreVarScope_VarValues_RestoredFromJSON verifies that variable values
// in the JSON "var" section are overlaid onto FSMGraph defaults. The FSMGraph
// default deliberately differs from the serialized value to prove it is the
// JSON that determines the restored value, not mere graph seeding.
func TestRestoreVarScope_VarValues_RestoredFromJSON(t *testing.T) {
	g := &FSMGraph{
		Variables: map[string]*VariableNode{
			"greeting": {Name: "greeting", Type: cty.String, Default: cty.StringVal("default-value")},
		},
	}
	// Serialize vars with a runtime value that differs from the FSMGraph default.
	vars := map[string]cty.Value{
		"var":   cty.ObjectVal(map[string]cty.Value{"greeting": cty.StringVal("hello-override")}),
		"steps": cty.EmptyObjectVal,
	}
	scopeJSON, err := SerializeVarScope(vars)
	if err != nil {
		t.Fatalf("SerializeVarScope: %v", err)
	}
	restored, _, err := RestoreVarScope(scopeJSON, g)
	if err != nil {
		t.Fatalf("RestoreVarScope: %v", err)
	}
	got := restored["var"].GetAttr("greeting")
	want := cty.StringVal("hello-override")
	if !got.RawEquals(want) {
		t.Errorf("greeting = %#v, want %#v (JSON override should take precedence over FSMGraph default)", got, want)
	}
}

// TestRestoreVarScope_VarTypeMismatch_ReturnsError verifies that a type mismatch
// between a JSON var value and the FSMGraph-declared type returns an error.
// This guards against corrupt scope blobs reaching the engine.
func TestRestoreVarScope_VarTypeMismatch_ReturnsError(t *testing.T) {
	// JSON declares count as a non-numeric string, but the graph declares count as number.
	jsonScope := `{"var": {"count": "not-a-number"}}`
	g := &FSMGraph{
		Variables: map[string]*VariableNode{
			"count": {Name: "count", Type: cty.Number, Default: cty.NumberFloatVal(7.0)},
		},
	}
	_, _, err := RestoreVarScope(jsonScope, g)
	if err == nil {
		t.Fatal("expected error for type-mismatched var value, got nil")
	}
	if !strings.Contains(err.Error(), "count") {
		t.Errorf("error should name the offending variable; got: %v", err)
	}
}

// TestRestoreVarScope_NumericPrefixGarbage_ReturnsError is a regression test for
// the strict numeric parsing requirement. "1oops" is prefix-valid (fmt.Sscanf
// would accept it as 1) but is not a valid float string. RestoreVarScope must
// reject it with an error rather than silently restoring count=1.
func TestRestoreVarScope_NumericPrefixGarbage_ReturnsError(t *testing.T) {
	jsonScope := `{"var": {"count": "1oops"}}`
	g := &FSMGraph{
		Variables: map[string]*VariableNode{
			"count": {Name: "count", Type: cty.Number, Default: cty.NumberFloatVal(0.0)},
		},
	}
	_, _, err := RestoreVarScope(jsonScope, g)
	if err == nil {
		t.Fatal("expected error for prefix-valid garbage '1oops', got nil (fmt.Sscanf regression)")
	}
	if !strings.Contains(err.Error(), "count") {
		t.Errorf("error should name the offending variable; got: %v", err)
	}
}
