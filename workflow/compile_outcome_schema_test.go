package workflow

// compile_outcome_schema_test.go — KB-45 tests for outcome-level payload
// contracts: schema (named type.<name> and inline forms), require_comment,
// fallback uniqueness, the subset rule against the adapter handshake
// output_schema and output projections, and the unsupported-attribute
// diagnostics on wait/approval outcomes.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/ext/typeexpr"
	"github.com/zclconf/go-cty/cty"
)

// outcomeSchemaSchemas is a compile-time adapter surface exercising each lane
// of the schema subset rule: strictly typed fields, a permissive field, and a
// nested object field.
var outcomeSchemaSchemas = map[string]AdapterInfo{
	"typed": {
		InputSchema:  map[string]ConfigField{},
		ConfigSchema: map[string]ConfigField{},
		OutputSchema: map[string]ConfigField{
			"severity": {Type: ConfigFieldString, CtyType: cty.String},
			"attempts": {Type: ConfigFieldNumber, CtyType: cty.Number},
			"meta": {
				Type: ConfigFieldString,
				// Nested object: adapter promises a, b, c; the outcome schema
				// may require only a subset of those.
				CtyType: cty.Object(map[string]cty.Type{"a": cty.String, "b": cty.String, "c": cty.String}),
			},
			"free": {Type: ConfigFieldString, CtyType: cty.NilType}, // permissive
		},
	},
	"notyped": {
		InputSchema:  map[string]ConfigField{},
		ConfigSchema: map[string]ConfigField{},
	},
}

// compileOutcomeSchemaSrc compiles src with the typed adapter surface and
// returns the graph and diagnostics. The test sources must reference
// adapter.typed.default for schema-subset validation lanes.
func compileOutcomeSchemaSrc(t *testing.T, src string) (*FSMGraph, hcl.Diagnostics) {
	t.Helper()
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := Compile(spec, outcomeSchemaSchemas)
	return g, diags
}

// parseTestExprDefaults re-parses an inline constraint through the production
// parser so expectations carry the same optional() defaults the compile path
// sees.
func parseTestExprDefaults(t *testing.T, src string) *typeexpr.Defaults {
	t.Helper()
	expr := parseTestExpr(t, src)
	typ, defs, diags := resolveTypeConstraint(expr)
	if diags.HasErrors() {
		t.Fatalf("resolveTypeConstraint: %s", diags.Error())
	}
	if !typ.IsObjectType() {
		t.Fatalf("expected object constraint, got %s", typ.FriendlyName())
	}
	return defs
}

func typedWorkflow(stepBody string) string {
	return `
workflow {
  name = "t"
  version       = "0.1"
  initial_state = "work"
  target_state  = "done"
}
adapter "typed" "default" {}
step "work" {
  target = adapter.typed.default
  ` + stepBody + `
}
state "done" {
  terminal = true
  success  = true
}
`
}

// TestOutcomeSchema_InlineCompilesToContract covers the happy path: an inline
// object constraint with optional() defaults compiles onto the CompiledOutcome
// as a cty type plus the exact converter bytes, and require_comment/fallback
// carry through.
func TestOutcomeSchema_InlineCompilesToContract(t *testing.T) {
	g, diags := compileOutcomeSchemaSrc(t, typedWorkflow(`
  outcome "success" {
    next    = step.done
    schema  = object({ severity = optional(string, "low"), attempts = number })
    require_comment = true
    fallback        = true
  }
`))
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	co, ok := g.Steps["work"].Outcomes["success"]
	if !ok {
		t.Fatal("outcome 'success' not found")
	}
	if co.Schema == nil || !co.Schema.IsObjectType() {
		t.Fatalf("Schema = %v; want object type", co.Schema)
	}
	want, err := CTypeToJSONSchema(*co.Schema, parseTestExprDefaults(t, `object({ severity = optional(string, "low"), attempts = number })`))
	if err != nil {
		t.Fatal(err)
	}
	if got := co.SchemaJSON; !bytes.Equal(got, want) {
		t.Errorf("SchemaJSON = %s; want %s", got, want)
	}
	if !co.RequireComment || !co.Fallback {
		t.Errorf("RequireComment = %v, Fallback = %v; want both true", co.RequireComment, co.Fallback)
	}
}

