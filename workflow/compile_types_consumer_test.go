package workflow

// compile_types_consumer_test.go — named type blocks consumed from data
// blocks, variable declarations, output projections, and subworkflow variable
// bindings (KB-48). Outcome-schema consumption (KB-45) lives in
// compile_outcome_schema_test.go and compile_types_test.go.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/ext/typeexpr"
	"github.com/zclconf/go-cty/cty"

	"github.com/brokenbots/criteria/workflow/lockfile"
)

// compileAllConsumers parses and compiles a full workflow source with
// parentDir as WorkflowDir, returning the graph (nil on errors) and diags.
func compileAllConsumers(t *testing.T, src, parentDir string) (*FSMGraph, hcl.Diagnostics) {
	t.Helper()
	spec, diags := Parse("main.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse failed: %s", diags)
	}
	g, diags := CompileWithOpts(spec, nil, CompileOpts{
		WorkflowDir:         parentDir,
		SubWorkflowResolver: stubConsumerResolver{},
	})
	if diags.HasErrors() {
		return nil, diags
	}
	return g, diags
}

// stubConsumerResolver maps "callee" to t.TempDir()/callee; anything else errors.
type stubConsumerResolver struct{}

func (stubConsumerResolver) ResolveSource(_ context.Context, callerDir, source string) (string, *lockfile.LockedWorkflowRef, error) {
	if source != "callee" {
		return "", nil, fmt.Errorf("unexpected source %q", source)
	}
	return filepath.Join(callerDir, "callee"), nil, nil
}

// calleeFixture writes a callee workflow directory into a fresh parent dir
// and returns the parent dir for use as WorkflowDir, mirroring the
// LocalSubWorkflowResolver path composition (callerDir + "./callee").
func calleeFixture(t *testing.T, content string) string {
	t.Helper()
	base := t.TempDir()
	dir := filepath.Join(base, "callee")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("mkdir callee: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.hcl"), []byte(content), 0o644); err != nil {
		t.Fatalf("write callee: %v", err)
	}
	return base
}

// consumerWorkflows returns the inline-typed and named-typed twin workflow
// sources. Both exercise every KB-48 consumer: a data block, a variable
// declaration, an output projection, and a subworkflow variable binding.
func consumerWorkflows() (inline, named string) {
	consumers := func(dataType, varType, outType string) string {
		return `
data "internal" "cfg" {
  type  = ` + dataType + `
  value = {}
}
variable "deploy" {
  type = ` + varType + `
  default = {}
}
output "deploy_summary" {
  value = var.deploy
  type  = ` + outType + `
}
subworkflow "child" {
  source = "callee"
  input = {
    region = "us-east"
  }
}
state "done" {
  terminal = true
  success  = true
}
`
	}
	payloadInline := consumerPayloadInline
	return consumerHeader + consumers(payloadInline, payloadInline, payloadInline),
		consumerHeader + consumerPayloadTypeBlock + consumers("type.payload", "type.payload", "type.payload")
}

const consumerPayloadInline = `object({
  zone    = optional(string, "z1")
  retries = optional(number, 3)
})`

const consumerPayloadTypeBlock = `
type "payload" {
  schema = object({
    zone    = optional(string, "z1")
    retries = optional(number, 3)
  })
}
`
const consumerHeader = `
workflow {
  name          = "consumers"
  version       = "0.1"
  initial_state = "done"
  target_state  = "done"
}
`

const calleeWorkflow = `
workflow {
  name          = "child"
  version       = "0.1"
  initial_state = "done"
  target_state  = "done"
}
variable "region" {
  type = string
}
state "done" {
  terminal = true
  success  = true
}
`

