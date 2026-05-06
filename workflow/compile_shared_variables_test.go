package workflow

// compile_shared_variables_test.go — unit tests for compileSharedVariables.

import (
	"strings"
	"testing"

	"github.com/zclconf/go-cty/cty"
)

// sharedVarWorkflow wraps a snippet into a minimal compilable workflow HCL.
func sharedVarWorkflow(sharedBlocks, extraBlocks string) string {
	return `workflow "test" {
  version       = "0.1"
  initial_state = "done"
  target_state  = "done"
}

state "done" {
  terminal = true
  success  = true
}
` + extraBlocks + sharedBlocks
}

func TestCompileSharedVariables_Simple(t *testing.T) {
	src := sharedVarWorkflow(`
shared_variable "counter" {
  type  = "number"
  value = 0
}
`, "")
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	if len(g.SharedVariables) != 1 {
		t.Fatalf("expected 1 shared variable, got %d", len(g.SharedVariables))
	}
	sv, ok := g.SharedVariables["counter"]
	if !ok {
		t.Fatal("shared_variable \"counter\" not found")
	}
	if sv.Type != cty.Number {
		t.Errorf("expected type number, got %s", sv.Type.FriendlyName())
	}
	if sv.InitialValue == cty.NilVal || sv.InitialValue.IsNull() {
		t.Fatal("expected non-null initial value")
	}
	bf := sv.InitialValue.AsBigFloat()
	f, _ := bf.Float64()
	if f != 0 {
		t.Errorf("expected initial value 0, got %v", f)
	}
}

func TestCompileSharedVariables_StringType(t *testing.T) {
	src := sharedVarWorkflow(`
shared_variable "status" {
  type  = "string"
  value = "pending"
}
`, "")
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	sv := g.SharedVariables["status"]
	if sv == nil {
		t.Fatal("shared_variable \"status\" not found")
	}
	if sv.Type != cty.String {
		t.Errorf("expected type string, got %s", sv.Type.FriendlyName())
	}
	if sv.InitialValue.AsString() != "pending" {
		t.Errorf("expected initial value %q, got %q", "pending", sv.InitialValue.AsString())
	}
}

func TestCompileSharedVariables_NoInitialValue(t *testing.T) {
	src := sharedVarWorkflow(`
shared_variable "flag" {
  type = "bool"
}
`, "")
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	sv := g.SharedVariables["flag"]
	if sv == nil {
		t.Fatal("shared_variable \"flag\" not found")
	}
	// No value declared: InitialValue must be cty.NullVal(cty.Bool).
	if sv.InitialValue == cty.NilVal {
		t.Fatal("expected cty.NullVal(Bool), got cty.NilVal")
	}
	if !sv.InitialValue.IsNull() {
		t.Errorf("expected null initial value when not declared, got %v", sv.InitialValue)
	}
	if sv.InitialValue.Type() != cty.Bool {
		t.Errorf("expected null value of type bool, got %s", sv.InitialValue.Type().FriendlyName())
	}
}

func TestCompileSharedVariables_TypeRequired(t *testing.T) {
	src := sharedVarWorkflow(`
shared_variable "bad" {
  value = "x"
}
`, "")
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for missing type")
	}
	if !strings.Contains(diags.Error(), "type") {
		t.Errorf("expected error about type, got: %s", diags.Error())
	}
}

func TestCompileSharedVariables_TypeMismatch(t *testing.T) {
	src := sharedVarWorkflow(`
shared_variable "bad" {
  type  = "number"
  value = "not-a-number"
}
`, "")
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for type mismatch")
	}
}

func TestCompileSharedVariables_NameCollisionWithVariable(t *testing.T) {
	src := sharedVarWorkflow(`
shared_variable "env" {
  type = "string"
}
`, `
variable "env" {
  type    = "string"
  default = "dev"
}
`)
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for name collision with variable")
	}
	if !strings.Contains(diags.Error(), "env") {
		t.Errorf("expected error mentioning name %q, got: %s", "env", diags.Error())
	}
}

