package workflow

// compile_data_test.go — compile-time validation tests for data blocks and
// write blocks introduced by WS02.

import (
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
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

// TestCompileWrite_ReadsWrittenData_Warns verifies that a write whose value
// reads a data value the same step also writes produces an informational
// warning (not an error) explaining the snapshot semantics.
func TestCompileWrite_ReadsWrittenData_Warns(t *testing.T) {
	src := minimalWorkflowWithData(`
data "internal" "counter" {
  type  = number
  value = 0
}
`, `
outcome "success" {
  next = step.done
  write {
    target = data.internal.counter.value
    value  = data.internal.counter.value + 1
  }
}
`)
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile should not error, got: %s", diags.Error())
	}
	var found bool
	for _, d := range diags {
		if d.Severity == hcl.DiagWarning && strings.Contains(d.Summary, "reads data.internal.counter, which this step also writes") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected snapshot-semantics warning, got diags: %s", diags.Error())
	}
}

// TestCompileWrite_ReadsOtherData_NoWarn verifies that reading a data value the
// step does not write produces no snapshot-semantics warning.
func TestCompileWrite_ReadsOtherData_NoWarn(t *testing.T) {
	src := minimalWorkflowWithData(`
data "internal" "counter" {
  type  = number
  value = 0
}
data "internal" "seed" {
  type  = number
  value = 5
}
`, `
outcome "success" {
  next = step.done
  write {
    target = data.internal.counter.value
    value  = data.internal.seed.value + 1
  }
}
`)
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, nil)
	for _, d := range diags {
		if d.Severity == hcl.DiagWarning && strings.Contains(d.Summary, "which this step also writes") {
			t.Errorf("did not expect snapshot-semantics warning, got: %s", d.Summary)
		}
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