// compareConsumerTwin asserts the inline-typed and named-typed twin graphs are
// indistinguishable on every compiled artifact the type feeds: the data node's
// type and defaults, the variable node's type and defaults, the output node's
// declared type and defaults, and the type namespace itself.
func compareConsumerTwin(t *testing.T, inline, named *FSMGraph) {
	t.Helper()

	sameType := func(a, b cty.Type, what string) {
		t.Helper()
		if a == cty.NilType && b == cty.NilType {
			return
		}
		if !a.Equals(b) {
			t.Fatalf("%s: twin types differ: inline=%s named=%s", what, a.FriendlyName(), b.FriendlyName())
		}
	}
	sameDefaults := func(a, b *typeexpr.Defaults, what string) {
		t.Helper()
		if a == nil || b == nil {
			if a != nil || b != nil {
				t.Fatalf("%s: one twin carries defaults, the other does not", what)
			}
			return
		}
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("%s: twin defaults differ", what)
		}
	}

	sameType(inline.Data["internal"]["cfg"].Type, named.Data["internal"]["cfg"].Type, "data cfg type")
	sameDefaults(inline.Data["internal"]["cfg"].TypeDefaults, named.Data["internal"]["cfg"].TypeDefaults, "data cfg defaults")
	sameValue(t, inline.Data["internal"]["cfg"].InitialValue, named.Data["internal"]["cfg"].InitialValue, "data cfg initial value")

	sameType(inline.Variables["deploy"].Type, named.Variables["deploy"].Type, "variable deploy type")
	sameDefaults(inline.Variables["deploy"].TypeDefaults, named.Variables["deploy"].TypeDefaults, "variable deploy defaults")
	sameValue(t, inline.Variables["deploy"].Default, named.Variables["deploy"].Default, "variable deploy default")

	sameType(inline.Outputs["deploy_summary"].DeclaredType, named.Outputs["deploy_summary"].DeclaredType, "output deploy_summary type")
	sameDefaults(inline.Outputs["deploy_summary"].TypeDefaults, named.Outputs["deploy_summary"].TypeDefaults, "output deploy_summary defaults")

	// The twins differ only in the type namespace itself: the named twin
	// declares exactly one type block, the inline twin declares none.
	if len(inline.Types) != 0 {
		t.Fatalf("inline twin unexpectedly declares types: %v", inline.TypeOrder)
	}
	if len(named.Types) != 1 || len(named.TypeOrder) != 1 || named.TypeOrder[0] != "payload" {
		t.Fatalf("named twin type namespace wrong: %v", named.TypeOrder)
	}
}

func sameValue(t *testing.T, a, b cty.Value, what string) {
	t.Helper()
	if !a.RawEquals(b) {
		t.Fatalf("%s: twin values differ: inline=%#v named=%#v", what, a, b)
	}
}

// compileTwinPair compiles the inline-typed and named-typed twin workflows and
// returns both graphs. Both are compiled against the same callee directory so
// the subworkflow consumer sees identical callee content.
func compileTwinPair(t *testing.T) (inline, named *FSMGraph) {
	t.Helper()
	dir := calleeFixture(t, calleeWorkflow)
	inlineSrc, namedSrc := consumerWorkflows()
	inlineG, diags := compileAllConsumers(t, inlineSrc, dir)
	if diags.HasErrors() {
		t.Fatalf("inline-typed twin failed to compile: %s", diags)
	}
	namedG, diags := compileAllConsumers(t, namedSrc, dir)
	if diags.HasErrors() {
		t.Fatalf("named-typed twin failed to compile: %s", diags)
	}
	return inlineG, namedG
}

// TestNamedType_TwinEquivalence_AllConsumers is the behavior-neutrality
// guarantee: refactoring a schema from inline to a named type block changes
// NOTHING at compile time (same cty.Type, same defaults, same data/variable/
// output/subworkflow artifacts).
func TestNamedType_TwinEquivalence_AllConsumers(t *testing.T) {
	inline, named := compileTwinPair(t)
	compareConsumerTwin(t, inline, named)
}

// TestNamedType_DataBlockResolves pins the data-block consumer: type.<name>
// resolves to the block's compiled type and defaults land on the DataNode.
func TestNamedType_DataBlockResolves(t *testing.T) {
	dir := calleeFixture(t, calleeWorkflow)
	src := consumerHeader + consumerPayloadTypeBlock + `
data "internal" "cfg" {
  type  = type.payload
  value = {}
}
state "done" {
  terminal = true
  success  = true
}
`
	g, diags := compileAllConsumers(t, src, dir)
	if diags.HasErrors() {
		t.Fatalf("unexpected errors: %s", diags)
	}
	dn := g.Data["internal"]["cfg"]
	want := cty.ObjectWithOptionalAttrs(map[string]cty.Type{"zone": cty.String, "retries": cty.Number}, []string{"zone", "retries"})
	if !dn.Type.Equals(want) {
		t.Fatalf("data type: want %s, got %s", want.FriendlyName(), dn.Type.FriendlyName())
	}
	if dn.TypeDefaults == nil {
		t.Fatal("data type defaults lost through named ref")
	}
	if !dn.InitialValue.RawEquals(cty.ObjectVal(map[string]cty.Value{
		"zone":    cty.StringVal("z1"),
		"retries": cty.NumberIntVal(3),
	})) {
		t.Fatalf("defaults not applied to data initial value: %#v", dn.InitialValue)
	}
}

