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

// TestCompileWorkflowStep_BodyHasFullSpec verifies that an inline body
// containing variable and local declarations is compiled as a full *Spec.
// After the WorkflowBodySpec → BodySpec change, the body graph should have
// a populated Variables and Locals map.
func TestCompileWorkflowStep_BodyHasFullSpec(t *testing.T) {
	src := `
workflow "x" {
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
  step "run" {
    type     = "workflow"
    for_each = ["a"]
    workflow {
      variable "label_prefix" {
        type    = "string"
        default = "test"
      }
      local "greeting" {
        value = "hello"
      }
      step "inner" {
        adapter = "noop"
        input { label = var.label_prefix }
        outcome "done" { transition_to = "_continue" }
      }
    }
    outcome "all_succeeded" { transition_to = "done" }
    outcome "any_failed"    { transition_to = "done" }
  }
  state "done" { terminal = true }
}
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("unexpected compile error: %s", diags.Error())
	}
	node := g.Steps["run"]
	if node.Body == nil {
		t.Fatal("expected Body to be non-nil")
	}
	varNode, ok := node.Body.Variables["label_prefix"]
	if !ok {
		t.Fatal("expected body Variables to contain 'label_prefix'")
	}
	if varNode.Default.AsString() != "test" {
		t.Errorf("expected label_prefix default = 'test', got %q", varNode.Default.AsString())
	}
	if _, ok := node.Body.Locals["greeting"]; !ok {
		t.Error("expected body Locals to contain 'greeting'")
	}
}

// TestCompileWorkflowStep_BodyVariableNotInOuterScope verifies that a body
// step referencing a var.* name that is not declared in the body produces a
// compile error. The outer workflow's variable scope is not accessible inside
// the body (isolation enforced at compile time by FoldExpr).
func TestCompileWorkflowStep_BodyVariableNotInOuterScope(t *testing.T) {
	src := `
workflow "x" {
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"

  variable "outer_only" {
    type    = "string"
    default = "val"
  }

  step "run" {
    type     = "workflow"
    for_each = ["a"]
    workflow {
      # No variable "outer_only" declared here — body has its own scope.
      step "inner" {
        adapter = "noop"
        input { label = var.outer_only }
        outcome "done" { transition_to = "_continue" }
      }
    }
    outcome "all_succeeded" { transition_to = "done" }
    outcome "any_failed"    { transition_to = "done" }
  }
  state "done" { terminal = true }
}
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for body step referencing undeclared outer var")
	}
	if !strings.Contains(diags.Error(), "outer_only") {
		t.Errorf("expected 'outer_only' in diagnostic, got: %s", diags.Error())
	}
}

// TestCompileWorkflowStep_InputBoundToBodyVariable verifies that a parent step
// with `input = { ... }` binding and a body that declares matching variables
// compiles successfully and sets BodyInputExpr on the compiled StepNode.
func TestCompileWorkflowStep_InputBoundToBodyVariable(t *testing.T) {
	src := `
workflow "x" {
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
  step "run" {
    type     = "workflow"
    for_each = ["a"]
    input    = { region = "us-west" }
    workflow {
      variable "region" {
        type    = "string"
        default = "us-east"
      }
      step "inner" {
        adapter = "noop"
        input { label = var.region }
        outcome "done" { transition_to = "_continue" }
      }
    }
    outcome "all_succeeded" { transition_to = "done" }
    outcome "any_failed"    { transition_to = "done" }
  }
  state "done" { terminal = true }
}
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("unexpected compile error: %s", diags.Error())
	}
	node := g.Steps["run"]
	if node.BodyInputExpr == nil {
		t.Error("expected BodyInputExpr to be non-nil when input = { ... } is present")
	}
}

// TestCompileWorkflowStep_InputMissingRequiredVariable verifies that a body
// that declares a required variable (no default) but whose parent step has no
// `input = { ... }` binding produces a compile error naming the variable.
func TestCompileWorkflowStep_InputMissingRequiredVariable(t *testing.T) {
	src := `
workflow "x" {
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
  step "run" {
    type     = "workflow"
    for_each = ["a"]
    # No input = { ... } — but body declares required variable "x".
    workflow {
      variable "x" {
        type = "string"
        # No default — this is a required input.
      }
      step "inner" {
        adapter = "noop"
        outcome "done" { transition_to = "_continue" }
      }
    }
    outcome "all_succeeded" { transition_to = "done" }
    outcome "any_failed"    { transition_to = "done" }
  }
  state "done" { terminal = true }
}
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for required body variable with no input binding")
	}
	if !strings.Contains(diags.Error(), "x") {
		t.Errorf("expected variable name 'x' in diagnostic, got: %s", diags.Error())
	}
}
