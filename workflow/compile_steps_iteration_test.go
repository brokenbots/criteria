package workflow

// compile_steps_iteration_test.go — W19 tests for the parallel step modifier
// compile path: mutual exclusion, parallel_max validation, expression fold.

import (
	"runtime"
	"strings"
	"testing"
)

// parallelWorkflow wraps a parallel step body in a minimal compilable workflow.
func parallelWorkflow(stepBody string) string {
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

// TestStep_ParallelMutualExclusion_ForEach_Error verifies that combining
// parallel with for_each on the same step is a compile error.
func TestStep_ParallelMutualExclusion_ForEach_Error(t *testing.T) {
	src := parallelWorkflow(`
  parallel  = ["a", "b"]
  for_each  = ["x", "y"]
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "failed" }
`)
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse error: %v", diags.Error())
	}
	_, diags = Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for parallel+for_each, got none")
	}
	if !strings.Contains(diags.Error(), "mutually exclusive") {
		t.Errorf("compile error = %q; want 'mutually exclusive'", diags.Error())
	}
}

// TestStep_ParallelMutualExclusion_Count_Error verifies that combining
// parallel with count on the same step is a compile error.
func TestStep_ParallelMutualExclusion_Count_Error(t *testing.T) {
	src := parallelWorkflow(`
  parallel = ["a", "b"]
  count    = 3
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "failed" }
`)
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse error: %v", diags.Error())
	}
	_, diags = Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for parallel+count, got none")
	}
	if !strings.Contains(diags.Error(), "mutually exclusive") {
		t.Errorf("compile error = %q; want 'mutually exclusive'", diags.Error())
	}
}

// TestStep_ParallelMaxZero_Error verifies that parallel_max = 0 is rejected.
func TestStep_ParallelMaxZero_Error(t *testing.T) {
	src := parallelWorkflow(`
  parallel     = ["a", "b"]
  parallel_max = 0
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "failed" }
`)
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse error: %v", diags.Error())
	}
	_, diags = Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for parallel_max=0, got none")
	}
	if !strings.Contains(diags.Error(), "parallel_max must be >= 1") {
		t.Errorf("compile error = %q; want 'parallel_max must be >= 1'", diags.Error())
	}
}

// TestStep_ParallelMaxAttribute_CompilesAndCaps verifies that a valid
// parallel_max value is compiled into the StepNode.
func TestStep_ParallelMaxAttribute_CompilesAndCaps(t *testing.T) {
	src := parallelWorkflow(`
  parallel     = ["a", "b", "c"]
  parallel_max = 2
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
	if node.Parallel == nil {
		t.Fatal("node.Parallel is nil; expected expression to be set")
	}
	if node.ParallelMax != 2 {
		t.Errorf("node.ParallelMax = %d; want 2", node.ParallelMax)
	}
}

// TestStep_ParallelDefaultMax_IsGOMAXPROCS verifies that omitting parallel_max
// sets ParallelMax to exactly runtime.GOMAXPROCS(0).
func TestStep_ParallelDefaultMax_IsGOMAXPROCS(t *testing.T) {
	src := parallelWorkflow(`
  parallel = ["a"]
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
	want := runtime.GOMAXPROCS(0)
	if node.ParallelMax != want {
		t.Errorf("node.ParallelMax = %d; want %d (runtime.GOMAXPROCS(0))", node.ParallelMax, want)
	}
}

// TestStep_ParallelExpressionFolds verifies that a literal parallel expression
// compiles without errors and stores the expression on the StepNode.
func TestStep_ParallelExpressionFolds(t *testing.T) {
	src := parallelWorkflow(`
  parallel = ["task_a", "task_b", "task_c"]
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
	node := g.Steps["work"]
	if node.Parallel == nil {
		t.Fatal("node.Parallel should not be nil after compilation")
	}
	// ForEach and Count must remain nil.
	if node.ForEach != nil {
		t.Error("node.ForEach should be nil for a parallel step")
	}
	if node.Count != nil {
		t.Error("node.Count should be nil for a parallel step")
	}
}

// TestStep_ParallelRequiresAllSucceededOutcome verifies that parallel steps,
// like for_each/count steps, must declare the all_succeeded outcome.
func TestStep_ParallelRequiresAllSucceededOutcome(t *testing.T) {
	src := parallelWorkflow(`
  parallel = ["a"]
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

// TestStep_ParallelMaxVarRef_Accepted verifies that parallel_max = var.cap
// compiles successfully when the variable has a known numeric default.
func TestStep_ParallelMaxVarRef_Accepted(t *testing.T) {
	src := `
workflow "t" {
  version       = "0.1"
  initial_state = "work"
  target_state  = "done"
}
variable "cap" {
  type    = "number"
  default = 3
}
adapter "noop" "default" {}
step "work" {
  target       = adapter.noop.default
  parallel     = ["a", "b", "c"]
  parallel_max = var.cap
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
	g, diags := Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile error for var.* parallel_max: %v", diags.Error())
	}
	if g.Steps["work"].ParallelMax != 3 {
		t.Errorf("ParallelMax = %d; want 3 (from var.cap default)", g.Steps["work"].ParallelMax)
	}
}

// TestStep_ParallelMaxRuntimeExpr_Rejected verifies that parallel_max = each.index
// (a runtime-only reference) is rejected at compile time with a clear error.
func TestStep_ParallelMaxRuntimeExpr_Rejected(t *testing.T) {
	src := parallelWorkflow(`
  parallel     = ["a", "b"]
  parallel_max = each.index
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "failed" }
`)
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse error: %v", diags.Error())
	}
	_, diags = Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for runtime-only parallel_max; got none")
	}
	if !strings.Contains(diags.Error(), "runtime-only") && !strings.Contains(diags.Error(), "compile-time") {
		t.Errorf("compile error = %q; want mention of 'runtime-only' or 'compile-time'", diags.Error())
	}
}

// TestStep_ParallelMapSyntax_Rejected verifies that parallel = { ... } (map/object)
// is rejected at compile time because parallel only supports list syntax.
func TestStep_ParallelMapSyntax_Rejected(t *testing.T) {
	src := parallelWorkflow(`
  parallel = { task_a = "x", task_b = "y" }
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "failed" }
`)
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse error: %v", diags.Error())
	}
	_, diags = Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for parallel map syntax; got none")
	}
	if !strings.Contains(diags.Error(), "parallel must be a list") {
		t.Errorf("compile error = %q; want mention of 'parallel must be a list'", diags.Error())
	}
}
