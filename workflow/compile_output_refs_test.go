package workflow

import (
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
)

// outputTestSchema is an adapter schema with output fields for compile-time
// output-reference validation tests.
var outputTestSchema = AdapterInfo{
	InputSchema: map[string]ConfigField{
		"command": {Required: true, Type: ConfigFieldString},
	},
	OutputSchema: map[string]ConfigField{
		"result":  {Type: ConfigFieldString},
		"status":  {Type: ConfigFieldString},
		"token":   {Type: ConfigFieldString, Sensitive: true},
		"exit":    {Type: ConfigFieldNumber},
	},
}

// TestCompileOutputRefs_Valid verifies that a well-formed steps.X.outputs.Y
// reference compiles without error when Y exists in the adapter's output schema.
func TestCompileOutputRefs_Valid(t *testing.T) {
	src := `
workflow {
  name = "x"
  version       = "0.1"
  initial_state = "first"
  target_state  = "done"
}

adapter "shell" "default" {}

step "first" {
  target = adapter.shell.default
  input { command = "echo hi" }
  outcome "success" { next = "second" }
}

step "second" {
  target = adapter.shell.default
  input { command = steps.first.outputs.result }
  outcome "success" { next = "done" }
}

state "done" { terminal = true }
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := Compile(spec, map[string]AdapterInfo{"shell": outputTestSchema})
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	if g.Steps["second"] == nil {
		t.Fatal("step 'second' not found")
	}
}

// TestCompileOutputRefs_InvalidField verifies that referencing an unknown
// output field produces an error with a diagnostic.
func TestCompileOutputRefs_InvalidField(t *testing.T) {
	src := `
workflow {
  name = "x"
  version       = "0.1"
  initial_state = "first"
  target_state  = "done"
}

adapter "shell" "default" {}

step "first" {
  target = adapter.shell.default
  input { command = "echo hi" }
  outcome "success" { next = "second" }
}

step "second" {
  target = adapter.shell.default
  input { command = steps.first.outputs.not_a_field }
  outcome "success" { next = "done" }
}

state "done" { terminal = true }
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, map[string]AdapterInfo{"shell": outputTestSchema})
	if !diags.HasErrors() {
		t.Fatal("expected compile error for output field")
	}
	found := false
	for _, d := range diags {
		if d.Severity == hcl.DiagError && strings.Contains(d.Summary, "output field") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected output field error, got: %s", diags.Error())
	}
}

// TestCompileOutputRefs_MisspelledField verifies that a misspelled output
// field name produces an error with a Levenshtein-sorted suggestion list.
func TestCompileOutputRefs_MisspelledField(t *testing.T) {
	src := `
workflow {
  name = "x"
  version       = "0.1"
  initial_state = "first"
  target_state  = "done"
}

adapter "shell" "default" {}

step "first" {
  target = adapter.shell.default
  input { command = "echo hi" }
  outcome "success" { next = "second" }
}

step "second" {
  target = adapter.shell.default
  input { command = steps.first.outputs.reslt }
  outcome "success" { next = "done" }
}

state "done" { terminal = true }
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, map[string]AdapterInfo{"shell": outputTestSchema})
	if !diags.HasErrors() {
		t.Fatal("expected compile error for misspelled output field")
	}
	msg := diags.Error()
	if !strings.Contains(msg, "output field") {
		t.Fatalf("expected output field error, got: %s", msg)
	}
	// Levenshtein distance should suggest "result" first (distance 1).
	if !strings.Contains(msg, "result") {
		t.Fatalf("expected suggestion 'result' in error message, got: %s", msg)
	}
}

// TestCompileOutputRefs_SwitchMatch verifies that output references inside
// switch match expressions are validated.
func TestCompileOutputRefs_SwitchMatch(t *testing.T) {
	src := `
workflow {
  name = "x"
  version       = "0.1"
  initial_state = "first"
  target_state  = "done"
}

adapter "shell" "default" {}

step "first" {
  target = adapter.shell.default
  input { command = "echo hi" }
  outcome "success" { next = "sw" }
}

switch "sw" {
  condition {
    match = steps.first.outputs.status == "ok"
    next  = "done"
  }
  default {
    next = "done"
  }
}

state "done" { terminal = true }
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := Compile(spec, map[string]AdapterInfo{"shell": outputTestSchema})
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	if g.Switches["sw"] == nil {
		t.Fatal("switch 'sw' not found")
	}
}

