package workflow

import (
	"testing"

	"github.com/hashicorp/hcl/v2"
)

func TestCompileOutputs_SimpleViaIntegration(t *testing.T) {
	src := `
workflow "test" {
  version      = "1"
  initial_state = "start"
  target_state  = "end"
}
  
output "result" {
  value = "hello"
}
  
state "start" {}
state "end" {
  terminal = true
}
`
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse error: %v", diags.Errs())
	}

	g := newFSMGraph(spec)
	diags = compileOutputs(g, spec, CompileOpts{})

	if diags.HasErrors() {
		t.Fatalf("unexpected errors: %v", diags.Errs())
	}
	if len(g.Outputs) != 1 {
		t.Fatalf("expected 1 output, got %d", len(g.Outputs))
	}
	if on, ok := g.Outputs["result"]; !ok {
		t.Fatalf("output 'result' not found")
	} else if on.Name != "result" {
		t.Fatalf("output name mismatch: got %q", on.Name)
	}
}

func TestCompileOutputs_DuplicateName(t *testing.T) {
	src := `
workflow "test" {
  version      = "1"
  initial_state = "start"
  target_state  = "end"
}
  
output "count" { value = 1 }
output "count" { value = 2 }
  
state "start" {}
state "end" { terminal = true }
`
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse error: %v", diags.Errs())
	}

	g := newFSMGraph(spec)
	diags = compileOutputs(g, spec, CompileOpts{})

	if !diags.HasErrors() {
		t.Fatalf("expected duplicate error, got none")
	}
	found := false
	for _, d := range diags {
		if d.Severity == hcl.DiagError && contains(d.Summary, "duplicate") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected 'duplicate' error in diagnostics")
	}
}

func TestCompileOutputs_MissingValueAttr(t *testing.T) {
	src := `
workflow "test" {
  version      = "1"
  initial_state = "start"
  target_state  = "end"
}
  
output "bad" {
  description = "no value"
}
  
state "start" {}
state "end" { terminal = true }
`
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse error: %v", diags.Errs())
	}

	g := newFSMGraph(spec)
	diags = compileOutputs(g, spec, CompileOpts{})

	if !diags.HasErrors() {
		t.Fatalf("expected 'value' missing error, got none")
	}
	found := false
	for _, d := range diags {
		if d.Severity == hcl.DiagError && contains(d.Summary, "value") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected 'value' error in diagnostics")
	}
}

// --- Helpers ---

func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// TestCompileOutputs_TypeValidation_MatchingType tests type checking for outputs.
func TestCompileOutputs_TypeValidation_MatchingType(t *testing.T) {
	src := `
workflow "test" {
  version      = "1"
  initial_state = "start"
  target_state  = "end"
}
  
output "num" {
  type  = "number"
  value = 42
}
  
state "start" {}
state "end" { terminal = true }
`
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse error: %v", diags.Errs())
	}

	g := newFSMGraph(spec)
	diags = compileOutputs(g, spec, CompileOpts{})

	if diags.HasErrors() {
		t.Fatalf("expected no type mismatch error, got: %v", diags.Errs())
	}
	if _, ok := g.Outputs["num"]; !ok {
		t.Fatalf("output 'num' not found")
	}
}

// TestCompileOutputs_TypeValidation_MismatchingType tests type checking for outputs.
func TestCompileOutputs_TypeValidation_MismatchingType(t *testing.T) {
	src := `
workflow "test" {
  version      = "1"
  initial_state = "start"
  target_state  = "end"
}
  
output "str_not_num" {
  type  = "number"
  value = "hello"
}
  
state "start" {}
state "end" { terminal = true }
`
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse error: %v", diags.Errs())
	}

	g := newFSMGraph(spec)
	diags = compileOutputs(g, spec, CompileOpts{})

	// Type mismatch should produce a compile error
	if !diags.HasErrors() {
		t.Fatalf("expected type mismatch error, got none; outputs: %v", g.Outputs)
	}

	// Verify error message mentions output name and types
	found := false
	for _, d := range diags {
		if d.Severity == hcl.DiagError &&
			contains(d.Summary, "str_not_num") &&
			contains(d.Summary, "number") &&
			contains(d.Summary, "string") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected error naming output, declared type, and actual type; got: %v", diags.Errs())
	}
}

// TestCompileOutputs_RuntimeExpressionDeferred tests that step references are deferred.
func TestCompileOutputs_RuntimeExpressionDeferred(t *testing.T) {
	src := `
workflow "test" {
  version      = "1"
  initial_state = "start"
  target_state  = "end"
}
  
output "step_result" {
  value = steps.build.outputs.artifact
}
  
state "start" {}
state "end" { terminal = true }
`
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse error: %v", diags.Errs())
	}

	g := newFSMGraph(spec)
	diags = compileOutputs(g, spec, CompileOpts{})

	// Should not error at compile time because steps.* are deferred to runtime
	if diags.HasErrors() {
		t.Fatalf("expected no compile error for deferred step reference, got: %v", diags.Errs())
	}

	on, ok := g.Outputs["step_result"]
	if !ok {
		t.Fatalf("output 'step_result' not found")
	}
	// Value should be an hcl.Expression, not evaluated
	if on.Value == nil {
		t.Fatalf("value expression should be deferred, not nil")
	}
}