// TestOutcomeSchema_NamedRefMatchesInline verifies the named form resolves to
// the same cty type and identical wire bytes as the equivalent inline
// constraint.
func TestOutcomeSchema_NamedRefMatchesInline(t *testing.T) {
	shared := `object({ severity = optional(string, "low") })`
	named := `
workflow {
  name = "t"
  version       = "0.1"
  initial_state = "work"
  target_state  = "done"
}
type "sig" {
  schema = ` + shared + `
}
adapter "typed" "default" {}
step "work" {
  target = adapter.typed.default
  outcome "success" {
    next   = step.done
    schema = type.sig
  }
}
state "done" {
  terminal = true
  success  = true
}
`
	g, diags := compileOutcomeSchemaSrc(t, named)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	co := g.Steps["work"].Outcomes["success"]
	if co == nil || co.Schema == nil {
		t.Fatal("schema not compiled from type.sig reference")
	}
	want, err := CTypeToJSONSchema(*co.Schema, parseTestExprDefaults(t, shared))
	if err != nil {
		t.Fatal(err)
	}
	// The named form must carry the type's defaults through to the wire bytes.
	if !bytes.Equal(co.SchemaJSON, want) {
		t.Errorf("SchemaJSON = %s; want %s", co.SchemaJSON, want)
	}
	// The default from the type block surfaces on the wire.
	if !bytes.Contains(co.SchemaJSON, []byte(`"default":"low"`)) {
		t.Errorf("named schema missing type default: %s", co.SchemaJSON)
	}
}

// TestOutcomeSchema_UnknownTypeRef is the named-reference resolution error.
func TestOutcomeSchema_UnknownTypeRef(t *testing.T) {
	_, diags := compileOutcomeSchemaSrc(t, typedWorkflow(`
  outcome "success" {
    next   = step.done
    schema = type.missing
  }
`))
	if !diags.HasErrors() {
		t.Fatal("expected compile error for unknown type reference")
	}
	if !strings.Contains(diags.Error(), `unknown workflow type "missing"`) {
		t.Errorf("diagnostics = %s", diags.Error())
	}
}

// TestOutcomeSchema_MustBeObject rejects non-object schemas before they reach
// the wire contract: the host predicate accepts only an object root. The gate
// applies to named refs and inline constraints alike (KB-48 behavior
// neutrality: an inline schema and its named twin compile identically).
func TestOutcomeSchema_MustBeObject(t *testing.T) {
	_, diags := compileOutcomeSchemaSrc(t, typedWorkflow(`
  outcome "success" {
    next   = step.done
    schema = list(string)
  }
`))
	if !diags.HasErrors() {
		t.Fatal("expected compile error for non-object schema")
	}
	if !strings.Contains(diags.Error(), "schema must be an object(...)") {
		t.Errorf("diagnostics = %s", diags.Error())
	}
}

// TestOutcomeSchema_NamedNonObjectRefGated pins that a named type resolving to
// a non-object type is rejected by the same object gate as the inline form.
func TestOutcomeSchema_NamedNonObjectRefGated(t *testing.T) {
	_, diags := compileOutcomeSchemaSrc(t, `
workflow {
  name = "t"
  version       = "0.1"
  initial_state = "work"
  target_state  = "done"
}
type "flat" {
  schema = list(string)
}
adapter "typed" "default" {}
step "work" {
  target = adapter.typed.default
  outcome "success" {
    next   = state.done
    schema = type.flat
  }
}
state "done" {
  terminal = true
  success  = true
}
`)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for non-object named schema")
	}
	if !strings.Contains(diags.Error(), `outcome "success": schema must be an object(...)`) {
		t.Errorf("diagnostics = %s", diags.Error())
	}
}

// TestOutcomeSchema_ComposedNamedRefRejected pins the outcome path through the
// same shared resolver (KB-48): composing a named ref into a wider constraint
// (schema = list(type.flat)) is a constraint-position error, not the object
// gate and not a value coercion.
func TestOutcomeSchema_ComposedNamedRefRejected(t *testing.T) {
	_, diags := compileOutcomeSchemaSrc(t, `
workflow {
  name = "t"
  version       = "0.1"
  initial_state = "work"
  target_state  = "done"
}
type "flat" {
  schema = list(string)
}
adapter "typed" "default" {}
step "work" {
  target = adapter.typed.default
  outcome "success" {
    next   = step.done
    schema = list(type.flat)
  }
}
state "done" {
  terminal = true
  success  = true
}
`)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for composed named schema")
	}
	if !strings.Contains(diags.Error(), `step "work" outcome "success": type references cannot be composed into other type constraints`) {
		t.Errorf("expected composition diagnostic, got: %s", diags.Error())
	}
	if strings.Contains(diags.Error(), "schema must be an object") {
		t.Errorf("composed ref must fail as a constraint, not via the object gate: %s", diags.Error())
	}
}