// TestCompileOutputRefs_SwitchMatchInvalid verifies that an invalid output
// reference inside a switch match expression produces an error.
func TestCompileOutputRefs_SwitchMatchInvalid(t *testing.T) {
	src := `
workflow {
  name = "x"
  version       = "0.1"
  initial_state = "first"
  target_state  = "done"
}

adapter "shell" "default" {}

step "first" {
  target = adapter.shell.default
  input { command = "echo hi" }
  outcome "success" { next = "sw" }
}

switch "sw" {
  condition {
    match = steps.first.outputs.bad == "ok"
    next  = "done"
  }
  default {
    next = "done"
  }
}

state "done" { terminal = true }
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, map[string]AdapterInfo{"shell": outputTestSchema})
	if !diags.HasErrors() {
		t.Fatal("expected compile error for invalid output field in switch match")
	}
	msg := diags.Error()
	if !strings.Contains(msg, "output field") {
		t.Fatalf("expected output field error, got: %s", msg)
	}
}

// TestCompileOutputRefs_TopLevelOutput verifies that output references in
// workflow-level output blocks are validated.
func TestCompileOutputRefs_TopLevelOutput(t *testing.T) {
	src := `
workflow {
  name = "x"
  version       = "0.1"
  initial_state = "first"
  target_state  = "done"
}

output "result" {
  value = steps.first.outputs.result
}

adapter "shell" "default" {}

step "first" {
  target = adapter.shell.default
  input { command = "echo hi" }
  outcome "success" { next = "done" }
}

state "done" { terminal = true }
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, map[string]AdapterInfo{"shell": outputTestSchema})
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
}

// TestCompileOutputRefs_TopLevelOutputInvalid verifies that an invalid
// output reference in a workflow-level output block produces an error.
func TestCompileOutputRefs_TopLevelOutputInvalid(t *testing.T) {
	src := `
workflow {
  name = "x"
  version       = "0.1"
  initial_state = "first"
  target_state  = "done"
}

output "result" {
  value = steps.first.outputs.nope
}

adapter "shell" "default" {}

step "first" {
  target = adapter.shell.default
  input { command = "echo hi" }
  outcome "success" { next = "done" }
}

state "done" { terminal = true }
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, map[string]AdapterInfo{"shell": outputTestSchema})
	if !diags.HasErrors() {
		t.Fatal("expected compile error for invalid output field in top-level output")
	}
	msg := diags.Error()
	if !strings.Contains(msg, "output field") {
		t.Fatalf("expected output field error, got: %s", msg)
	}
}

// TestCompileOutputRefs_UnknownStepSkipped verifies that references to
// unknown steps do NOT produce an output-ref error (they are handled by
// other passes or runtime resolution).
func TestCompileOutputRefs_UnknownStepSkipped(t *testing.T) {
	src := `
workflow {
  name = "x"
  version       = "0.1"
  initial_state = "done"
  target_state  = "done"
}

output "result" {
  value = steps.missing.outputs.result
}

state "done" { terminal = true }
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, map[string]AdapterInfo{"shell": outputTestSchema})
	// Should NOT error from compileOutputRefs because the step is unknown.
	if diags.HasErrors() {
		for _, d := range diags {
			if strings.Contains(d.Summary, "output field") {
				t.Fatalf("unexpected output-ref error for unknown step: %s", diags.Error())
			}
		}
	}
}

// TestCompileOutputRefs_NoOutputSchemaSkipped verifies that references to a
// step whose adapter has no OutputSchema are skipped without error.
func TestCompileOutputRefs_NoOutputSchemaSkipped(t *testing.T) {
	src := `
workflow {
  name = "x"
  version       = "0.1"
  initial_state = "first"
  target_state  = "done"
}

adapter "noop" "default" {}

step "first" {
  target = adapter.noop.default
  outcome "success" { next = "second" }
}

step "second" {
  target = adapter.noop.default
  input { command = steps.first.outputs.result }
  outcome "success" { next = "done" }
}

state "done" { terminal = true }
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, map[string]AdapterInfo{"noop": noopSchema})
	// Should NOT error because the first step has no OutputSchema.
	if diags.HasErrors() {
		for _, d := range diags {
			if strings.Contains(d.Summary, "output field") {
				t.Fatalf("unexpected output-ref error for step with no OutputSchema: %s", diags.Error())
			}
		}
	}
}

// TestCompileOutputRefs_OutcomeOutputExpr verifies that output references
// inside outcome output blocks are validated.
func TestCompileOutputRefs_OutcomeOutputExpr(t *testing.T) {
	src := `
workflow {
  name = "x"
  version       = "0.1"
  initial_state = "first"
  target_state  = "done"
}

adapter "shell" "default" {}

step "first" {
  target = adapter.shell.default
  input { command = "echo hi" }
  outcome "success" {
    output = { copied = steps.first.outputs.status }
    next   = "done"
  }
}

state "done" { terminal = true }
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, map[string]AdapterInfo{"shell": outputTestSchema})
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
}

// TestCompileOutputRefs_OutcomeOutputExprInvalid verifies that an invalid
// output reference inside an outcome output block produces an error.
func TestCompileOutputRefs_OutcomeOutputExprInvalid(t *testing.T) {
	src := `
workflow {
  name = "x"
  version       = "0.1"
  initial_state = "first"
  target_state  = "done"
}

adapter "shell" "default" {}

step "first" {
  target = adapter.shell.default
  input { command = "echo hi" }
  outcome "success" {
    output = { copied = steps.first.outputs.bogus }
    next   = "done"
  }
}

state "done" { terminal = true }
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, map[string]AdapterInfo{"shell": outputTestSchema})
	if !diags.HasErrors() {
		t.Fatal("expected compile error for invalid output field in outcome output")
	}
	msg := diags.Error()
	if !strings.Contains(msg, "output field") {
		t.Fatalf("expected output field error, got: %s", msg)
	}
}
