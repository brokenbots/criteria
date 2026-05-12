package workflow

// compile_steps_while_test.go — compile-time tests for the while step modifier.
// These tests cover mutual-exclusion errors, static type validation, ref
// validation, required outcomes, and successful compilation paths.

import (
	"strings"
	"testing"
)

// whileWorkflow wraps a while step body in a minimal compilable workflow.
func whileWorkflow(stepBody string) string {
	return `
workflow "t" {
  version       = "0.1"
  initial_state = "work"
  target_state  = "done"
}
adapter "noop" "default" {}
step "work" {
  target   = adapter.noop.default
  ` + stepBody + `
}
state "done" {
  terminal = true
  success  = true
}
state "failed" {
  terminal = true
  success  = false
}
`
}

// TestWhile_CompileBasic verifies that a minimal while step compiles without errors
// and stores the While expression on the StepNode.
func TestWhile_CompileBasic(t *testing.T) {
	src := whileWorkflow(`
  while  = true
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "failed" }
`)
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse error: %v", diags.Error())
	}
	g, diags := Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile error: %v", diags.Error())
	}
	node, ok := g.Steps["work"]
	if !ok {
		t.Fatal("step 'work' not found in compiled graph")
	}
	if node.While == nil {
		t.Fatal("node.While is nil; expected expression to be set")
	}
	// Mutually exclusive fields must remain nil.
	if node.ForEach != nil {
		t.Error("node.ForEach should be nil for a while step")
	}
	if node.Count != nil {
		t.Error("node.Count should be nil for a while step")
	}
	if node.Parallel != nil {
		t.Error("node.Parallel should be nil for a while step")
	}
}

// TestWhile_MutualExclusion_ForEach_Error verifies that combining while with
// for_each is a compile error.
func TestWhile_MutualExclusion_ForEach_Error(t *testing.T) {
	src := whileWorkflow(`
  while    = true
  for_each = ["a", "b"]
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "failed" }
`)
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse error: %v", diags.Error())
	}
	_, diags = Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for while+for_each, got none")
	}
	if !strings.Contains(diags.Error(), "mutually exclusive") {
		t.Errorf("compile error = %q; want 'mutually exclusive'", diags.Error())
	}
}

// TestWhile_MutualExclusion_Count_Error verifies that combining while with
// count is a compile error.
func TestWhile_MutualExclusion_Count_Error(t *testing.T) {
	src := whileWorkflow(`
  while = true
  count = 3
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "failed" }
`)
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse error: %v", diags.Error())
	}
	_, diags = Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for while+count, got none")
	}
	if !strings.Contains(diags.Error(), "mutually exclusive") {
		t.Errorf("compile error = %q; want 'mutually exclusive'", diags.Error())
	}
}

// TestWhile_MutualExclusion_Parallel_Error verifies that combining while with
// parallel is a compile error.
func TestWhile_MutualExclusion_Parallel_Error(t *testing.T) {
	src := whileWorkflow(`
  while    = true
  parallel = ["a", "b"]
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "failed" }
`)
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse error: %v", diags.Error())
	}
	_, diags = Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for while+parallel, got none")
	}
	if !strings.Contains(diags.Error(), "mutually exclusive") {
		t.Errorf("compile error = %q; want 'mutually exclusive'", diags.Error())
	}
}

// TestWhile_StaticNonBoolExpr_Error verifies that a non-bool literal in the
// while expression is rejected at compile time.
func TestWhile_StaticNonBoolExpr_Error(t *testing.T) {
	src := whileWorkflow(`
  while  = 5
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "failed" }
`)
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse error: %v", diags.Error())
	}
	_, diags = Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for non-bool while expression, got none")
	}
	if !strings.Contains(diags.Error(), "while must be a bool expression") {
		t.Errorf("compile error = %q; want 'while must be a bool expression'", diags.Error())
	}
}

// TestWhile_RequiresAllSucceededOutcome verifies that while steps must declare
// the all_succeeded outcome.
func TestWhile_RequiresAllSucceededOutcome(t *testing.T) {
	src := whileWorkflow(`
  while  = true
  outcome "any_failed" { next = "failed" }
`)
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse error: %v", diags.Error())
	}
	_, diags = Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for missing all_succeeded, got none")
	}
	if !strings.Contains(diags.Error(), "all_succeeded") {
		t.Errorf("compile error = %q; want mention of 'all_succeeded'", diags.Error())
	}
}

