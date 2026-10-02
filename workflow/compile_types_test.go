package workflow

// compile_types_test.go — tests for workflow-level type "name" blocks (KB-45).

import (
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
)

// compileTypesFromHCL parses full workflow HCL source and runs the compileTypes
// pass against the resulting Spec.
func compileTypesFromHCL(t *testing.T, src string) (*FSMGraph, hcl.Diagnostics) {
	t.Helper()
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("spec parse failed: %s", diags)
	}
	g := newFSMGraph(spec)
	return g, compileTypes(g, spec)
}

func TestCompileTypesObjectWithDefaults(t *testing.T) {
	src := `
workflow {
  name          = "t"
  version       = "0.1"
  initial_state = "a"
  target_state  = "done"
}
type "payload" {
  schema = object({
    summary = optional(string, "unchanged")
    details = optional(string)
  })
}
`
	g, diags := compileTypesFromHCL(t, src)
	if diags.HasErrors() {
		t.Fatalf("unexpected errors: %s", diags)
	}
	decl, ok := g.Types["payload"]
	if !ok {
		t.Fatal("type payload not registered")
	}
	obj := decl.Type.AttributeType("summary")
	if obj != cty.String {
		t.Fatalf("unexpected summary type: %v", decl.Type)
	}
	if decl.Defaults == nil {
		t.Fatal("expected non-nil Defaults for optional(..., default)")
	}
	val := decl.Defaults.Apply(cty.EmptyObjectVal)
	sum := val.GetAttr("summary")
	if sum.AsString() != "unchanged" {
		t.Fatalf("defaults not applied: %v", sum)
	}
}

func TestCompileTypesOrderPreserved(t *testing.T) {
	src := `
workflow {
  name          = "t"
  version       = "0.1"
  initial_state = "a"
  target_state  = "done"
}
type "zzz" {
  schema = string
}
type "aaa" {
  schema = list(number)
}
`
	g, diags := compileTypesFromHCL(t, src)
	if diags.HasErrors() {
		t.Fatalf("unexpected errors: %s", diags)
	}
	if len(g.TypeOrder) != 2 || g.TypeOrder[0] != "zzz" || g.TypeOrder[1] != "aaa" {
		t.Fatalf("type order not declaration order: %v", g.TypeOrder)
	}
}

func TestCompileTypesDuplicate(t *testing.T) {
	src := `
workflow {
  name          = "t"
  version       = "0.1"
  initial_state = "a"
  target_state  = "done"
}
type "a" {
  schema = string
}
type "a" {
  schema = number
}
`
	_, diags := compileTypesFromHCL(t, src)
	if !diags.HasErrors() {
		t.Fatal("expected duplicate type error")
	}
	found := false
	for _, d := range diags {
		if strings.Contains(d.Summary, `"a": duplicate type declaration`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected duplicate diagnostic, got: %s", diags)
	}
}

func TestCompileTypesSlice1NoTypeRefs(t *testing.T) {
	src := `
workflow {
  name          = "t"
  version       = "0.1"
  initial_state = "a"
  target_state  = "done"
}
type "a" {
  schema = object({
    x = type.b
  })
}
`
	_, diags := compileTypesFromHCL(t, src)
	if !diags.HasErrors() {
		t.Fatal("expected slice-1 error for type-to-type reference")
	}
	found := false
	for _, d := range diags {
		if strings.Contains(d.Summary, `"a": type blocks cannot reference other type blocks`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected slice-1 diagnostic, got: %s", diags)
	}
}

func TestCompileTypesExtraAttributeRejected(t *testing.T) {
	src := `
workflow {
  name          = "t"
  version       = "0.1"
  initial_state = "a"
  target_state  = "done"
}
type "a" {
  schema = string
  description = "nope"
}
`
	_, diags := compileTypesFromHCL(t, src)
	if !diags.HasErrors() {
		t.Fatal("expected unsupported attribute error")
	}
	found := false
	for _, d := range diags {
		if strings.Contains(d.Summary, `"a": unsupported attribute "description"`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected unsupported-attribute diagnostic, got: %s", diags)
	}
}

func TestCompileTypesInvalidName(t *testing.T) {
	src := `
workflow {
  name          = "t"
  version       = "0.1"
  initial_state = "a"
  target_state  = "done"
}
type "not valid" {
  schema = string
}
`
	_, diags := compileTypesFromHCL(t, src)
	if !diags.HasErrors() {
		t.Fatal("expected invalid type name error")
	}
}
