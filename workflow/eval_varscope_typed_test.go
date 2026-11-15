package workflow

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"
)

// typedVarsScopeJSON builds a scope blob carrying a typed "vars_typed" cty-JSON
// pair for the given run-time var object, optionally merged into a base map of
// extra sections (such as the legacy "var" string map).
func typedVarsScopeJSON(t *testing.T, vars cty.Value, extra map[string]interface{}) string {
	t.Helper()
	valBytes, err := ctyjson.Marshal(vars, vars.Type())
	if err != nil {
		t.Fatalf("marshal vars: %v", err)
	}
	typeBytes, err := ctyjson.MarshalType(vars.Type())
	if err != nil {
		t.Fatalf("marshal var types: %v", err)
	}
	scope := map[string]interface{}{
		"vars_typed":      string(valBytes),
		"vars_typed_type": string(typeBytes),
	}
	for k, v := range extra {
		scope[k] = v
	}
	b, err := json.Marshal(scope)
	if err != nil {
		t.Fatalf("marshal scope: %v", err)
	}
	return string(b)
}

// TestRestoreVarScope_TypedVars_OverrideWinsOverDefault verifies that the
// typed "vars_typed" pair overlays the graph-seeded var object: a run-time
// value restored from the checkpoint wins over the declared default, which is
// what keeps input-expression re-evaluation identical across a restart.
func TestRestoreVarScope_TypedVars_OverrideWinsOverDefault(t *testing.T) {
	g := fsmGraphWithVarDefaults(map[string]cty.Value{
		"repos": cty.ListValEmpty(cty.String),
		"flag":  cty.BoolVal(true),
	})
	scopeJSON := typedVarsScopeJSON(t, cty.ObjectVal(map[string]cty.Value{
		"repos": cty.ListVal([]cty.Value{cty.StringVal("infra"), cty.StringVal("tools")}),
		"flag":  cty.BoolVal(false),
	}), nil)

	restored, _, err := RestoreVarScope(scopeJSON, g)
	if err != nil {
		t.Fatalf("RestoreVarScope: %v", err)
	}

	varObj := restored["var"]
	assertCtyMapEqual(t, map[string]cty.Value{
		"repos": cty.ListVal([]cty.Value{cty.StringVal("infra"), cty.StringVal("tools")}),
		"flag":  cty.BoolVal(false),
	}, map[string]cty.Value{
		"repos": varObj.GetAttr("repos"),
		"flag":  varObj.GetAttr("flag"),
	})
}

// TestRestoreVarScope_TypedVars_NullOverlayPreserved verifies that a null
// run-time value restores as null — the exact pre-restart value — rather than
// being replaced by the declared default.
func TestRestoreVarScope_TypedVars_NullOverlayPreserved(t *testing.T) {
	g := fsmGraphWithVarDefaults(map[string]cty.Value{
		"tag": cty.StringVal("fallback"),
	})
	scopeJSON := typedVarsScopeJSON(t, cty.ObjectVal(map[string]cty.Value{
		"tag": cty.NullVal(cty.String),
	}), nil)

	restored, _, err := RestoreVarScope(scopeJSON, g)
	if err != nil {
		t.Fatalf("RestoreVarScope: %v", err)
	}

	if got := restored["var"].GetAttr("tag"); !got.IsNull() {
		t.Errorf("expected null overlay for %q, got %#v", "tag", got)
	}
}

// TestRestoreVarScope_TypedVars_LegacyBlobFallsBackToDefaults verifies that a
// checkpoint blob written before the typed pair existed (only the legacy
// string "var" map) restores graph defaults — the long-standing behavior for
// blobs without run-time variable values.
func TestRestoreVarScope_TypedVars_LegacyBlobFallsBackToDefaults(t *testing.T) {
	g := fsmGraphWithVarDefaults(map[string]cty.Value{
		"repos": cty.ListVal([]cty.Value{cty.StringVal("default-repo")}),
	})
	scopeJSON := `{"var": {"repos": "default-repo"}}`

	restored, _, err := RestoreVarScope(scopeJSON, g)
	if err != nil {
		t.Fatalf("RestoreVarScope: %v", err)
	}

	assertCtyMapEqual(t, map[string]cty.Value{
		"repos": cty.ListVal([]cty.Value{cty.StringVal("default-repo")}),
	}, map[string]cty.Value{
		"repos": restored["var"].GetAttr("repos"),
	})
}