// TestOutcomeSchema_FieldNotInAdapterSchema: a schema field the adapter never
// declares can never be satisfied — compile error.
func TestOutcomeSchema_FieldNotInAdapterSchema(t *testing.T) {
	_, diags := compileOutcomeSchemaSrc(t, typedWorkflow(`
  outcome "success" {
    next   = step.done
    schema = object({ missing = string })
  }
`))
	if !diags.HasErrors() {
		t.Fatal("expected compile error for field outside adapter output schema")
	}
	if !strings.Contains(diags.Error(), `schema field "missing" is not declared in the adapter's output schema`) {
		t.Errorf("diagnostics = %s", diags.Error())
	}
}

// TestOutcomeSchema_FieldTypeIncompatibility: string-producing adapter field
// vs number-requiring schema — never satisfiable.
func TestOutcomeSchema_FieldTypeIncompatibility(t *testing.T) {
	_, diags := compileOutcomeSchemaSrc(t, typedWorkflow(`
  outcome "success" {
    next   = step.done
    schema = object({ severity = number })
  }
`))
	if !diags.HasErrors() {
		t.Fatal("expected compile error for incompatible field type")
	}
	if !strings.Contains(diags.Error(), "is not compatible with the adapter's declared output type string") {
		t.Errorf("diagnostics = %s", diags.Error())
	}
}

// TestOutcomeSchema_SubsetOfNestedObject: the schema may require a strict
// subset of a nested object field with equal leaf types.
func TestOutcomeSchema_SubsetOfNestedObject(t *testing.T) {
	g, diags := compileOutcomeSchemaSrc(t, typedWorkflow(`
  outcome "success" {
    next   = step.done
    schema = object({ meta = object({ a = string }) })
  }
`))
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	if g.Steps["work"].Outcomes["success"].Schema == nil {
		t.Fatal("schema not compiled")
	}
}

// TestOutcomeSchema_OptionalAdapterAttrVsRequiredSchema: when the adapter
// marks a nested attribute optional (may omit it) but the outcome schema
// requires it, the schema can never be satisfied.
func TestOutcomeSchema_OptionalAdapterAttrVsRequiredSchema(t *testing.T) {
	loose := map[string]AdapterInfo{
		"typed": {
			InputSchema:  map[string]ConfigField{},
			ConfigSchema: map[string]ConfigField{},
			OutputSchema: map[string]ConfigField{
				"meta": {Type: ConfigFieldString, CtyType: cty.ObjectWithOptionalAttrs(
					map[string]cty.Type{"a": cty.String},
					[]string{"a"})},
			},
		},
	}
	spec, diags := Parse("t.hcl", []byte(typedWorkflow(`
  outcome "success" {
    next   = step.done
    schema = object({ meta = object({ a = string }) })
  }
`)))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, loose)
	if !diags.HasErrors() {
		t.Fatal("expected compile error: optional adapter attribute vs required schema attribute")
	}
	if !strings.Contains(diags.Error(), "not compatible") {
		t.Errorf("diagnostics = %s", diags.Error())
	}
}

// TestOutcomeSchema_PermissiveAdapterField: an adapter field with no declared
// cty type accepts any outcome schema shape for that field.
func TestOutcomeSchema_PermissiveAdapterField(t *testing.T) {
	g, diags := compileOutcomeSchemaSrc(t, typedWorkflow(`
  outcome "success" {
    next   = step.done
    schema = object({ free = object({ any = string }) })
  }
`))
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	if g.Steps["work"].Outcomes["success"].Schema == nil {
		t.Fatal("schema not compiled")
	}
}