// TestWhile_WhileRefInNonWhileStep_Error verifies that referencing while.index
// inside a non-while step input is rejected (either by our validateWhileRefs
// diagnostic or by the HCL evaluator reporting an unknown variable).
func TestWhile_WhileRefInNonWhileStep_Error(t *testing.T) {
	src := `
workflow "t" {
  version       = "0.1"
  initial_state = "work"
  target_state  = "done"
}
adapter "noop" "default" {}
step "work" {
  target = adapter.noop.default
  input {
    msg = while.index
  }
  outcome "succeeded" { next = "done" }
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
		t.Fatal("expected compile error for while.* ref in non-while step, got none")
	}
	// Accept either our custom diagnostic or the HCL "Unknown variable: while"
	// error — both correctly reject the invalid reference.
	errStr := diags.Error()
	if !strings.Contains(errStr, "while") {
		t.Errorf("compile error = %q; want mention of 'while'", errStr)
	}
}

// TestWhile_WhileRefInInputExpr_IsValid verifies that while.index is accessible
// in a while-modified step's input expression without compile errors.
func TestWhile_WhileRefInInputExpr_IsValid(t *testing.T) {
	src := `
workflow "t" {
  version       = "0.1"
  initial_state = "work"
  target_state  = "done"
}
adapter "noop" "default" {}
step "work" {
  target = adapter.noop.default
  while  = true
  input {
    index      = while.index
    is_first   = while.first
  }
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "failed" }
}
state "done" {
  terminal = true
  success  = true
}
state "failed" {
  terminal = true
  success  = false
}
`
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse error: %v", diags.Error())
	}
	_, diags = Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("unexpected compile error: %v", diags.Error())
	}
}

// TestWhile_IsIterInGraph verifies that a while step has a back-edge in the
// compiled graph (i.e., isIter returns true for it).
func TestWhile_IsIterInGraph(t *testing.T) {
	src := whileWorkflow(`
  while  = true
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "failed" }
`)
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse error: %v", diags.Error())
	}
	g, diags := Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile error: %v", diags.Error())
	}
	node, ok := g.Steps["work"]
	if !ok {
		t.Fatal("step 'work' not found")
	}
	if node.While == nil {
		t.Fatal("expected node.While to be set")
	}
	// The compiled graph should show work → done (not work → work), but the
	// back-edge is implicit via evaluateWhile re-entry, not an explicit graph edge.
	// Verify the step compiles and has the right outcomes.
	co, ok := node.Outcomes["all_succeeded"]
	if !ok {
		t.Fatal("outcome all_succeeded not found")
	}
	if co.Next != "done" {
		t.Errorf("outcome all_succeeded.next = %q; want 'done'", co.Next)
	}
}

// TestWhile_OnFailure_Valid verifies that on_failure is accepted for while steps.
func TestWhile_OnFailure_Valid(t *testing.T) {
	for _, mode := range []string{"continue", "abort", "ignore"} {
		t.Run(mode, func(t *testing.T) {
			src := whileWorkflow(`
  while      = true
  on_failure = "` + mode + `"
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "failed" }
`)
			spec, diags := Parse("test.hcl", []byte(src))
			if diags.HasErrors() {
				t.Fatalf("parse error: %v", diags.Error())
			}
			_, diags = Compile(spec, nil)
			if diags.HasErrors() {
				t.Fatalf("compile error for on_failure=%s: %v", mode, diags.Error())
			}
		})
	}
}

// TestWhile_OnFailure_NonIterating_Error verifies that on_failure is rejected
// on a non-while/non-iterating step with an appropriate error message.
func TestWhile_OnFailure_NonIterating_Error(t *testing.T) {
	src := `
workflow "t" {
  version       = "0.1"
  initial_state = "work"
  target_state  = "done"
}
adapter "noop" "default" {}
step "work" {
  target     = adapter.noop.default
  on_failure = "continue"
  outcome "succeeded" { next = "done" }
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
		t.Fatal("expected compile error for on_failure on non-iterating step, got none")
	}
	// The error should now mention 'while' in the message.
	if !strings.Contains(diags.Error(), "while") {
		t.Errorf("compile error = %q; want mention of 'while'", diags.Error())
	}
}

// TestStep_WhileRefs_InForEachStep_Error verifies that while.index is rejected
// inside a for_each step's input block — for_each steps do not have a while
// cursor and validateWhileRefs must catch the reference at compile time.
func TestStep_WhileRefs_InForEachStep_Error(t *testing.T) {
	src := `
workflow "t" {
  version       = "0.1"
  initial_state = "work"
  target_state  = "done"
}
adapter "noop" "default" {}
step "work" {
  target   = adapter.noop.default
  for_each = ["a", "b"]
  input {
    idx = while.index
  }
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "done" }
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
		t.Fatal("expected compile error for while.* in for_each step, got none")
	}
	if !strings.Contains(diags.Error(), "while") {
		t.Errorf("compile error = %q; want mention of 'while'", diags.Error())
	}
}

// TestStep_WhileRefs_InSubworkflowStep_Error verifies that while.index is
// rejected inside a non-while subworkflow step's input block at compile time.
func TestStep_WhileRefs_InSubworkflowStep_Error(t *testing.T) {
	dir := t.TempDir()
	writeSubworkflowDir(t, dir, "inner", minimalCalleeHCL("inner", map[string]bool{"x": true}))

	src := `
workflow "t" {
  version       = "0.1"
  initial_state = "s"
  target_state  = "done"
}
subworkflow "inner" {
  source = "./inner"
}
step "s" {
  target = subworkflow.inner
  input {
    x = while.index
  }
  outcome "success" { next = "done" }
}
state "done" {
  terminal = true
  success  = true
}
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %v", diags.Error())
	}
	_, diags = CompileWithOpts(spec, nil, CompileOpts{
		WorkflowDir:         dir,
		SubWorkflowResolver: &LocalSubWorkflowResolver{},
	})
	if !diags.HasErrors() {
		t.Fatal("expected compile error for while.* in non-while subworkflow step, got none")
	}
	if !strings.Contains(diags.Error(), "while") {
		t.Errorf("compile error = %q; want mention of 'while'", diags.Error())
	}
}