// TestRestoreVarScope_TypedVars_RetypedVarFallsBackToDefault verifies the
// safety rule for edited workflows: when a restored attribute's cty type no
// longer matches the declared type (the workflow was retyped between the
// checkpoint and the restore), the declared default is kept and the sibling
// variables that did not drift still restore.
func TestRestoreVarScope_TypedVars_RetypedVarFallsBackToDefault(t *testing.T) {
	g := fsmGraphWithVarDefaults(map[string]cty.Value{
		"repos": cty.StringVal("base"),
		"flag":  cty.BoolVal(false),
	})
	scopeJSON := typedVarsScopeJSON(t, cty.ObjectVal(map[string]cty.Value{
		// "repos" was a list at checkpoint time but is a string now.
		"repos": cty.ListVal([]cty.Value{cty.StringVal("infra"), cty.StringVal("tools")}),
		"flag":  cty.BoolVal(true),
	}), nil)

	restored, _, err := RestoreVarScope(scopeJSON, g)
	if err != nil {
		t.Fatalf("RestoreVarScope: %v", err)
	}

	varObj := restored["var"]
	if got := varObj.GetAttr("repos"); got.AsString() != "base" {
		t.Errorf("retyped var should fall back to declared default, got %#v", got)
	}
	if got := varObj.GetAttr("flag"); !got.True() {
		t.Errorf("undrifted var should still restore from the checkpoint, got %#v", got)
	}
}

// TestRestoreVarScope_TypedVars_RemovedVarAttrIgnored verifies that a
// checkpoint attribute that no longer exists in the workflow does not leak
// into the restored var object.
func TestRestoreVarScope_TypedVars_RemovedVarAttrIgnored(t *testing.T) {
	g := fsmGraphWithVarDefaults(map[string]cty.Value{
		"kept": cty.StringVal("base"),
	})
	scopeJSON := typedVarsScopeJSON(t, cty.ObjectVal(map[string]cty.Value{
		"kept":  cty.StringVal("override"),
		"ghost": cty.StringVal("renamed-or-removed"),
	}), nil)

	restored, _, err := RestoreVarScope(scopeJSON, g)
	if err != nil {
		t.Fatalf("RestoreVarScope: %v", err)
	}

	varObj := restored["var"]
	if got := varObj.GetAttr("kept"); got.AsString() != "override" {
		t.Errorf("kept var should restore from the checkpoint, got %#v", got)
	}
	if varObj.Type().HasAttribute("ghost") {
		t.Errorf("removed checkpoint attribute leaked into the restored var object")
	}
}

// TestRestoreVarScope_TypedVars_MalformedErrorsAreFailClosed verifies that a
// corrupted typed pair aborts the restore instead of silently falling back to
// defaults — the same contract restoreStepOutputs holds for step outputs.
func TestRestoreVarScope_TypedVars_MalformedErrorsAreFailClosed(t *testing.T) {
	g := fsmGraphWithVarDefaults(map[string]cty.Value{
		"keep": cty.StringVal("base"),
	})

	t.Run("bad_type_bytes", func(t *testing.T) {
		raw := map[string]interface{}{
			"vars_typed":      `{"keep":"override"}`,
			"vars_typed_type": "not-a-serialized-cty-type",
		}
		b, err := json.Marshal(raw)
		if err != nil {
			t.Fatalf("marshal scope: %v", err)
		}

		_, _, err = RestoreVarScope(string(b), g)
		if err == nil {
			t.Fatal("expected error for malformed vars_typed_type bytes")
		}
		if !strings.Contains(err.Error(), "restore variable types") {
			t.Errorf("error should describe the failed type decode; got: %v", err)
		}
	})

	t.Run("value_type_mismatch", func(t *testing.T) {
		// The type bytes declare a list attribute but the value bytes carry a
		// number — unmarshal must fail rather than guess.
		typeBytes, err := ctyjson.MarshalType(cty.Object(map[string]cty.Type{"keep": cty.List(cty.String)}))
		if err != nil {
			t.Fatalf("marshal type: %v", err)
		}
		scope := map[string]interface{}{
			"vars_typed":      `{"keep":"override"}`,
			"vars_typed_type": string(typeBytes),
		}
		b, err := json.Marshal(scope)
		if err != nil {
			t.Fatalf("marshal scope: %v", err)
		}

		_, _, err = RestoreVarScope(string(b), g)
		if err == nil {
			t.Fatal("expected error for value that does not match declared type")
		}
		if !strings.Contains(err.Error(), "restore variable values") {
			t.Errorf("error should describe the failed value decode; got: %v", err)
		}
	})
}