// TestOutcomeSchema_AdapterWithoutOutputSchema: with no handshake output
// schema there is nothing to subset against at compile time; the contract
// still compiles and is enforced host-side at runtime.
func TestOutcomeSchema_AdapterWithoutOutputSchema(t *testing.T) {
	noschema := `
workflow {
  name = "t"
  version       = "0.1"
  initial_state = "work"
  target_state  = "done"
}
adapter "notyped" "default" {}
step "work" {
  target = adapter.notyped.default
  outcome "success" {
    next   = step.done
    schema = object({ severity = string })
  }
}
state "done" {
  terminal = true
  success  = true
}
`
	g, diags := compileOutcomeSchemaSrc(t, noschema)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	if g.Steps["work"].Outcomes["success"].Schema == nil {
		t.Fatal("schema not compiled")
	}
}

// TestOutcomeSchema_FallbackTwice is the max-one-fallback-per-step rule,
// including a duplicate that involves the default outcome.
func TestOutcomeSchema_FallbackTwice(t *testing.T) {
	_, diags := compileOutcomeSchemaSrc(t, typedWorkflow(`
  outcome "success" {
    next    = step.done
    fallback = true
  }
  outcome "failure" {
    next    = step.done
    fallback = true
  }
`))
	if !diags.HasErrors() {
		t.Fatal("expected compile error for two fallback outcomes")
	}
	if !strings.Contains(diags.Error(), "only one outcome per step may set fallback = true") {
		t.Errorf("diagnostics = %s", diags.Error())
	}
}

// TestOutcomeSchema_DefaultCarriesContract: the default outcome (un_mapped
// adapter names) may carry its own schema/require_comment; fallback on it
// also participates in the uniqueness count.
func TestOutcomeSchema_DefaultCarriesContract(t *testing.T) {
	g, diags := compileOutcomeSchemaSrc(t, typedWorkflow(`
  outcome "success" { next = step.done }
  outcome "default" {
    next   = step.done
    schema = object({ severity = optional(string, "low") })
    require_comment = true
  }
`))
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	co := g.Steps["work"].DefaultOutcome
	if co == nil || co.Schema == nil || !co.RequireComment {
		t.Fatalf("default outcome contract not compiled: %+v", co)
	}
}

// TestOutcomeSchema_AggregateRequiresProjection: aggregate iteration outcomes
// see no raw adapter outputs, so a schema without an output projection cannot
// be validated — compile error.
func TestOutcomeSchema_AggregateRequiresProjection(t *testing.T) {
	_, diags := compileOutcomeSchemaSrc(t, `
workflow {
  name = "t"
  version       = "0.1"
  initial_state = "work"
  target_state  = "done"
}
adapter "typed" "default" {}
step "work" {
  target = adapter.typed.default
  for_each = ["a", "b"]
  outcome "success" { next = continue }
  outcome "all_succeeded" {
    next   = step.done
    schema = object({ severity = string })
  }
}
state "done" {
  terminal = true
  success  = true
}
`)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for aggregate schema without projection")
	}
	if !strings.Contains(diags.Error(), "requires an output = { ... } projection") {
		t.Errorf("diagnostics = %s", diags.Error())
	}
}

// TestOutcomeSchema_AggregateProjectValidated: with a foldable projection the
// schema validates field-by-field against it.
func TestOutcomeSchema_AggregateProjectValidated(t *testing.T) {
	g, diags := compileOutcomeSchemaSrc(t, `
workflow {
  name = "t"
  version       = "0.1"
  initial_state = "work"
  target_state  = "done"
}
adapter "typed" "default" {}
step "work" {
  target = adapter.typed.default
  for_each = ["a", "b"]
  outcome "success" { next = continue }
  outcome "all_succeeded" {
    next    = step.done
    schema  = object({ severity = string })
    output  = { severity = "low" }
  }
}
state "done" {
  terminal = true
  success  = true
}
`)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	co := g.Steps["work"].Outcomes["all_succeeded"]
	if co == nil || co.Schema == nil {
		t.Fatal("aggregate schema not compiled")
	}
}

// TestOutcomeSchema_ProjectionFieldMissing: a schema field the projection
// never produces is a compile error.
func TestOutcomeSchema_ProjectionFieldMissing(t *testing.T) {
	_, diags := compileOutcomeSchemaSrc(t, typedWorkflow(`
  outcome "success" {
    next   = step.done
    schema = object({ missing = string })
    output = { severity = "low" }
  }
`))
	if !diags.HasErrors() {
		t.Fatal("expected compile error for schema field outside projection")
	}
	if !strings.Contains(diags.Error(), `schema field "missing" is not produced by the outcome's output projection`) {
		t.Errorf("diagnostics = %s", diags.Error())
	}
}

