package workflow

// compile_cross_step_refs_test.go — tests for the warnCrossStepFieldRefs
// post-compilation pass (BF-03). Verifies that steps.<name>.<field> traversals
// are checked against the referenced step's OutputSchema, and that the pass is
// permissive when no schema is provided.

import (
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
)

// outputSchemaFor returns a schemas map for adapter type "noop" with the given
// output field names declared. Used to keep test bodies concise.
func outputSchemaFor(fields ...string) map[string]AdapterInfo {
	schema := make(map[string]ConfigField, len(fields))
	for _, f := range fields {
		schema[f] = ConfigField{}
	}
	return map[string]AdapterInfo{
		"noop": {OutputSchema: schema},
	}
}

// crossStepSwitchSrc returns a minimal workflow with:
//   - step "build" (adapter noop.default)
//   - switch "check" whose first condition references steps.build.<field>
//   - state targets for both arms
func crossStepSwitchSrc(field string) string {
	return `
workflow "t" {
  version       = "0.1"
  initial_state = "build"
  target_state  = "done"
}
adapter "noop" "default" {}
step "build" {
  target = adapter.noop.default
  outcome "success" { next = "check" }
}
switch "check" {
  condition {
    match = steps.build.` + field + ` == "ok"
    next  = state.done
  }
  default { next = state.done }
}
state "done" { terminal = true }
`
}

// crossStepSwitchCondOutputSrc returns a workflow with a switch condition arm
// that carries an output projection referencing steps.build.<field>.
func crossStepSwitchCondOutputSrc(field string) string {
	return `
workflow "t" {
  version       = "0.1"
  initial_state = "build"
  target_state  = "done"
}
adapter "noop" "default" {}
step "build" {
  target = adapter.noop.default
  outcome "success" { next = "check" }
}
switch "check" {
  condition {
    match  = true
    next   = state.done
    output = { x = steps.build.` + field + ` }
  }
  default { next = state.done }
}
state "done" { terminal = true }
`
}

// crossStepInputSrc returns a minimal workflow with:
//   - step "build" (adapter noop.default)
//   - step "run" whose input references steps.build.<field>
func crossStepInputSrc(field string) string {
	return `
workflow "t" {
  version       = "0.1"
  initial_state = "build"
  target_state  = "done"
}
adapter "noop" "default" {}
step "build" {
  target = adapter.noop.default
  outcome "success" { next = "run" }
}
step "run" {
  target = adapter.noop.default
  input {
    command = steps.build.` + field + `
  }
  outcome "success" { next = "done" }
}
state "done" { terminal = true }
`
}

// crossStepOutcomeSrc returns a minimal two-step workflow where step "build"'s
// success outcome has an output projection referencing steps.build.<field>.
func crossStepOutcomeSrc(field string) string {
	return `
workflow "t" {
  version       = "0.1"
  initial_state = "build"
  target_state  = "done"
}
adapter "noop" "default" {}
step "build" {
  target = adapter.noop.default
  outcome "success" {
    next   = "run"
    output = { x = steps.build.` + field + ` }
  }
}
step "run" {
  target = adapter.noop.default
  outcome "success" { next = "done" }
}
state "done" { terminal = true }
`
}

// compileWithSchemas is a small helper to parse + compile with a schemas map.
func compileWithSchemas(t *testing.T, src string, schemas map[string]AdapterInfo) (*FSMGraph, hcl.Diagnostics) {
	t.Helper()
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse error: %s", diags.Error())
	}
	return Compile(spec, schemas)
}

// countWarnings returns the number of DiagWarning diagnostics whose Summary
// contains substr.
func countWarnings(diags hcl.Diagnostics, substr string) int {
	n := 0
	for _, d := range diags {
		if d.Severity == hcl.DiagWarning && strings.Contains(d.Summary, substr) {
			n++
		}
	}
	return n
}