func TestCompileSharedVariables_NameCollisionWithLocal(t *testing.T) {
	src := sharedVarWorkflow(`
shared_variable "greeting" {
  type = "string"
}
`, `
local "greeting" {
  value = "hello"
}
`)
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for name collision with local")
	}
	if !strings.Contains(diags.Error(), "greeting") {
		t.Errorf("expected error mentioning name %q, got: %s", "greeting", diags.Error())
	}
}

func TestCompileSharedVariables_DuplicateDeclaration(t *testing.T) {
	src := sharedVarWorkflow(`
shared_variable "counter" {
  type = "number"
}
shared_variable "counter" {
  type = "string"
}
`, "")
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for duplicate shared_variable")
	}
}

func TestCompileSharedVariables_Order(t *testing.T) {
	src := sharedVarWorkflow(`
shared_variable "alpha" {
  type = "string"
}
shared_variable "beta" {
  type = "number"
}
`, "")
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	if len(g.SharedVariableOrder) != 2 {
		t.Fatalf("expected 2 in SharedVariableOrder, got %d", len(g.SharedVariableOrder))
	}
	if g.SharedVariableOrder[0] != "alpha" || g.SharedVariableOrder[1] != "beta" {
		t.Errorf("unexpected order: %v", g.SharedVariableOrder)
	}
}

func TestCompileSharedWrites_ValidKey(t *testing.T) {
	src := `
workflow "test" {
  version       = "0.1"
  initial_state = "inc"
  target_state  = "done"
}

state "done" {
  terminal = true
  success  = true
}

shared_variable "counter" {
  type = "number"
}

adapter "noop" "default" {}

step "inc" {
  target = adapter.noop.default
  outcome "done" {
    next         = "done"
    shared_writes = { counter = "count_val" }
  }
}
`
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	stepNode := g.Steps["inc"]
	if stepNode == nil {
		t.Fatal("step \"inc\" not found")
	}
	outcome := stepNode.Outcomes["done"]
	if outcome == nil {
		t.Fatal("outcome \"done\" not found")
	}
	if outcome.SharedWrites["counter"] != "count_val" {
		t.Errorf("expected shared_writes[counter]=count_val, got %v", outcome.SharedWrites["counter"])
	}
}

func TestCompileSharedWrites_UnknownKey(t *testing.T) {
	src := `
workflow "test" {
  version       = "0.1"
  initial_state = "inc"
  target_state  = "done"
}

state "done" {
  terminal = true
  success  = true
}

shared_variable "counter" {
  type = "number"
}

adapter "noop" "default" {}

step "inc" {
  target = adapter.noop.default
  outcome "done" {
    next         = "done"
    shared_writes = { not_declared = "val" }
  }
}
`
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for unknown shared_writes key")
	}
	if !strings.Contains(diags.Error(), "not_declared") {
		t.Errorf("expected error mentioning %q, got: %s", "not_declared", diags.Error())
	}
}

// TestCompileSharedWrites_OutputKeyNotInProjection tests that shared_writes
// referencing an output key absent from the outcome's output projection
// is rejected at compile time.
func TestCompileSharedWrites_OutputKeyNotInProjection(t *testing.T) {
	src := `
workflow "test" {
  version       = "0.1"
  initial_state = "inc"
  target_state  = "done"
}

state "done" {
  terminal = true
  success  = true
}

shared_variable "counter" {
  type = "number"
}

adapter "noop" "default" {}

step "inc" {
  target = adapter.noop.default
  outcome "done" {
    next         = "done"
    output       = { actual_key = "literal" }
    shared_writes = { counter = "wrong_key" }
  }
}
`
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for output key not in projection")
	}
	if !strings.Contains(diags.Error(), "wrong_key") {
		t.Errorf("expected error mentioning %q, got: %s", "wrong_key", diags.Error())
	}
}

