package workflow

// compile_step_target_test.go — tests for W14 universal step target attribute.
//
// Covers: TestCompileStep_TargetAdapter, TestCompileStep_TargetSubworkflow,
// TestCompileStep_TargetUnresolvedAdapter, TestCompileStep_TargetUnresolvedSubworkflow,
// TestCompileStep_LegacyAdapterAttr_HardError, TestCompileStep_EnvironmentOverride_Resolves,
// TestCompileStep_EnvironmentOverride_Missing.

import (
	"strings"
	"testing"
)

// minimalWorkflow returns a minimal workflow HCL string with a single step using the given target line.
func minimalWorkflow(extraDecls, stepBody string) string {
	return `
workflow "t" {
  adapter "noop" "default" {}
` + extraDecls + `
  version       = "0.1"
  initial_state = "s"
  target_state  = "done"
  step "s" {
` + stepBody + `
    outcome "success" { next = "done" }
  }
  state "done" { terminal = true }
}
`
}

func TestCompileStep_TargetAdapter(t *testing.T) {
	src := minimalWorkflow("", "    target = adapter.noop.default\n")
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("unexpected compile error: %s", diags.Error())
	}
	step, ok := g.Steps["s"]
	if !ok {
		t.Fatal("step 's' not found in compiled graph")
	}
	if step.TargetKind != StepTargetAdapter {
		t.Errorf("TargetKind = %v, want StepTargetAdapter", step.TargetKind)
	}
	if step.AdapterRef != "noop.default" {
		t.Errorf("AdapterRef = %q, want %q", step.AdapterRef, "noop.default")
	}
}

func TestCompileStep_TargetSubworkflow(t *testing.T) {
	// Subworkflow requires a directory; use a temp dir with a stub workflow file.
	dir := t.TempDir()
	subHCL := minimalCalleeHCL("inner", nil)
	writeSubworkflowDir(t, dir, "inner", subHCL)

	src := `
workflow "t" {
  adapter "noop" "default" {}
  version       = "0.1"
  initial_state = "s"
  target_state  = "done"
  subworkflow "inner" {
    source = "inner"
  }
  step "s" {
    target = subworkflow.inner
    outcome "success" { next = "done" }
  }
  state "done" { terminal = true }
}
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := CompileWithOpts(spec, nil, CompileOpts{
		WorkflowDir:         dir,
		SubWorkflowResolver: &LocalSubWorkflowResolver{},
	})
	if diags.HasErrors() {
		t.Fatalf("unexpected compile error: %s", diags.Error())
	}
	step, ok := g.Steps["s"]
	if !ok {
		t.Fatal("step 's' not found in compiled graph")
	}
	if step.TargetKind != StepTargetSubworkflow {
		t.Errorf("TargetKind = %v, want StepTargetSubworkflow", step.TargetKind)
	}
	if step.SubworkflowRef != "inner" {
		t.Errorf("SubworkflowRef = %q, want %q", step.SubworkflowRef, "inner")
	}
}

func TestCompileStep_TargetUnresolvedAdapter(t *testing.T) {
	src := minimalWorkflow("", "    target = adapter.missing.default\n")
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for unresolved adapter reference; got none")
	}
	if !strings.Contains(diags.Error(), "missing.default") {
		t.Errorf("expected error to mention adapter name, got: %s", diags.Error())
	}
}

func TestCompileStep_TargetUnresolvedSubworkflow(t *testing.T) {
	src := `
workflow "t" {
  adapter "noop" "default" {}
  version       = "0.1"
  initial_state = "s"
  target_state  = "done"
  step "s" {
    target = subworkflow.nonexistent
    outcome "success" { next = "done" }
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
		t.Fatal("expected compile error for unresolved subworkflow reference; got none")
	}
	if !strings.Contains(diags.Error(), "nonexistent") {
		t.Errorf("expected error to mention subworkflow name, got: %s", diags.Error())
	}
}

func TestCompileStep_LegacyAdapterAttr_HardError(t *testing.T) {
	src := `
workflow "t" {
  adapter "noop" "default" {}
  version       = "0.1"
  initial_state = "s"
  target_state  = "done"
  step "s" {
    adapter = adapter.noop.default
    outcome "success" { next = "done" }
  }
  state "done" { terminal = true }
}
`
	_, diags := Parse("t.hcl", []byte(src))
	if !diags.HasErrors() {
		t.Fatal("expected hard parse error for legacy adapter attribute; got none")
	}
	msg := diags.Error()
	if !strings.Contains(msg, `"adapter"`) {
		t.Errorf("expected error to mention 'adapter' attribute, got: %s", msg)
	}
	if !strings.Contains(msg, "target") {
		t.Errorf("expected error to mention 'target', got: %s", msg)
	}
}

func TestCompileStep_TargetStepKindRejected(t *testing.T) {
	src := minimalWorkflow("", "    target = step.other\n")
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for target = step.<name>; got none")
	}
	if !strings.Contains(diags.Error(), "step.") {
		t.Errorf("expected error to mention step target kind, got: %s", diags.Error())
	}
}