// TestWarnCrossStepField_SwitchKnownField verifies that a switch condition
// referencing a field that IS declared in the step's OutputSchema produces
// no diagnostic and still returns a valid graph.
func TestWarnCrossStepField_SwitchKnownField(t *testing.T) {
	g, diags := compileWithSchemas(t, crossStepSwitchSrc("stdout"), outputSchemaFor("stdout"))
	if diags.HasErrors() {
		t.Fatalf("unexpected compile error: %s", diags.Error())
	}
	if g == nil {
		t.Fatal("expected non-nil FSMGraph")
	}
	if n := countWarnings(diags, "stdout"); n != 0 {
		t.Errorf("expected 0 warnings for known field, got %d", n)
	}
}

// TestWarnCrossStepField_SwitchUnknownField verifies that a switch condition
// referencing a misspelled field (stddout) not in the OutputSchema produces
// exactly one DiagWarning containing the field name and still returns a valid graph.
func TestWarnCrossStepField_SwitchUnknownField(t *testing.T) {
	g, diags := compileWithSchemas(t, crossStepSwitchSrc("stddout"), outputSchemaFor("stdout"))
	if diags.HasErrors() {
		t.Fatalf("unexpected compile error: %s", diags.Error())
	}
	if g == nil {
		t.Fatal("warning-only compile must return a non-nil FSMGraph")
	}
	if n := countWarnings(diags, "stddout"); n != 1 {
		t.Errorf("expected exactly 1 warning mentioning %q, got %d; diags: %v", "stddout", n, diags)
	}
}

// TestWarnCrossStepField_StepInputKnownField verifies that a step input
// expression referencing a known field produces no diagnostic and returns a valid graph.
func TestWarnCrossStepField_StepInputKnownField(t *testing.T) {
	g, diags := compileWithSchemas(t, crossStepInputSrc("stdout"), outputSchemaFor("stdout"))
	if diags.HasErrors() {
		t.Fatalf("unexpected compile error: %s", diags.Error())
	}
	if g == nil {
		t.Fatal("expected non-nil FSMGraph")
	}
	if n := countWarnings(diags, "stdout"); n != 0 {
		t.Errorf("expected 0 warnings for known field, got %d", n)
	}
}

// TestWarnCrossStepField_StepInputUnknownField verifies that a step input
// expression referencing a misspelled field (stddout) produces exactly one
// DiagWarning and still returns a valid graph.
func TestWarnCrossStepField_StepInputUnknownField(t *testing.T) {
	g, diags := compileWithSchemas(t, crossStepInputSrc("stddout"), outputSchemaFor("stdout"))
	if diags.HasErrors() {
		t.Fatalf("unexpected compile error: %s", diags.Error())
	}
	if g == nil {
		t.Fatal("warning-only compile must return a non-nil FSMGraph")
	}
	if n := countWarnings(diags, "stddout"); n != 1 {
		t.Errorf("expected exactly 1 warning mentioning %q, got %d; diags: %v", "stddout", n, diags)
	}
}

// TestWarnCrossStepField_NoSchema verifies that when schemas is nil (permissive
// mode), no diagnostic is produced regardless of the field name, and the graph
// is non-nil.
func TestWarnCrossStepField_NoSchema(t *testing.T) {
	g, diags := compileWithSchemas(t, crossStepSwitchSrc("nonexistent"), nil)
	if diags.HasErrors() {
		t.Fatalf("unexpected compile error: %s", diags.Error())
	}
	if g == nil {
		t.Fatal("expected non-nil FSMGraph")
	}
	if n := countWarnings(diags, "nonexistent"); n != 0 {
		t.Errorf("expected 0 warnings for nil schemas, got %d", n)
	}
}

