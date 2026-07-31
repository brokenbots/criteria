package workflow

import (
	"encoding/json"
	"strings"
	"testing"
)

// maxVisitsWorkflowSrc returns a minimal workflow source that declares a
// variable and a step using max_visits = var.max_refine. The returned src
// intentionally omits any iteration modifiers so the compiled StepNode has no
// hcl.Expression fields, making JSON round-trips trivial.
func maxVisitsWorkflowSrc(maxVisitsExpr string, varDefault *string, localExpr string) string {
	varBlock := ""
	if varDefault != nil {
		varBlock = `
variable "max_refine" {
  type    = number
  default = ` + *varDefault + `
}
`
	} else {
		varBlock = `
variable "max_refine" {
  type = number
}
`
	}
	localBlock := ""
	if localExpr != "" {
		localBlock = `
local "max_refine" {
  value = ` + localExpr + `
}
`
	}
	return `
workflow {
  name          = "max_visits_test"
  version       = "0.1"
  initial_state = "execute"
  target_state  = "done"
}
` + varBlock + localBlock + `
adapter "noop" "default" {}

step "execute" {
  target     = adapter.noop.default
  max_visits = ` + maxVisitsExpr + `
  outcome "done" { next = step.done }
}

state "done" {
  terminal = true
  success  = true
}
`
}

func mustCompileMaxVisits(t *testing.T, src string) *FSMGraph {
	t.Helper()
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse error: %v", diags.Error())
	}
	g, diags := Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile error: %v", diags.Error())
	}
	return g
}

// TestMaxVisits_VarRef_Compiles verifies that max_visits = var.max_refine
// compiles when the variable has a known numeric default and the resolved
// value survives a JSON round-trip.
func TestMaxVisits_VarRef_Compiles(t *testing.T) {
	defaultVal := "4"
	src := maxVisitsWorkflowSrc("var.max_refine", &defaultVal, "")
	g := mustCompileMaxVisits(t, src)

	step := g.Steps["execute"]
	if step.MaxVisits != 4 {
		t.Errorf("MaxVisits = %d, want 4", step.MaxVisits)
	}

	raw, err := json.Marshal(step)
	if err != nil {
		t.Fatalf("marshal step: %v", err)
	}
	var rt StepNode
	if err := json.Unmarshal(raw, &rt); err != nil {
		t.Fatalf("unmarshal step: %v", err)
	}
	if rt.MaxVisits != 4 {
		t.Errorf("round-trip MaxVisits = %d, want 4", rt.MaxVisits)
	}
}

// TestMaxVisits_LocalRef_Compiles verifies that max_visits = local.max_refine
// compiles when the local resolves to a known integer.
func TestMaxVisits_LocalRef_Compiles(t *testing.T) {
	defaultVal := "5"
	src := maxVisitsWorkflowSrc("local.max_refine", &defaultVal, "5")
	g := mustCompileMaxVisits(t, src)

	if g.Steps["execute"].MaxVisits != 5 {
		t.Errorf("MaxVisits = %d, want 5", g.Steps["execute"].MaxVisits)
	}
}

// TestMaxVisits_MissingDefault_Fails verifies that a variable with no default
// used as max_visits produces a single clear compile-time diagnostic.
func TestMaxVisits_MissingDefault_Fails(t *testing.T) {
	src := maxVisitsWorkflowSrc("var.max_refine", nil, "")
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse error: %v", diags.Error())
	}
	_, diags = Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for variable with no default")
	}
	msg := diags.Error()
	if strings.Contains(msg, "Variables not allowed") {
		t.Errorf("unexpected 'Variables not allowed' diagnostic: %s", msg)
	}
	if strings.Contains(msg, "Unsuitable value type") {
		t.Errorf("unexpected 'Unsuitable value type' diagnostic: %s", msg)
	}
	if !strings.Contains(msg, "compile-time") && !strings.Contains(msg, "known default") {
		t.Errorf("expected compile-time diagnostic; got: %s", msg)
	}
	if strings.Count(msg, "Error:") > 1 {
		t.Errorf("expected a single error diagnostic; got: %s", msg)
	}
}

// TestMaxVisits_FractionalVariable_Fails verifies that a variable whose default
// is a fractional number used as max_visits is rejected with a whole-number
// diagnostic.
func TestMaxVisits_FractionalVariable_Fails(t *testing.T) {
	defaultVal := "3.5"
	src := maxVisitsWorkflowSrc("var.max_refine", &defaultVal, "")
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse error: %v", diags.Error())
	}
	_, diags = Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for fractional max_visits")
	}
	msg := diags.Error()
	if !strings.Contains(msg, "whole number") && !strings.Contains(msg, " fractional") {
		t.Errorf("expected whole-number diagnostic; got: %s", msg)
	}
}

// TestMaxVisits_NegativeVariable_Fails verifies that a variable whose default
// is negative used as max_visits is rejected with the existing >= 0 diagnostic.
func TestMaxVisits_NegativeVariable_Fails(t *testing.T) {
	defaultVal := "-1"
	src := maxVisitsWorkflowSrc("var.max_refine", &defaultVal, "")
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse error: %v", diags.Error())
	}
	_, diags = Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for negative max_visits")
	}
	msg := diags.Error()
	if !strings.Contains(msg, "max_visits must be >= 0") {
		t.Errorf("expected 'max_visits must be >= 0'; got: %s", msg)
	}
}

// TestMaxVisits_RuntimeReference_Fails verifies that runtime-only references
// such as steps.* are rejected with a single clear compile-time diagnostic.
func TestMaxVisits_RuntimeReference_Fails(t *testing.T) {
	src := `
workflow {
  name          = "max_visits_test"
  version       = "0.1"
  initial_state = "first"
  target_state  = "done"
}
adapter "noop" "default" {}
step "first" {
  target     = adapter.noop.default
  outcome "done" { next = step.second }
}
step "second" {
  target     = adapter.noop.default
  max_visits = steps.first.done
  outcome "done" { next = step.done }
}
state "done" {
  terminal = true
  success  = true
}
`
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse error: %v", diags.Error())
	}
	_, diags = Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for runtime-only max_visits reference")
	}
	msg := diags.Error()
	if strings.Contains(msg, "Variables not allowed") {
		t.Errorf("unexpected 'Variables not allowed' diagnostic: %s", msg)
	}
	if !strings.Contains(msg, "compile-time") && !strings.Contains(msg, "runtime-only") {
		t.Errorf("expected compile-time/runtime-only diagnostic; got: %s", msg)
	}
	if strings.Count(msg, "Error:") > 1 {
		t.Errorf("expected a single error diagnostic; got: %s", msg)
	}
}

// TestMaxVisits_LiteralStillWorks is a regression guard confirming that a plain
// literal integer still compiles and resolves correctly.
func TestMaxVisits_LiteralStillWorks(t *testing.T) {
	src := maxVisitsWorkflowSrc("3", nil, "")
	// Remove the required variable declaration since it is unused with a literal.
	src = strings.Replace(src, `
variable "max_refine" {
  type = number
}
`, "", 1)
	g := mustCompileMaxVisits(t, src)
	if g.Steps["execute"].MaxVisits != 3 {
		t.Errorf("MaxVisits = %d, want 3", g.Steps["execute"].MaxVisits)
	}
}
