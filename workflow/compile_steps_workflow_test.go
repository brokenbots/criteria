package workflow

import (
	"strings"
	"testing"
)

// TestWorkflowStep_AllowToolsWithoutAgent verifies that a type="workflow" step
// that specifies allow_tools without an agent produces a compile error
// containing "allow_tools requires agent".
func TestWorkflowStep_AllowToolsWithoutAgent(t *testing.T) {
	src := `
workflow "x" {
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
  step "run" {
    type        = "workflow"
    allow_tools = ["read"]
    workflow {
      step "inner" {
        adapter = "noop"
        outcome "done" { transition_to = "_continue" }
      }
    }
    outcome "done" { transition_to = "done" }
  }
  state "done" { terminal = true }
}
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, testSchemas)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for allow_tools without agent on workflow step")
	}
	if !strings.Contains(diags.Error(), "allow_tools requires agent") {
		t.Errorf("expected 'allow_tools requires agent' in diagnostic, got: %s", diags.Error())
	}
}

// TestWorkflowStep_LifecycleWithoutAgent verifies that a type="workflow" step
// that specifies lifecycle without an agent produces a compile error containing
// "lifecycle requires agent".
func TestWorkflowStep_LifecycleWithoutAgent(t *testing.T) {
	src := `
workflow "x" {
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
  step "run" {
    type      = "workflow"
    lifecycle = "open"
    workflow {
      step "inner" {
        adapter = "noop"
        outcome "done" { transition_to = "_continue" }
      }
    }
    outcome "done" { transition_to = "done" }
  }
  state "done" { terminal = true }
}
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, testSchemas)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for lifecycle without agent on workflow step")
	}
	if !strings.Contains(diags.Error(), "lifecycle requires agent") {
		t.Errorf("expected 'lifecycle requires agent' in diagnostic, got: %s", diags.Error())
	}
}

// TestWorkflowStep_InvalidLifecycle verifies that a step with an agent but an
// invalid lifecycle value produces a compile error containing "invalid lifecycle".
func TestWorkflowStep_InvalidLifecycle(t *testing.T) {
	src := `
workflow "x" {
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
  agent "bot" { adapter = "noop" }
  step "run" {
    agent     = "bot"
    lifecycle = "bad"
    outcome "done" { transition_to = "done" }
  }
  state "done" { terminal = true }
}
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, testSchemas)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for invalid lifecycle")
	}
	if !strings.Contains(diags.Error(), "invalid lifecycle") {
		t.Errorf("expected 'invalid lifecycle' in diagnostic, got: %s", diags.Error())
	}
}

// TestWorkflowStep_AllowToolsWithLifecycle verifies that a step with both
// allow_tools and lifecycle (and an agent) produces a compile error containing
// "allow_tools is only valid on execute-shape steps".
func TestWorkflowStep_AllowToolsWithLifecycle(t *testing.T) {
	src := `
workflow "x" {
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
  agent "bot" { adapter = "noop" }
  step "run" {
    agent       = "bot"
    lifecycle   = "open"
    allow_tools = ["read"]
    outcome "done" { transition_to = "done" }
  }
  state "done" { terminal = true }
}
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, testSchemas)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for allow_tools + lifecycle")
	}
	if !strings.Contains(diags.Error(), "allow_tools is only valid on execute-shape steps") {
		t.Errorf("expected 'allow_tools is only valid on execute-shape steps' in diagnostic, got: %s", diags.Error())
	}
}