// TestWarnCrossStepField_OutcomeOutputCrossStep verifies that an outcome output
// projection referencing a known cross-step field produces no diagnostic and
// returns a valid graph.
func TestWarnCrossStepField_OutcomeOutputCrossStep(t *testing.T) {
	g, diags := compileWithSchemas(t, crossStepOutcomeSrc("stdout"), outputSchemaFor("stdout"))
	if diags.HasErrors() {
		t.Fatalf("unexpected compile error: %s", diags.Error())
	}
	if g == nil {
		t.Fatal("expected non-nil FSMGraph")
	}
	if n := countWarnings(diags, "stdout"); n != 0 {
		t.Errorf("expected 0 warnings for known field, got %d", n)
	}
}

// TestWarnCrossStepField_OutcomeOutputCrossStepUnknown verifies that an outcome
// output projection referencing an undeclared field ("ghost") produces exactly
// one DiagWarning and still returns a valid graph.
func TestWarnCrossStepField_OutcomeOutputCrossStepUnknown(t *testing.T) {
	g, diags := compileWithSchemas(t, crossStepOutcomeSrc("ghost"), outputSchemaFor("stdout"))
	if diags.HasErrors() {
		t.Fatalf("unexpected compile error: %s", diags.Error())
	}
	if g == nil {
		t.Fatal("warning-only compile must return a non-nil FSMGraph")
	}
	if n := countWarnings(diags, "ghost"); n != 1 {
		t.Errorf("expected exactly 1 warning mentioning %q, got %d; diags: %v", "ghost", n, diags)
	}
}

// TestWarnCrossStepField_SwitchCondOutputKnownField verifies that a switch
// condition arm output projection referencing a known field produces no
// diagnostic and returns a valid graph.
func TestWarnCrossStepField_SwitchCondOutputKnownField(t *testing.T) {
	g, diags := compileWithSchemas(t, crossStepSwitchCondOutputSrc("stdout"), outputSchemaFor("stdout"))
	if diags.HasErrors() {
		t.Fatalf("unexpected compile error: %s", diags.Error())
	}
	if g == nil {
		t.Fatal("expected non-nil FSMGraph")
	}
	if n := countWarnings(diags, "stdout"); n != 0 {
		t.Errorf("expected 0 warnings for known field, got %d", n)
	}
}

// TestWarnCrossStepField_SwitchCondOutputUnknownField verifies that a switch
// condition arm output projection referencing an undeclared field produces
// exactly one DiagWarning and still returns a valid graph.
func TestWarnCrossStepField_SwitchCondOutputUnknownField(t *testing.T) {
	g, diags := compileWithSchemas(t, crossStepSwitchCondOutputSrc("typo"), outputSchemaFor("stdout"))
	if diags.HasErrors() {
		t.Fatalf("unexpected compile error: %s", diags.Error())
	}
	if g == nil {
		t.Fatal("warning-only compile must return a non-nil FSMGraph")
	}
	if n := countWarnings(diags, "typo"); n != 1 {
		t.Errorf("expected exactly 1 warning mentioning %q, got %d; diags: %v", "typo", n, diags)
	}
}

// TestWarnCrossStepField_UnknownStepName verifies that a step input expression
// referencing an unknown step name produces exactly one DiagWarning and still
// returns a valid graph.
func TestWarnCrossStepField_UnknownStepName(t *testing.T) {
	src := `
workflow "t" {
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
}
adapter "noop" "default" {}
step "run" {
  target = adapter.noop.default
  input {
    command = steps.bulid.stdout
  }
  outcome "success" { next = "done" }
}
state "done" { terminal = true }
`
	g, diags := compileWithSchemas(t, src, outputSchemaFor("stdout"))
	if diags.HasErrors() {
		t.Fatalf("unexpected compile error: %s", diags.Error())
	}
	if g == nil {
		t.Fatal("warning-only compile must return a non-nil FSMGraph")
	}
	if n := countWarnings(diags, "bulid"); n != 1 {
		t.Errorf("expected exactly 1 warning mentioning %q, got %d; diags: %v", "bulid", n, diags)
	}
}
