package workflow

import (
	"strings"
	"testing"
)

// TestWorkflowStep_AllowToolsWithoutAgent verifies that a type="workflow" step
// that specifies allow_tools without an adapter produces a compile error
// containing "allow_tools requires an adapter reference".
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
      adapter "noop" "default" {}
      step "inner" {
        adapter = adapter.noop.default
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
		t.Fatal("expected compile error for allow_tools without adapter on workflow step")
	}
	if !strings.Contains(diags.Error(), "allow_tools requires an adapter reference") {
		t.Errorf("expected 'allow_tools requires an adapter reference' in diagnostic, got: %s", diags.Error())
	}
}

// TestWorkflowStep_LifecycleWithoutAgent tests that lifecycle attributes on steps
// produce a parse-time error.
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
      adapter "noop" "default" {}
      step "inner" {
        adapter = adapter.noop.default
        outcome "done" { transition_to = "_continue" }
      }
    }
    outcome "done" { transition_to = "done" }
  }
  state "done" { terminal = true }
}
`
	_, diags := Parse("t.hcl", []byte(src))
	if !diags.HasErrors() {
		t.Fatal("expected parse error for lifecycle attribute")
	}
	if !strings.Contains(diags.Error(), `removed attribute "lifecycle"`) {
		t.Errorf("expected 'removed attribute \"lifecycle\"' in diagnostic, got: %s", diags.Error())
	}
}

// TestWorkflowStep_InvalidOnFailureValue verifies that a type="workflow" step
// with an invalid on_failure value produces a compile error containing
// "invalid on_failure". This tests that validateOnFailureValue is called by
// compileWorkflowStep.
func TestWorkflowStep_InvalidOnFailureValue(t *testing.T) {
	src := `
workflow "x" {
  adapter "noop" "default" {}
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
  step "run" {
    type       = "workflow"
    on_failure = "bad"
    workflow {
      step "inner" {
        adapter = adapter.noop.default
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
  adapter "noop" "default" {}
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
  step "run" {
    type       = "workflow"
    on_failure = "continue"
    workflow {
      step "inner" {
        adapter = adapter.noop.default
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
      adapter "noop" "default" {}
      variable "label_prefix" {
        type    = "string"
        default = "test"
      }
      local "greeting" {
        value = "hello"
      }
      step "inner" {
        adapter = adapter.noop.default
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
      adapter "noop" "default" {}
      # No variable "outer_only" declared here — body has its own scope.
      step "inner" {
        adapter = adapter.noop.default
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
      adapter "noop" "default" {}
      variable "region" {
        type    = "string"
        default = "us-east"
      }
      step "inner" {
        adapter = adapter.noop.default
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
      adapter "noop" "default" {}
      variable "x" {
        type = "string"
        # No default — this is a required input.
      }
      step "inner" {
        adapter = adapter.noop.default
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

// TestCompileWorkflowStep_InputInvalidNamespace verifies that a body step
// `input = { ... }` expression that references an unsupported variable namespace
// (not var.*, local.*, each.*, steps.*, or shared_variable.*) is rejected at
// compile time with a diagnostic. This ensures that typos or fully unknown
// roots (e.g. `nonexistent.val`) are caught before any runtime evaluation.
func TestCompileWorkflowStep_InputInvalidNamespace(t *testing.T) {
	src := `
workflow "x" {
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
  step "run" {
    type     = "workflow"
    for_each = ["a"]
    input    = { region = nonexistent.val }
    workflow {
      variable "region" {
        type    = "string"
        default = "us-east"
      }
      step "inner" {
        adapter = adapter.noop.default
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
	_, diags = Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for unsupported namespace in body input expression")
	}
}

// TestCompileWorkflowStep_InputNonObjectShape verifies that a body step
// `input = <scalar>` expression that does not evaluate to a cty.Object is
// rejected at compile time with an actionable diagnostic. This prevents a
// silent runtime failure when the engine tries to apply the value as a var map.
func TestCompileWorkflowStep_InputNonObjectShape(t *testing.T) {
	src := `
workflow "x" {
  adapter "noop" "default" {}
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
  step "run" {
    type     = "workflow"
    for_each = ["a"]
    input    = "not-an-object"
    workflow {
      adapter "noop" "default" {}
      step "inner" {
        adapter = adapter.noop.default
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
		t.Fatal("expected compile error for non-object body input expression")
	}
	if !strings.Contains(diags.Error(), "object") {
		t.Errorf("expected 'object' in diagnostic, got: %s", diags.Error())
	}
}

// TestResolveBodyEntry_ExplicitEntry verifies that an explicit entry field
// takes precedence over initial_state and first-step fallback.
func TestResolveBodyEntry_ExplicitEntry(t *testing.T) {
	wb := &BodySpec{Entry: "myentry", InitialState: "other"}
	steps := []StepSpec{{Name: "first"}}
	if got := resolveBodyEntry(wb, steps); got != "myentry" {
		t.Errorf("expected myentry, got %q", got)
	}
}

// TestResolveBodyEntry_InitialState verifies that initial_state is used when
// no explicit entry is set.
func TestResolveBodyEntry_InitialState(t *testing.T) {
	wb := &BodySpec{InitialState: "starthere"}
	steps := []StepSpec{{Name: "first"}}
	if got := resolveBodyEntry(wb, steps); got != "starthere" {
		t.Errorf("expected starthere, got %q", got)
	}
}

// TestValidateWorkflowStepOutcomes_NoOutcomesError verifies that a
// non-iterating step with zero outcomes produces a compile error.
func TestValidateWorkflowStepOutcomes_NoOutcomesError(t *testing.T) {
	sp := &StepSpec{Name: "needs-outcome"}
	node := &StepNode{Outcomes: map[string]string{}}
	diags := validateWorkflowStepOutcomes(sp, node, false)
	if !diags.HasErrors() {
		t.Fatal("expected error for step with no outcomes")
	}
	if !strings.Contains(diags.Error(), "at least one outcome is required") {
		t.Errorf("unexpected error message: %s", diags.Error())
	}
}

// TestBuildBodySpec_WiresNameAndVersion verifies that user-supplied name and
// version in the body block are propagated to the synthetic Spec.
func TestBuildBodySpec_WiresNameAndVersion(t *testing.T) {
	wb := &BodySpec{Name: "custom-name", Version: "2.0"}
	content := &SpecContent{Steps: []StepSpec{{Name: "s"}}}
	result := buildBodySpec("step1", wb, &Spec{}, content, "s")
	if result.Name != "custom-name" {
		t.Errorf("expected Name=%q, got %q", "custom-name", result.Name)
	}
	if result.Version != "2.0" {
		t.Errorf("expected Version=%q, got %q", "2.0", result.Version)
	}
}

// TestBuildBodySpec_DefaultsNameAndVersion verifies that the synthetic
// defaults ("<step>:body" and "1") are used when wb.Name/wb.Version are empty.
func TestBuildBodySpec_DefaultsNameAndVersion(t *testing.T) {
	wb := &BodySpec{}
	content := &SpecContent{Steps: []StepSpec{{Name: "s"}}}
	result := buildBodySpec("mystep", wb, &Spec{}, content, "s")
	if result.Name != "mystep:body" {
		t.Errorf("expected Name=%q, got %q", "mystep:body", result.Name)
	}
	if result.Version != "1" {
		t.Errorf("expected Version=%q, got %q", "1", result.Version)
	}
}