// TestNamedType_VariableAndOutputResolve pins the variable and output
// consumers: defaults and assignability checks flow through named refs
// identically to the inline form.
func TestNamedType_VariableAndOutputResolve(t *testing.T) {
	dir := calleeFixture(t, calleeWorkflow)
	src := consumerHeader + consumerPayloadTypeBlock + `
variable "deploy" {
  type = type.payload
  default = {}
}
output "deploy_summary" {
  value = var.deploy
  type  = type.payload
}
state "done" {
  terminal = true
  success  = true
}
`
	g, diags := compileAllConsumers(t, src, dir)
	if diags.HasErrors() {
		t.Fatalf("unexpected errors: %s", diags)
	}
	vn := g.Variables["deploy"]
	want := cty.ObjectWithOptionalAttrs(map[string]cty.Type{"zone": cty.String, "retries": cty.Number}, []string{"zone", "retries"})
	if !vn.Type.Equals(want) || vn.TypeDefaults == nil {
		t.Fatalf("variable type/defaults not resolved from named ref: %s", vn.Type.FriendlyName())
	}
	if got := vn.Default; !got.RawEquals(cty.ObjectVal(map[string]cty.Value{
		"zone":    cty.StringVal("z1"),
		"retries": cty.NumberIntVal(3),
	})) {
		t.Fatalf("defaults not applied to variable default: %#v", got)
	}
	on := g.Outputs["deploy_summary"]
	if !on.DeclaredType.Equals(want) || on.TypeDefaults == nil {
		t.Fatalf("output type/defaults not resolved from named ref: %s", on.DeclaredType.FriendlyName())
	}
}

// goldenCheck asserts a single error whose summary contains want appears.
func goldenCheck(t *testing.T, diags hcl.Diagnostics, want string) {
	t.Helper()
	if !diags.HasErrors() {
		t.Fatalf("expected error containing %q, compile succeeded", want)
	}
	for _, d := range diags {
		if strings.Contains(d.Summary, want) {
			return
		}
	}
	t.Fatalf("expected error containing %q, got: %s", want, diags)
}

