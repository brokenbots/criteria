package cli

import (
	"encoding/json"
	"testing"

	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"

	"github.com/brokenbots/criteria/workflow"
)

// typedRestoreGraph builds a graph declaring a list-typed and a string-typed
// variable so restoreRunScope's typed/legacy gating can be exercised against
// both shapes.
func typedRestoreGraph() *workflow.FSMGraph {
	return &workflow.FSMGraph{Variables: map[string]*workflow.VariableNode{
		"repos": {Name: "repos", Type: cty.List(cty.String), Default: cty.ListValEmpty(cty.String)},
		"tag":   {Name: "tag", Type: cty.String, Default: cty.StringVal("none")},
	}}
}

// TestServeAdapterRestoreRunScope_TypedVarsSkipLegacyMap verifies that a
// checkpoint blob carrying the typed "vars_typed" pair restores structured
// variable values losslessly and does not re-apply the legacy string map, which
// cannot represent a list-typed override.
func TestServeAdapterRestoreRunScope_TypedVarsSkipLegacyMap(t *testing.T) {
	valBytes, err := ctyjson.Marshal(cty.ObjectVal(map[string]cty.Value{
		"repos": cty.ListVal([]cty.Value{cty.StringVal("infra"), cty.StringVal("tools")}),
		"tag":   cty.StringVal("nightly"),
	}), cty.Object(map[string]cty.Type{
		"repos": cty.List(cty.String),
		"tag":   cty.String,
	}))
	if err != nil {
		t.Fatalf("marshal vars: %v", err)
	}
	typeBytes, err := ctyjson.MarshalType(cty.Object(map[string]cty.Type{
		"repos": cty.List(cty.String),
		"tag":   cty.String,
	}))
	if err != nil {
		t.Fatalf("marshal var types: %v", err)
	}
	// The legacy string "repos" value is what the lossy string form produces
	// for a list; re-applying it would fail to parse as the declared type.
	scope := map[string]interface{}{
		"var":             map[string]string{"repos": "infra,tools", "tag": "nightly"},
		"vars_typed":      string(valBytes),
		"vars_typed_type": string(typeBytes),
	}
	b, err := json.Marshal(scope)
	if err != nil {
		t.Fatalf("marshal scope: %v", err)
	}

	vars, _, err := restoreRunScope(string(b), typedRestoreGraph())
	if err != nil {
		t.Fatalf("restoreRunScope: %v", err)
	}

	if got := vars["var"].GetAttr("repos"); !got.RawEquals(cty.ListVal([]cty.Value{cty.StringVal("infra"), cty.StringVal("tools")})) {
		t.Errorf("typed list override lost: got %#v", got)
	}
	if got := vars["var"].GetAttr("tag"); got.AsString() != "nightly" {
		t.Errorf("string override lost: got %q", got.AsString())
	}
}

// TestServeAdapterRestoreRunScope_LegacyBlobStillOverlays verifies that a
// checkpoint blob from before the typed pair existed still has its string
// "var" section applied, keeping old snapshots restorable.
func TestServeAdapterRestoreRunScope_LegacyBlobStillOverlays(t *testing.T) {
	scopeJSON := `{"var": {"tag": "nightly"}}`

	vars, _, err := restoreRunScope(scopeJSON, typedRestoreGraph())
	if err != nil {
		t.Fatalf("restoreRunScope: %v", err)
	}

	if got := vars["var"].GetAttr("tag"); got.AsString() != "nightly" {
		t.Errorf("legacy string overlay not applied: tag = %q", got.AsString())
	}
	if got := vars["var"].GetAttr("repos"); len(got.AsValueSlice()) != 0 {
		t.Errorf("repos should keep the declared default, got %#v", got)
	}
}