// TestCompileSharedWrites_OutputKeyInProjection tests that shared_writes
// referencing a key declared in the outcome's output projection compiles cleanly.
func TestCompileSharedWrites_OutputKeyInProjection(t *testing.T) {
	src := `
workflow "test" {
  version       = "0.1"
  initial_state = "inc"
  target_state  = "done"
}

state "done" {
  terminal = true
  success  = true
}

shared_variable "counter" {
  type = "number"
}

adapter "noop" "default" {}

step "inc" {
  target = adapter.noop.default
  outcome "done" {
    next         = "done"
    output       = { count_val = "0" }
    shared_writes = { counter = "count_val" }
  }
}
`
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	outcome := g.Steps["inc"].Outcomes["done"]
	if outcome.SharedWrites["counter"] != "count_val" {
		t.Errorf("expected shared_writes[counter]=count_val, got %v", outcome.SharedWrites["counter"])
	}
}

// TestCompileSharedWrites_OutputKeyNotInAdapterSchema tests that when no
// output projection exists but the adapter declares an output schema,
// shared_writes values not in the schema are rejected at compile time.
func TestCompileSharedWrites_OutputKeyNotInAdapterSchema(t *testing.T) {
	src := `
workflow "test" {
  version       = "0.1"
  initial_state = "inc"
  target_state  = "done"
}

state "done" {
  terminal = true
  success  = true
}

shared_variable "counter" {
  type = "number"
}

adapter "noop" "default" {}

step "inc" {
  target = adapter.noop.default
  outcome "done" {
    next         = "done"
    shared_writes = { counter = "nonexistent_output" }
  }
}
`
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	// Pass a schema declaring specific output keys.
	schemas := map[string]AdapterInfo{
		"noop.default": {
			OutputSchema: map[string]ConfigField{
				"count_lines": {},
			},
		},
	}
	_, diags = Compile(spec, schemas)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for output key not in adapter schema")
	}
	if !strings.Contains(diags.Error(), "nonexistent_output") {
		t.Errorf("expected error mentioning %q, got: %s", "nonexistent_output", diags.Error())
	}
}

// TestCompileSharedWrites_OutputKeyInAdapterSchema tests that when no output
// projection exists and the adapter schema declares the output key, compilation
// succeeds.
func TestCompileSharedWrites_OutputKeyInAdapterSchema(t *testing.T) {
	src := `
workflow "test" {
  version       = "0.1"
  initial_state = "inc"
  target_state  = "done"
}

state "done" {
  terminal = true
  success  = true
}

shared_variable "counter" {
  type = "number"
}

adapter "noop" "default" {}

step "inc" {
  target = adapter.noop.default
  outcome "done" {
    next         = "done"
    shared_writes = { counter = "count_lines" }
  }
}
`
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	schemas := map[string]AdapterInfo{
		"noop.default": {
			OutputSchema: map[string]ConfigField{
				"count_lines": {},
			},
		},
	}
	g, diags := Compile(spec, schemas)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	outcome := g.Steps["inc"].Outcomes["done"]
	if outcome.SharedWrites["counter"] != "count_lines" {
		t.Errorf("expected shared_writes[counter]=count_lines, got %v", outcome.SharedWrites["counter"])
	}
}

// TestCompileSharedWrites_NoSchemaNoProjection_Permissive tests that when no
// output projection and no adapter schema is available, any output key is
// accepted (permissive — runtime validation only).
func TestCompileSharedWrites_NoSchemaNoProjection_Permissive(t *testing.T) {
	src := `
workflow "test" {
  version       = "0.1"
  initial_state = "inc"
  target_state  = "done"
}

state "done" {
  terminal = true
  success  = true
}

shared_variable "counter" {
  type = "number"
}

adapter "noop" "default" {}

step "inc" {
  target = adapter.noop.default
  outcome "done" {
    next         = "done"
    shared_writes = { counter = "any_adapter_output" }
  }
}
`
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	// No schemas — permissive, any output key accepted.
	g, diags := Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("expected no compile error in permissive mode: %s", diags.Error())
	}
	outcome := g.Steps["inc"].Outcomes["done"]
	if outcome.SharedWrites["counter"] != "any_adapter_output" {
		t.Errorf("expected shared_writes[counter]=any_adapter_output, got %v", outcome.SharedWrites["counter"])
	}
}