// TestNamedType_UnknownRefDiagnostics_Golden pins the compile-time error for
// unknown type.<name> refs at each consumer, mirroring the outcome-schema
// strictness (validateOutputExprStepOutputRefs).
func TestNamedType_UnknownRefDiagnostics_Golden(t *testing.T) {
	dir := calleeFixture(t, calleeWorkflow)
	cases := []struct {
		name, block, want string
	}{
		{
			name:  "data",
			block: `data "internal" "cfg" {\n  type  = type.missing\n  value = {}\n}`,
			want:  `data "internal" "cfg": unknown workflow type "missing"`,
		},
		{
			name:  "variable",
			block: `variable "deploy" {\n  type = type.missing\n}`,
			want:  `variable "deploy": unknown workflow type "missing"`,
		},
		{
			name:  "output",
			block: `output "deploy_summary" {\n  value = "x"\n  type  = type.missing\n}`,
			want:  `output "deploy_summary": unknown workflow type "missing"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := consumerHeader + tc.block + `
state "done" {
  terminal = true
  success  = true
}
`
			_, diags := compileAllConsumers(t, strings.ReplaceAll(src, `\n`, "\n"), dir)
			goldenCheck(t, diags, tc.want)
			for _, d := range diags {
				if strings.Contains(d.Summary, tc.want) && !strings.Contains(d.Detail, "may reference a top-level type block declared in this workflow") {
					t.Fatalf("unknown-type detail hint missing: %s / %s", d.Summary, d.Detail)
				}
			}
		})
	}
}

// TestNamedType_TypesDoNotShadowValueNamespaces pins that the type namespace
// is its own: a type may share a name with a variable, local, data block, or
// step without collision, because type refs only resolve in type-constraint
// positions and value refs only resolve in the var/local/data namespaces.
func TestNamedType_TypesDoNotShadowValueNamespaces(t *testing.T) {
	dir := calleeFixture(t, calleeWorkflow)
	src := consumerHeader + `
type "steps" {
  schema = object({ phase = string })
}
type "region" {
  schema = string
}
data "internal" "region" {
  type  = type.steps
  value = { phase = "build" }
}
variable "steps" {
  type    = type.region
  default = "primary"
}
local "cfg" { value = "eu-west" }
state "done" {
  terminal = true
  success  = true
}
`
	g, diags := compileAllConsumers(t, src, dir)
	if diags.HasErrors() {
		t.Fatalf("type/value namespace collision rejected: %s", diags)
	}
	if !g.Data["internal"]["region"].Type.Equals(cty.Object(map[string]cty.Type{"phase": cty.String})) {
		t.Fatalf("data did not resolve type.steps: %s", g.Data["internal"]["region"].Type.FriendlyName())
	}
	if !g.Variables["steps"].Type.Equals(cty.String) {
		t.Fatalf("variable did not resolve type.region: %s", g.Variables["steps"].Type.FriendlyName())
	}
}

// compositionDiagCheck asserts the mis-scoped-composition diagnostic itself:
// a summary naming the consumer loc AND a detail explaining the leaf-only
// composition rule. This cannot be satisfied by a downstream value-coercion
// error (different summary/detail), so a regression that silently drops a
// composed wrapper — resolving list(type.payload) to type.payload — fails
// here.
func compositionDiagCheck(t *testing.T, diags hcl.Diagnostics, wantSummary, wantDetail string) {
	t.Helper()
	if !diags.HasErrors() {
		t.Fatalf("expected error with summary %q, compile succeeded", wantSummary)
	}
	for _, d := range diags {
		if strings.Contains(d.Summary, wantSummary) && strings.Contains(d.Detail, wantDetail) {
			return
		}
	}
	t.Fatalf("expected diagnostic with summary containing %q and detail containing %q, got: %s", wantSummary, wantDetail, diags)
}

// TestNamedType_MisScoped_RefInsideTypeConstraint pins the mis-scoped usage:
// composing type.<name> into an inline constraint (list(type.payload)) is not
// a valid constraint — type refs are leaf positions only (slice-1 spirit).
// Cases include a variable with no default and no value-bearing attribute, so
// the assertion must be satisfied by the constraint diagnostic itself (any
// value coercion is impossible).
func TestNamedType_MisScoped_RefInsideTypeConstraint(t *testing.T) {
	dir := calleeFixture(t, calleeWorkflow)
	const composedDetail = "valid only as the whole type constraint"
	cases := []struct {
		name, block, wantSummary string
	}{
		{
			"data list composition",
			`data "internal" "cfg" {
  type  = list(type.payload)
  value = []
}`,
			`data "internal" "cfg"`,
		},
		{
			"data object composition",
			`data "internal" "cfg" {
  type  = object({ zone = type.payload })
  value = {}
}`,
			`data "internal" "cfg"`,
		},
		{
			"data optional composition",
			`data "internal" "cfg" {
  type  = optional(type.payload)
  value = null
}`,
			`data "internal" "cfg"`,
		},
		{
			// No default and no initial value: nothing downstream can produce
			// a value-coercion error, so the composition diagnostic must carry
			// this failure on its own.
			"variable composition without default",
			`variable "amount" {
  type = list(type.payload)
}`,
			`variable "amount"`,
		},
		{
			"output composition",
			`output "spent" {
  value = 1
  type  = map(type.payload)
}`,
			`output "spent"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := consumerHeader + consumerPayloadTypeBlock + tc.block + `
state "done" {
  terminal = true
  success  = true
}
`
			_, diags := compileAllConsumers(t, src, dir)
			compositionDiagCheck(t, diags, tc.wantSummary, composedDetail)
		})
	}

	t.Run("positive control: bare ref compiles clean", func(t *testing.T) {
		src := consumerHeader + consumerPayloadTypeBlock + `
data "internal" "cfg" {
  type = type.payload
}
state "done" {
  terminal = true
  success  = true
}
`
		g, diags := compileAllConsumers(t, src, dir)
		if diags.HasErrors() {
			t.Fatalf("bare type.<name> ref must compile clean, got: %s", diags)
		}
		if got := g.Data["internal"]["cfg"].Type.FriendlyName(); !strings.HasPrefix(got, "object") {
			t.Fatalf("bare ref must resolve to the declared type, got: %s", got)
		}
	})

	t.Run("positive control: composed built-ins still fine", func(t *testing.T) {
		src := consumerHeader + consumerPayloadTypeBlock + `
data "internal" "cfg" {
  type  = list(number)
  value = [1]
}
state "done" {
  terminal = true
  success  = true
}
`
		_, diags := compileAllConsumers(t, src, dir)
		if diags.HasErrors() {
			t.Fatalf("inline composed constraint without type refs must compile clean, got: %s", diags)
		}
	})
}

