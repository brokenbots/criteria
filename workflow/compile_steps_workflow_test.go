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

// TestWorkflowStep_InvalidOnFailureValue verifies that a type="workflow" step
// with an invalid on_failure value produces a compile error containing
// "invalid on_failure". This tests that validateOnFailureValue is called by
// compileWorkflowStep.
func TestWorkflowStep_InvalidOnFailureValue(t *testing.T) {
	src := `
workflow "x" {
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
  step "run" {
    type       = "workflow"
    on_failure = "bad"
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
		t.Fatal("expected compile error for invalid on_failure on workflow step")
	}
	if !strings.Contains(diags.Error(), "invalid on_failure") {
		t.Errorf("expected 'invalid on_failure' in diagnostic, got: %s", diags.Error())
	}
}

// TestWorkflowStep_OnFailureRequiresIterating verifies that a non-iterating
// type="workflow" step with on_failure set (valid value) produces a compile
// error containing "on_failure requires for_each or count".
func TestWorkflowStep_OnFailureRequiresIterating(t *testing.T) {
	src := `
workflow "x" {
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
  step "run" {
    type       = "workflow"
    on_failure = "continue"
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
		t.Fatal("expected compile error for on_failure without for_each/count on workflow step")
	}
	if !strings.Contains(diags.Error(), "on_failure requires for_each or count") {
		t.Errorf("expected 'on_failure requires for_each or count' in diagnostic, got: %s", diags.Error())
	}
}
