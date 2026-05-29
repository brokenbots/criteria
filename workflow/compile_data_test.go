package workflow

// compile_data_test.go — compile-time validation tests for data blocks and
// write blocks introduced by WS02.

import (
	"strings"
	"testing"
)

// minimalWorkflowWithData is a minimal compilable workflow preamble that
// includes a data block and a step referencing it.
func minimalWorkflowWithData(dataBody, stepBody string) string {
	return `
workflow {
  name = "t"
  version       = "0.1"
  initial_state = "work"
  target_state  = "done"
}
` + dataBody + `
adapter "noop" "default" {}
step "work" {
  target = adapter.noop.default
  ` + stepBody + `
}
state "done" {
  terminal = true
  success  = true
}
`
}

// TestCompileData_UnsupportedKind verifies that data kinds other than
// "internal" are rejected at compile time.
func TestCompileData_UnsupportedKind(t *testing.T) {
	src := minimalWorkflowWithData(`
data "http" "x" {
  type = string
}
`, `outcome "success" { next = step.done }`)
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for unsupported data kind, got none")
	}
	if !strings.Contains(diags.Error(), "unsupported data kind") {
		t.Errorf("error should mention 'unsupported data kind', got: %s", diags.Error())
	}
}

// TestCompileData_NameCollision_Variable verifies that a data block whose
// name clashes with a declared variable is rejected.
func TestCompileData_NameCollision_Variable(t *testing.T) {
	src := `
workflow {
  name = "t"
  version       = "0.1"
  initial_state = "work"
  target_state  = "done"
}
variable "counter" {
  type = number
}
data "internal" "counter" {
  type = number
}
adapter "noop" "default" {}
step "work" {
  target = adapter.noop.default
  outcome "success" { next = step.done }
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
	_, diags = Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for data name colliding with variable, got none")
	}
	if !strings.Contains(diags.Error(), "conflicts with a declared variable") {
		t.Errorf("error should mention 'conflicts with a declared variable', got: %s", diags.Error())
	}
}

// TestCompileData_NameCollision_Local verifies that a data block whose
// name clashes with a declared local is rejected.
func TestCompileData_NameCollision_Local(t *testing.T) {
	src := `
workflow {
  name = "t"
  version       = "0.1"
  initial_state = "work"
  target_state  = "done"
}
local "counter" {
  value = 1
}
data "internal" "counter" {
  type = number
}
adapter "noop" "default" {}
step "work" {
  target = adapter.noop.default
  outcome "success" { next = step.done }
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
	_, diags = Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for data name colliding with local, got none")
	}
	if !strings.Contains(diags.Error(), "conflicts with a declared local") {
		t.Errorf("error should mention 'conflicts with a declared local', got: %s", diags.Error())
	}
}

// TestCompileData_NameCollision_Duplicate verifies that duplicate data blocks
// with the same kind+name are rejected.
func TestCompileData_NameCollision_Duplicate(t *testing.T) {
	src := `
workflow {
  name = "t"
  version       = "0.1"
  initial_state = "work"
  target_state  = "done"
}
data "internal" "counter" {
  type = number
}
data "internal" "counter" {
  type = string
}
adapter "noop" "default" {}
step "work" {
  target = adapter.noop.default
  outcome "success" { next = step.done }
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
	_, diags = Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for duplicate data block, got none")
	}
	if !strings.Contains(diags.Error(), "duplicate data") {
		t.Errorf("error should mention 'duplicate data', got: %s", diags.Error())
	}
}

// TestCompileWrite_TargetNotDeclared verifies that a write block whose
// target references a non-existent data block is rejected.
func TestCompileWrite_TargetNotDeclared(t *testing.T) {
	src := minimalWorkflowWithData(`
data "internal" "counter" {
  type = number
}
`, `
outcome "success" {
  next = step.done
  write {
    target = data.internal.nonexistent.value
    value  = "x"
  }
}
`)
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for write target not declared, got none")
	}
	if !strings.Contains(diags.Error(), "is not declared") {
		t.Errorf("error should mention 'is not declared', got: %s", diags.Error())
	}
}

// TestCompileWrite_MalformedTraversal verifies that a write block whose
// target is not exactly data.<kind>.<name>.value is rejected.
func TestCompileWrite_MalformedTraversal(t *testing.T) {
	src := minimalWorkflowWithData(`
data "internal" "counter" {
  type = number
}
`, `
outcome "success" {
  next = step.done
  write {
    target = data.internal.counter
    value  = "y"
  }
}
`)
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for malformed write target, got none")
	}
	if !strings.Contains(diags.Error(), "target must be a traversal of the form data.<kind>.<name>.value") {
		t.Errorf("error should mention 'target must be a traversal of the form data.<kind>.<name>.value', got: %s", diags.Error())
	}
}