// TestNamedType_MisScoped_RefInValueExpression pins that type.<name> is not a
// value: referencing it where data values are computed fails the fold with the
// standard unknown-variable diagnostic.
func TestNamedType_MisScoped_RefInValueExpression(t *testing.T) {
	dir := calleeFixture(t, calleeWorkflow)
	src := consumerHeader + consumerPayloadTypeBlock + `
data "internal" "cfg" {
  type  = object({ zone = string })
  value = type.payload
}
state "done" {
  terminal = true
  success  = true
}
`
	_, diags := compileAllConsumers(t, src, dir)
	// The fold evaluator only knows var/local (and each/data snapshot) roots,
	// so type.<name> is an unknown variable when referenced as a value.
	goldenCheck(t, diags, "Unknown variable")
	for _, d := range diags {
		if d.Summary == "Unknown variable" && strings.Contains(d.Error(), `There is no variable named "type"`) {
			return
		}
	}
	t.Fatalf("expected unknown-variable diagnostic naming root type, got: %s", diags)
}

// TestNamedType_SubworkflowBindingResolvesCalleeTypes pins the subworkflow
// consumer: the callee declares its variable with a named type, and the
// parent's binding type-compat check runs against the RESOLVED type (number),
// rejecting incompatible bindings exactly like the inline twin.
func TestNamedType_SubworkflowBindingResolvesCalleeTypes(t *testing.T) {
	calleeNamed := `
workflow {
  name          = "child"
  version       = "0.1"
  initial_state = "done"
  target_state  = "done"
}
type "gauge" {
  schema = number
}
variable "amount" {
  type = type.gauge
}
state "done" {
  terminal = true
  success  = true
}
`
	dir := calleeFixture(t, calleeNamed)
	src := consumerHeader + `
subworkflow "child" {
  source = "callee"
  input = {
    amount = "not-a-number"
  }
}
state "done" {
  terminal = true
  success  = true
}
`
	_, diags := compileAllConsumers(t, src, dir)
	goldenCheck(t, diags, `subworkflow "child": input "amount"`)
}

// TestNamedType_ParentTypesNotVisibleInCallee pins the namespace boundary:
// a callee references the PARENT's type block — the callee compiles its own
// type namespace, so the ref is an unknown workflow type there.
func TestNamedType_ParentTypesNotVisibleInCallee(t *testing.T) {
	calleeBad := `
workflow {
  name          = "child"
  version       = "0.1"
  initial_state = "done"
  target_state  = "done"
}
variable "region" {
  type = type.parentonly
}
state "done" {
  terminal = true
  success  = true
}
`
	dir := calleeFixture(t, calleeBad)
	src := consumerHeader + `
type "parentonly" {
  schema = string
}
subworkflow "child" {
  source = "callee"
  input = {
    region = "us-east"
  }
}
state "done" {
  terminal = true
  success  = true
}
`
	_, diags := compileAllConsumers(t, src, dir)
	goldenCheck(t, diags, `variable "region": unknown workflow type "parentonly"`)
}

// TestNamedType_SubworkflowTwinBindings pins that parent bindings to callee
// variables validate identically whether the callee types them inline or via
// named refs: the same binding compiles in both twins.
func TestNamedType_SubworkflowTwinBindings(t *testing.T) {
	calleeInline := `
workflow {
  name          = "child"
  version       = "0.1"
  initial_state = "done"
  target_state  = "done"
}
variable "amount" {
  type = number
}
state "done" {
  terminal = true
  success  = true
}
`
	for name, callee := range map[string]string{"inline": calleeInline, "named": `
workflow {
  name          = "child"
  version       = "0.1"
  initial_state = "done"
  target_state  = "done"
}
type "gauge" {
  schema = number
}
variable "amount" {
  type = type.gauge
}
state "done" {
  terminal = true
  success  = true
}
`} {
		t.Run(name, func(t *testing.T) {
			dir := calleeFixture(t, callee)
			src := consumerHeader + `
subworkflow "child" {
  source = "callee"
  input = {
    amount = 42
  }
}
state "done" {
  terminal = true
  success  = true
}
`
			_, diags := compileAllConsumers(t, src, dir)
			if diags.HasErrors() {
				t.Fatalf("%s-typed callee rejected an identical binding: %s", name, diags)
			}
		})
	}
}
