package workflow

// jsonschema_test.go — tests for the deterministic cty→JSON Schema converter
// (KB-45) including round-tripping through the pinned proto host validator.

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/ext/typeexpr"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"

	criteriav2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
)

func parseTestExpr(t *testing.T, src string) hcl.Expression {
	t.Helper()
	expr, diags := hclsyntax.ParseExpression([]byte(src), "test.hcl", hcl.Pos{Line: 1, Column: 1})
	if diags.HasErrors() {
		t.Fatalf("parse expression: %s", diags)
	}
	return expr
}

func mustJS(t *testing.T, typ cty.Type, defs *typeexpr.Defaults) string {
	t.Helper()
	raw, err := CTypeToJSONSchema(typ, defs)
	if err != nil {
		t.Fatalf("CTypeToJSONSchema: %v", err)
	}
	return string(raw)
}

func TestJSONSchemaObjectShape(t *testing.T) {
	typ := cty.ObjectWithOptionalAttrs(map[string]cty.Type{
		"summary": cty.String,
		"count":   cty.Number,
		"flag":    cty.Bool,
	}, nil)
	got := mustJS(t, typ, nil)
	want := `{"type":"object","properties":{"count":{"type":"number"},"flag":{"type":"boolean"},"summary":{"type":"string"}},"required":["count","flag","summary"]}`
	if got != want {
		t.Fatalf("unexpected schema bytes:\n got %s\nwant %s", got, want)
	}
}

func TestJSONSchemaOptionalsAndDefaults(t *testing.T) {
	// optional(string, "hello") with a default; bare optional() is optional
	// without a default; required attrs carry no markers.
	defs := &typeexpr.Defaults{
		Type: cty.Object(map[string]cty.Type{"summary": cty.String, "count": cty.Number}),
		DefaultValues: map[string]cty.Value{
			"count": cty.NumberIntVal(1),
		},
	}
	typ := cty.ObjectWithOptionalAttrs(map[string]cty.Type{
		"summary": cty.String,
		"count":   cty.Number,
	}, []string{"count", "summary"})
	got := mustJS(t, typ, defs)
	want := `{"type":"object","properties":{"count":{"type":"number","default":1},"summary":{"type":"string"}}}`
	if got != want {
		t.Fatalf("unexpected schema bytes:\n got %s\nwant %s", got, want)
	}
}

func TestJSONSchemaCollections(t *testing.T) {
	typ := cty.Object(map[string]cty.Type{
		"tags": cty.List(cty.String),
		"set":  cty.Set(cty.Number),
		"map":  cty.Map(cty.Bool),
	})
	got := mustJS(t, typ, nil)
	want := `{"type":"object","properties":{"map":{"type":"object"},"set":{"type":"array","items":{"type":"number"}},"tags":{"type":"array","items":{"type":"string"}}},"required":["map","set","tags"]}`
	if got != want {
		t.Fatalf("unexpected schema bytes:\n got %s\nwant %s", got, want)
	}
}

func TestJSONSchemaDeterminism(t *testing.T) {
	typ := cty.Object(map[string]cty.Type{
		"z": cty.String, "a": cty.List(cty.Number), "m": cty.Bool,
	})
	first := mustJS(t, typ, nil)
	for i := 0; i < 20; i++ {
		if again := mustJS(t, typ, nil); again != first {
			t.Fatalf("non-deterministic output at iteration %d", i)
		}
	}
}

// TestJSONSchemaNamedInlineByteEqual proves the wire-indistinguishability
// requirement: resolving the same constraint once through a named type block
// and once inline yields identical schema bytes.
func TestJSONSchemaNamedInlineByteEqual(t *testing.T) {
	named := `
workflow {
  name          = "t"
  version       = "0.1"
  initial_state = "a"
  target_state  = "done"
}
type "payload" {
  schema = object({
    summary = optional(string, "unchanged")
    detail  = string
  })
}
`
	spec, diags := Parse("t.hcl", []byte(named))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags)
	}
	g := newFSMGraph(spec)
	if d := compileTypes(g, spec); d.HasErrors() {
		t.Fatalf("compileTypes: %s", d)
	}
	namedBytes := mustJS(t, g.Types["payload"].Type, g.Types["payload"].Defaults)

	inlineTyp, inlineDefs, dgs := resolveTypeConstraint(parseTestExpr(t, `object({
    summary = optional(string, "unchanged")
    detail  = string
  })`))
	if dgs.HasErrors() {
		t.Fatalf("inline resolve: %s", dgs)
	}
	inlineBytes := mustJS(t, inlineTyp, inlineDefs)
	if namedBytes != inlineBytes {
		t.Fatalf("named ≠ inline:\n named %s\ninline %s", namedBytes, inlineBytes)
	}
}

// TestJSONSchemaAgainstPinnedValidator round-trips emitted schemas through the
// pinned proto host validator to prove wire compatibility.
func TestJSONSchemaAgainstPinnedValidator(t *testing.T) {
	typ, defs, dgs := resolveTypeConstraint(parseTestExpr(t, `object({
    summary = optional(string, "unchanged")
    detail  = string
  })`))
	if dgs.HasErrors() {
		t.Fatalf("resolve: %s", dgs)
	}
	schemaBytes := mustJS(t, typ, defs)

	if issues := criteriav2.ValidatePayloadSchema([]byte(schemaBytes), []byte(`{"detail":"done"}`)); len(issues) != 0 {
		t.Fatalf("valid payload rejected: %v", issues)
	}
	if issues := criteriav2.ValidatePayloadSchema([]byte(schemaBytes), []byte(`{"detail":5}`)); len(issues) == 0 ||
		issues[0] != `payload_schema: property "detail": expected "string", got "number"` {
		t.Fatalf("invalid payload not rejected: %v", issues)
	}
	if issues := criteriav2.ValidatePayloadSchema([]byte(schemaBytes), []byte(`{}`)); len(issues) == 0 ||
		issues[0] != `payload_schema: property "detail": required property is missing` {
		t.Fatalf("missing property not rejected: %v", issues)
	}
}

func TestJSONSchemaUnsupportedTypeErrors(t *testing.T) {
	_, err := CTypeToJSONSchema(cty.Capsule("opaque", reflect.TypeOf(opaqueCapsule{})), nil)
	if err == nil || fmt.Sprint(err) == "" {
		t.Fatal("expected error for capsule types")
	}
}

// opaqueCapsule is a stand-in capsule type for the unsupported-type test.
type opaqueCapsule struct{}