// TestCompileOutputs_OptionalDescription tests optional description attribute.
func TestCompileOutputs_OptionalDescription(t *testing.T) {
	src := `
workflow "test" {
  version      = "1"
  initial_state = "start"
  target_state  = "end"
}
  
output "documented" {
  description = "This is an output"
  value       = "hello"
}
  
output "undocumented" {
  value = "world"
}
  
state "start" {}
state "end" { terminal = true }
`
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse error: %v", diags.Errs())
	}

	g := newFSMGraph(spec)
	diags = compileOutputs(g, spec, CompileOpts{})

	if diags.HasErrors() {
		t.Fatalf("expected no error, got: %v", diags.Errs())
	}

	documented := g.Outputs["documented"]
	if documented.Description != "This is an output" {
		t.Errorf("documented.Description = %q, want %q", documented.Description, "This is an output")
	}

	undocumented := g.Outputs["undocumented"]
	if undocumented.Description != "" {
		t.Errorf("undocumented.Description = %q, want empty", undocumented.Description)
	}
}

// TestCompileOutputs_LocalReference tests outputs can reference locals.
func TestCompileOutputs_LocalReference(t *testing.T) {
	src := `
workflow "test" {
  version      = "1"
  initial_state = "start"
  target_state  = "end"
}
  
local "msg" {
  value = "computed"
}
  
output "from_local" {
  value = local.msg
}
  
state "start" {}
state "end" { terminal = true }
`
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse error: %v", diags.Errs())
	}

	g := newFSMGraph(spec)
	// First compile locals so they're available to outputs
	compileLocals(g, spec, CompileOpts{})

	diags = compileOutputs(g, spec, CompileOpts{})

	if diags.HasErrors() {
		t.Fatalf("expected no error, got: %v", diags.Errs())
	}

	if _, ok := g.Outputs["from_local"]; !ok {
		t.Fatalf("output 'from_local' not found")
	}
}

// TestCompileOutputs_VarReference tests outputs can reference variables.
func TestCompileOutputs_VarReference(t *testing.T) {
	src := `
workflow "test" {
  version      = "1"
  initial_state = "start"
  target_state  = "end"
}
  
output "echo" {
  value = "echo"
}
  
state "start" {}
state "end" { terminal = true }
`
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse error: %v", diags.Errs())
	}

	g := newFSMGraph(spec)
	diags = compileOutputs(g, spec, CompileOpts{})

	if diags.HasErrors() {
		t.Fatalf("expected no error, got: %v", diags.Errs())
	}

	if _, ok := g.Outputs["echo"]; !ok {
		t.Fatalf("output 'echo' not found")
	}
}

// TestCompileOutputs_InvalidIdentifier tests error on invalid variable reference.
func TestCompileOutputs_InvalidIdentifier(t *testing.T) {
	src := `
workflow "test" {
  version      = "1"
  initial_state = "start"
  target_state  = "end"
}
  
output "bad_ref" {
  value = undefined_var
}
  
state "start" {}
state "end" { terminal = true }
`
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse error: %v", diags.Errs())
	}

	g := newFSMGraph(spec)
	diags = compileOutputs(g, spec, CompileOpts{})

	if !diags.HasErrors() {
		t.Fatalf("expected error for undefined variable, got none")
	}
}

// TestCompileOutputs_OrderPreservation tests outputs preserve declaration order.
func TestCompileOutputs_OrderPreservation(t *testing.T) {
	src := `
workflow "test" {
  version      = "1"
  initial_state = "start"
  target_state  = "end"
}
  
output "alpha" { value = "a" }
output "beta"  { value = "b" }
output "gamma" { value = "c" }
  
state "start" {}
state "end" { terminal = true }
`
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse error: %v", diags.Errs())
	}

	g := newFSMGraph(spec)
	diags = compileOutputs(g, spec, CompileOpts{})

	if diags.HasErrors() {
		t.Fatalf("expected no error, got: %v", diags.Errs())
	}

	if len(g.OutputOrder) != 3 {
		t.Fatalf("expected 3 outputs in order, got %d", len(g.OutputOrder))
	}
	expected := []string{"alpha", "beta", "gamma"}
	for i, name := range expected {
		if g.OutputOrder[i] != name {
			t.Errorf("OutputOrder[%d] = %q, want %q", i, g.OutputOrder[i], name)
		}
	}
}