// TestOutcomeSchema_ProjectionTypeMismatch: the projection's folded field
// types must satisfy the schema.
func TestOutcomeSchema_ProjectionTypeMismatch(t *testing.T) {
	_, diags := compileOutcomeSchemaSrc(t, typedWorkflow(`
  outcome "success" {
    next   = step.done
    schema = object({ severity = number })
    output = { severity = "low" }
  }
`))
	if !diags.HasErrors() {
		t.Fatal("expected compile error for projection type mismatch")
	}
	if !strings.Contains(diags.Error(), "is not compatible with the projected type string") {
		t.Errorf("diagnostics = %s", diags.Error())
	}
}

// TestWaitOutcomeContractAttrsUnsupported: wait outcomes have no payload
// contract surface in KB-45 — unsupported attributes fail loudly.
func TestWaitOutcomeContractAttrsUnsupported(t *testing.T) {
	src := `
workflow {
  name = "t"
  version       = "0.1"
  initial_state = "work"
  target_state  = "done"
}
adapter "typed" "default" {}
step "work" {
  target = adapter.typed.default
  outcome "success" { next = wait.pause }
}
wait "pause" {
  duration = "1s"
  outcome "expired" {
    next            = step.done
    schema          = object({ a = string })
    require_comment = true
    fallback        = true
  }
}
state "done" {
  terminal = true
  success  = true
}
`
	_, diags := compileOutcomeSchemaSrc(t, src)
	if !diags.HasErrors() {
		t.Fatal("expected compile errors for contract attributes on wait outcomes")
	}
	all := diagsText(diags)
	for _, want := range []string{
		`schema is not supported on wait outcomes`,
		`require_comment is not supported on wait outcomes`,
		`fallback is not supported on wait outcomes`,
	} {
		if !strings.Contains(all, want) {
			t.Errorf("diagnostics missing %q; got %s", want, all)
		}
	}
}

// diagsText joins every diagnostic so substring assertions see all of them,
// unlike hcl.Diagnostics.Error() which truncates after the first.
func diagsText(diags hcl.Diagnostics) string {
	msgs := make([]string, 0, len(diags))
	for _, d := range diags {
		msgs = append(msgs, d.Summary+"\n"+d.Detail)
	}
	return strings.Join(msgs, "\n")
}

// TestApprovalOutcomeContractAttrsUnsupported: approval outcomes are
// engine-mandated (approved/rejected, no payload) — contract attributes are
// rejected.
func TestApprovalOutcomeContractAttrsUnsupported(t *testing.T) {
	src := `
workflow {
  name = "t"
  version       = "0.1"
  initial_state = "work"
  target_state  = "done"
}
adapter "typed" "default" {}
step "work" {
  target = adapter.typed.default
  outcome "success" { next = approval.gate }
}
approval "gate" {
  approvers = ["alice"]
  reason    = "gate"
  outcome "approved" { next = step.done }
  outcome "rejected" {
    next            = step.done
    require_comment = true
  }
}
state "done" {
  terminal = true
  success  = true
}
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, outcomeSchemaSchemas)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for require_comment on approval outcome")
	}
	if !strings.Contains(diags.Error(), `require_comment is not supported on approval outcomes`) {
		t.Errorf("diagnostics = %s", diags.Error())
	}
}

// TestOutcomeSchema_BackCompat: the pre-KB-45 outcome shape (next/output/write
// only) compiles with no contract fields set.
func TestOutcomeSchema_BackCompat(t *testing.T) {
	g, diags := compileOutcomeSchemaSrc(t, typedWorkflow(`
  outcome "success" {
    next   = step.done
    output = { severity = "low" }
  }
`))
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	co := g.Steps["work"].Outcomes["success"]
	if co == nil {
		t.Fatal("outcome not compiled")
	}
	if co.Schema != nil || co.SchemaJSON != nil || co.RequireComment || co.Fallback {
		t.Errorf("contract fields set on a contract-free outcome: %+v", co)
	}
}