func TestCompileStep_MissingTarget_Error(t *testing.T) {
	src := `
workflow "t" {
  adapter "noop" "default" {}
  version       = "0.1"
  initial_state = "s"
  target_state  = "done"
  step "s" {
    outcome "success" { next = "done" }
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
		t.Fatal("expected compile error for missing target; got none")
	}
	if !strings.Contains(diags.Error(), "target is required") {
		t.Errorf("expected 'target is required' in error, got: %s", diags.Error())
	}
}

func TestCompileStep_EnvironmentOverride_Resolves(t *testing.T) {
	src := `
workflow "t" {
  adapter "noop" "default" {}
  environment "shell" "ci" {}
  version       = "0.1"
  initial_state = "s"
  target_state  = "done"
  step "s" {
    target      = adapter.noop.default
    environment = shell.ci
    outcome "success" { next = "done" }
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
	step, ok := g.Steps["s"]
	if !ok {
		t.Fatal("step 's' not found in compiled graph")
	}
	if step.Environment != "shell.ci" {
		t.Errorf("Environment = %q, want %q", step.Environment, "shell.ci")
	}
}

func TestCompileStep_EnvironmentOverride_Missing(t *testing.T) {
	src := `
workflow "t" {
  adapter "noop" "default" {}
  version       = "0.1"
  initial_state = "s"
  target_state  = "done"
  step "s" {
    target      = adapter.noop.default
    environment = shell.missing
    outcome "success" { next = "done" }
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
		t.Fatal("expected compile error for missing environment override reference; got none")
	}
	if !strings.Contains(diags.Error(), "shell.missing") {
		t.Errorf("expected error to mention environment name, got: %s", diags.Error())
	}
}

// TestCompileStep_EnvironmentOverride_QuotedStringRejected verifies that a
// step using the quoted-string form (environment = "shell.ci") produces a
// compile error pointing to the bare-traversal form.
func TestCompileStep_EnvironmentOverride_QuotedStringRejected(t *testing.T) {
	src := `
workflow "t" {
  adapter "noop" "default" {}
  environment "shell" "ci" {}
  version       = "0.1"
  initial_state = "s"
  target_state  = "done"
  step "s" {
    target      = adapter.noop.default
    environment = "shell.ci"
    outcome "success" { next = "done" }
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
		t.Fatal("expected compile error for quoted environment override; got none")
	}
	if !strings.Contains(diags.Error(), "bareword") {
		t.Errorf("expected error to mention bareword syntax, got: %s", diags.Error())
	}
}

// TestCompileStep_SubworkflowStepInput verifies that a subworkflow-targeted step
// accepts an input { } block for a declared callee variable and stores the
// expressions in StepNode.InputExprs.
func TestCompileStep_SubworkflowStepInput(t *testing.T) {
	dir := t.TempDir()
	// Callee declares "greeting" with a default so the declaration-level subworkflow
	// block does not need to supply it, but step-level input can still override it.
	subHCL := minimalCalleeHCL("inner", map[string]bool{"greeting": true})
	writeSubworkflowDir(t, dir, "inner", subHCL)

	src := `
workflow "t" {
  adapter "noop" "default" {}
  version       = "0.1"
  initial_state = "s"
  target_state  = "done"
  subworkflow "inner" {
    source = "inner"
  }
  step "s" {
    target = subworkflow.inner
    input {
      greeting = "hello"
    }
    outcome "success" { next = "done" }
  }
  state "done" { terminal = true }
}
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := CompileWithOpts(spec, nil, CompileOpts{
		WorkflowDir:         dir,
		SubWorkflowResolver: &LocalSubWorkflowResolver{},
	})
	if diags.HasErrors() {
		t.Fatalf("unexpected compile error: %s", diags.Error())
	}
	step, ok := g.Steps["s"]
	if !ok {
		t.Fatal("step 's' not found in compiled graph")
	}
	if step.TargetKind != StepTargetSubworkflow {
		t.Errorf("TargetKind = %v, want StepTargetSubworkflow", step.TargetKind)
	}
	if step.InputExprs == nil {
		t.Fatal("InputExprs is nil; expected step-level input to be stored")
	}
	if _, ok := step.InputExprs["greeting"]; !ok {
		t.Errorf("InputExprs missing %q key; got keys: %v", "greeting", mapKeys(step.InputExprs))
	}
}

// TestCompileStep_SubworkflowStepInput_UndeclaredKeyRejected verifies that a
// step-level input key that is not declared in the callee's variables is rejected
// at compile time for non-iterating subworkflow-targeted steps.
func TestCompileStep_SubworkflowStepInput_UndeclaredKeyRejected(t *testing.T) {
	dir := t.TempDir()
	subHCL := minimalCalleeHCL("inner", nil) // callee declares no variables
	writeSubworkflowDir(t, dir, "inner", subHCL)

	src := `
workflow "t" {
  adapter "noop" "default" {}
  version       = "0.1"
  initial_state = "s"
  target_state  = "done"
  subworkflow "inner" {
    source = "inner"
  }
  step "s" {
    target = subworkflow.inner
    input {
      typo = "oops"
    }
    outcome "success" { next = "done" }
  }
  state "done" { terminal = true }
}
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = CompileWithOpts(spec, nil, CompileOpts{
		WorkflowDir:         dir,
		SubWorkflowResolver: &LocalSubWorkflowResolver{},
	})
	if !diags.HasErrors() {
		t.Fatal("expected compile error for undeclared step input key; got none")
	}
	if !strings.Contains(diags.Error(), "typo") {
		t.Errorf("expected error to mention undeclared key %q, got: %s", "typo", diags.Error())
	}
}

// TestCompileStep_SubworkflowIterStepInput_UndeclaredKeyRejected verifies that
// undeclared step input keys are also rejected for iterating subworkflow-targeted steps.
func TestCompileStep_SubworkflowIterStepInput_UndeclaredKeyRejected(t *testing.T) {
	dir := t.TempDir()
	subHCL := minimalCalleeHCL("inner", nil) // callee declares no variables
	writeSubworkflowDir(t, dir, "inner", subHCL)

	src := `
workflow "t" {
  adapter "noop" "default" {}
  version       = "0.1"
  initial_state = "s"
  target_state  = "done"
  subworkflow "inner" {
    source = "inner"
  }
  step "s" {
    for_each = ["a", "b"]
    target   = subworkflow.inner
    input {
      typo = each.value
    }
    outcome "success" { next = "done" }
  }
  state "done" { terminal = true }
}
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = CompileWithOpts(spec, nil, CompileOpts{
		WorkflowDir:         dir,
		SubWorkflowResolver: &LocalSubWorkflowResolver{},
	})
	if !diags.HasErrors() {
		t.Fatal("expected compile error for undeclared iterating step input key; got none")
	}
	if !strings.Contains(diags.Error(), "typo") {
		t.Errorf("expected error to mention undeclared key %q, got: %s", "typo", diags.Error())
	}
}

func mapKeys[K comparable, V any](m map[K]V) []K {
	keys := make([]K, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// TestCompileStep_SubworkflowTarget_EnvironmentRejected verifies that setting
// environment on a non-iterating subworkflow-targeted step is a compile error.
func TestCompileStep_SubworkflowTarget_EnvironmentRejected(t *testing.T) {
	dir := t.TempDir()
	writeSubworkflowDir(t, dir, "inner", minimalCalleeHCL("inner", nil))

	src := `
workflow "t" {
  adapter "noop" "default" {}
  environment "shell" "ci" {}
  version       = "0.1"
  initial_state = "s"
  target_state  = "done"
  subworkflow "inner" {
    source = "inner"
  }
  step "s" {
    target      = subworkflow.inner
    environment = shell.ci
    outcome "success" { next = "done" }
  }
  state "done" { terminal = true }
}
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = CompileWithOpts(spec, nil, CompileOpts{
		WorkflowDir:         dir,
		SubWorkflowResolver: &LocalSubWorkflowResolver{},
	})
	if !diags.HasErrors() {
		t.Fatal("expected compile error for environment on subworkflow-targeted step; got none")
	}
	if !strings.Contains(diags.Error(), "not valid for subworkflow-targeted steps") {
		t.Errorf("expected error to mention subworkflow restriction, got: %s", diags.Error())
	}
}

// TestCompileStep_SubworkflowIterTarget_EnvironmentRejected verifies that
// setting environment on an iterating subworkflow-targeted step is also rejected.
func TestCompileStep_SubworkflowIterTarget_EnvironmentRejected(t *testing.T) {
	dir := t.TempDir()
	writeSubworkflowDir(t, dir, "inner", minimalCalleeHCL("inner", nil))

	src := `
workflow "t" {
  adapter "noop" "default" {}
  environment "shell" "ci" {}
  version       = "0.1"
  initial_state = "s"
  target_state  = "done"
  subworkflow "inner" {
    source = "inner"
  }
  step "s" {
    for_each    = ["a"]
    target      = subworkflow.inner
    environment = shell.ci
    outcome "success" { next = "done" }
  }
  state "done" { terminal = true }
}
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = CompileWithOpts(spec, nil, CompileOpts{
		WorkflowDir:         dir,
		SubWorkflowResolver: &LocalSubWorkflowResolver{},
	})
	if !diags.HasErrors() {
		t.Fatal("expected compile error for environment on iterating subworkflow-targeted step; got none")
	}
	if !strings.Contains(diags.Error(), "not valid for subworkflow-targeted steps") {
		t.Errorf("expected error to mention subworkflow restriction, got: %s", diags.Error())
	}
}