// TestRestoreVarScope_TypedVars_NullObjectBlobFallsBackToDefaults verifies
// that a typed pair carrying a null whole-object restores graph defaults
// without error, mirroring restoreStepOutputs' handling of a null steps blob.
func TestRestoreVarScope_TypedVars_NullObjectBlobFallsBackToDefaults(t *testing.T) {
	g := fsmGraphWithVarDefaults(map[string]cty.Value{
		"keep": cty.StringVal("base"),
	})
	typeBytes, err := ctyjson.MarshalType(cty.Object(map[string]cty.Type{}))
	if err != nil {
		t.Fatalf("marshal type: %v", err)
	}
	scope := map[string]interface{}{
		// ctyjson.Marshal of a null value renders the literal "null".
		"vars_typed":      "null",
		"vars_typed_type": string(typeBytes),
	}
	b, err := json.Marshal(scope)
	if err != nil {
		t.Fatalf("marshal scope: %v", err)
	}
	restored, _, err := RestoreVarScope(string(b), g)
	if err != nil {
		t.Fatalf("RestoreVarScope: %v", err)
	}
	if got := restored["var"].GetAttr("keep"); got.AsString() != "base" {
		t.Errorf("expected declared default, got %#v", got)
	}
}

// TestSerializeVarScope_TypedVarsBlobShape is the golden-shape check for the
// blob: the typed pair must be present and decode back to the exact run-time
// values, and it must sit alongside the legacy string "var" map so older
// readers keep working.
func TestSerializeVarScope_TypedVarsBlobShape(t *testing.T) {
	runVars := cty.ObjectVal(map[string]cty.Value{
		"repos": cty.ListVal([]cty.Value{cty.StringVal("infra"), cty.StringVal("tools")}),
		"flag":  cty.BoolVal(true),
	})
	vars := map[string]cty.Value{
		"var":   runVars,
		"steps": cty.EmptyObjectVal,
	}

	scopeJSON, err := SerializeVarScope(vars)
	if err != nil {
		t.Fatalf("SerializeVarScope: %v", err)
	}

	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(scopeJSON), &raw); err != nil {
		t.Fatalf("scope is not valid JSON: %v", err)
	}
	valStr, ok := raw["vars_typed"].(string)
	if !ok {
		t.Fatalf("missing string field vars_typed in %s", scopeJSON)
	}
	typeStr, ok := raw["vars_typed_type"].(string)
	if !ok {
		t.Fatalf("missing string field vars_typed_type in %s", scopeJSON)
	}
	declared, err := ctyjson.UnmarshalType([]byte(typeStr))
	if err != nil {
		t.Fatalf("vars_typed_type does not decode: %v", err)
	}
	decoded, err := ctyjson.Unmarshal([]byte(valStr), declared)
	if err != nil {
		t.Fatalf("vars_typed does not decode: %v", err)
	}
	if !decoded.RawEquals(runVars) {
		t.Errorf("typed var pair did not round-trip losslessly: want %#v, got %#v", runVars, decoded)
	}
	if _, ok := raw["var"]; !ok {
		t.Errorf("legacy string var map must remain for older readers; blob: %s", scopeJSON)
	}
}
