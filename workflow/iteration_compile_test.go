package workflow_test

// iteration_compile_test.go — compile-layer tests for step-level for_each,
// count, on_failure, and type="workflow" steps (W10).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brokenbots/criteria/workflow"
)

// --- for_each compile tests ---

func TestIteration_ForEach_CompilesSuccessfully(t *testing.T) {
	src := `
workflow "w" {
  version       = "0.1"
  initial_state = "items"
  target_state  = "done"
  step "items" {
    adapter  = "noop"
    for_each = ["a", "b"]
    outcome "all_succeeded" { transition_to = "done" }
    outcome "any_failed"    { transition_to = "done" }
  }
  state "done" { terminal = true }
}`
	g := mustParseAndCompile(t, src)
	node, ok := g.Steps["items"]
	if !ok {
		t.Fatal("step 'items' not found in compiled graph")
	}
	if node.ForEach == nil {
		t.Error("expected ForEach expression to be set")
	}
	if node.Count != nil {
		t.Error("expected Count to be nil for for_each step")
	}
	if _, ok := node.Outcomes["all_succeeded"]; !ok {
		t.Error("expected all_succeeded outcome")
	}
	if _, ok := node.Outcomes["any_failed"]; !ok {
		t.Error("expected any_failed outcome")
	}
}

// TestIteration_Count_CompilesSuccessfully verifies that count = N on a step
// compiles correctly and sets CountExpr without ForEach.
func TestIteration_Count_CompilesSuccessfully(t *testing.T) {
	src := `
workflow "w" {
  version       = "0.1"
  initial_state = "n"
  target_state  = "done"
  step "n" {
    adapter = "noop"
    count   = 5
    outcome "all_succeeded" { transition_to = "done" }
    outcome "any_failed"    { transition_to = "done" }
  }
  state "done" { terminal = true }
}`
	g := mustParseAndCompile(t, src)
	node := g.Steps["n"]
	if node.Count == nil {
		t.Error("expected Count expression to be set")
	}
	if node.ForEach != nil {
		t.Error("expected ForEach to be nil for count step")
	}
}

// TestIteration_ForEachAndCount_MutuallyExclusive verifies that declaring
// both for_each and count on the same step is rejected.
func TestIteration_ForEachAndCount_MutuallyExclusive(t *testing.T) {
	src := `
workflow "w" {
  version       = "0.1"
  initial_state = "x"
  target_state  = "done"
  step "x" {
    adapter  = "noop"
    for_each = ["a"]
    count    = 3
    outcome "all_succeeded" { transition_to = "done" }
    outcome "any_failed"    { transition_to = "done" }
  }
  state "done" { terminal = true }
}`
	compileExpectError(t, src, "mutually exclusive")
}

// TestIteration_MissingAllSucceeded_IsError verifies that an iterating step
// that does not declare all_succeeded is rejected.
func TestIteration_MissingAllSucceeded_IsError(t *testing.T) {
	src := `
workflow "w" {
  version       = "0.1"
  initial_state = "items"
  target_state  = "done"
  step "items" {
    adapter  = "noop"
    for_each = ["a", "b"]
    outcome "any_failed" { transition_to = "done" }
  }
  state "done" { terminal = true }
}`
	compileExpectError(t, src, "all_succeeded")
}

// TestIteration_OnFailure_ValidValues verifies that valid on_failure values
// compile without error.
func TestIteration_OnFailure_ValidValues(t *testing.T) {
	for _, v := range []string{"continue", "abort", "ignore"} {
		v := v
		t.Run(v, func(t *testing.T) {
			src := `
workflow "w" {
  version       = "0.1"
  initial_state = "items"
  target_state  = "done"
  step "items" {
    adapter    = "noop"
    for_each   = ["a"]
    on_failure = "` + v + `"
    outcome "all_succeeded" { transition_to = "done" }
    outcome "any_failed"    { transition_to = "done" }
  }
  state "done" { terminal = true }
}`
			mustParseAndCompile(t, src)
		})
	}
}

// TestIteration_OnFailure_InvalidValue verifies that an invalid on_failure
// value is rejected at compile time.
func TestIteration_OnFailure_InvalidValue(t *testing.T) {
	src := `
workflow "w" {
  version       = "0.1"
  initial_state = "items"
  target_state  = "done"
  step "items" {
    adapter    = "noop"
    for_each   = ["a"]
    on_failure = "retry"
    outcome "all_succeeded" { transition_to = "done" }
    outcome "any_failed"    { transition_to = "done" }
  }
  state "done" { terminal = true }
}`
	compileExpectError(t, src, "on_failure")
}

// --- type="workflow" step compile tests ---

// TestIteration_WorkflowStep_CompilesSuccessfully verifies that a
// type="workflow" step with an inline body compiles and populates Body.
func TestIteration_WorkflowStep_CompilesSuccessfully(t *testing.T) {
	src := `
workflow "w" {
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
  step "run" {
    type     = "workflow"
    for_each = ["a", "b"]
    workflow {
      step "do" {
        adapter = "noop"
        outcome "success" { transition_to = "_continue" }
      }
    }
    outcome "all_succeeded" { transition_to = "done" }
    outcome "any_failed"    { transition_to = "done" }
  }
  state "done" { terminal = true }
}`
	g := mustParseAndCompile(t, src)
	node := g.Steps["run"]
	if node.Body == nil {
		t.Error("expected Body to be compiled for type=workflow step")
	}
	if node.BodyEntry == "" {
		t.Error("expected BodyEntry to be set")
	}
	// Body graph should have 'do' as a step and '_continue' as a terminal state.
	if _, ok := node.Body.Steps["do"]; !ok {
		t.Error("body graph should contain step 'do'")
	}
	if _, ok := node.Body.States["_continue"]; !ok {
		t.Error("body graph should contain synthetic '_continue' terminal state")
	}
}

// TestIteration_WorkflowStep_NoBody_IsError verifies that type="workflow"
// without a workflow { } block is rejected.
func TestIteration_WorkflowStep_NoBody_IsError(t *testing.T) {
	src := `
workflow "w" {
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
  step "run" {
    type     = "workflow"
    for_each = ["a"]
    outcome "all_succeeded" { transition_to = "done" }
    outcome "any_failed"    { transition_to = "done" }
  }
  state "done" { terminal = true }
}`
	compileExpectError(t, src, "workflow { ... } block")
}

// TestIteration_WorkflowStep_EmptyBody_IsError verifies that a type="workflow"
// step with an empty inline body is rejected.
func TestIteration_WorkflowStep_EmptyBody_IsError(t *testing.T) {
	src := `
workflow "w" {
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
  step "run" {
    type     = "workflow"
    for_each = ["a"]
    workflow {}
    outcome "all_succeeded" { transition_to = "done" }
    outcome "any_failed"    { transition_to = "done" }
  }
  state "done" { terminal = true }
}`
	compileExpectError(t, src, "must contain at least one step")
}

// TestIteration_WorkflowStep_InvalidType verifies that an unknown step type
// is rejected.
func TestIteration_WorkflowStep_InvalidType(t *testing.T) {
	src := `
workflow "w" {
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
  step "run" {
    type    = "agent_runner"
    adapter = "noop"
    outcome "success" { transition_to = "done" }
  }
  state "done" { terminal = true }
}`
	compileExpectError(t, src, "invalid type")
}

// TestIteration_WorkflowStep_MaxNestingDepth verifies that nesting beyond 4
// levels is rejected. Each intermediate step must be type="workflow" to chain
// the depth counter.
func TestIteration_WorkflowStep_MaxNestingDepth(t *testing.T) {
	// 5 levels of type="workflow" nesting: run→l1→l2→l3→l4(triggers error).
	// The check fires when compileWorkflowBody is called with LoadDepth >= 4.
	src := `
workflow "w" {
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
  step "run" {
    type     = "workflow"
    for_each = ["a"]
    workflow {
      step "l1" {
        type     = "workflow"
        for_each = ["b"]
        workflow {
          step "l2" {
            type     = "workflow"
            for_each = ["c"]
            workflow {
              step "l3" {
                type     = "workflow"
                for_each = ["d"]
                workflow {
                  step "l4" {
                    type     = "workflow"
                    for_each = ["e"]
                    workflow {
                      step "leaf" {
                        adapter = "noop"
                        outcome "success" { transition_to = "_continue" }
                      }
                    }
                    outcome "all_succeeded" { transition_to = "_continue" }
                    outcome "any_failed"    { transition_to = "_continue" }
                  }
                }
                outcome "all_succeeded" { transition_to = "_continue" }
                outcome "any_failed"    { transition_to = "_continue" }
              }
            }
            outcome "all_succeeded" { transition_to = "_continue" }
            outcome "any_failed"    { transition_to = "_continue" }
          }
        }
        outcome "all_succeeded" { transition_to = "_continue" }
        outcome "any_failed"    { transition_to = "_continue" }
      }
    }
    outcome "all_succeeded" { transition_to = "done" }
    outcome "any_failed"    { transition_to = "done" }
  }
  state "done" { terminal = true }
}`
	compileExpectError(t, src, "maximum workflow nesting depth")
}

// --- testdata file tests ---

// TestIteration_Testdata_SimpleCompiles verifies iteration_simple.hcl compiles.
func TestIteration_Testdata_SimpleCompiles(t *testing.T) {
	src, err := os.ReadFile("testdata/iteration_simple.hcl")
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	spec, diags := workflow.Parse("testdata/iteration_simple.hcl", src)
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := workflow.Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	if _, ok := g.Steps["process"]; !ok {
		t.Error("expected step 'process' in compiled graph")
	}
	if _, ok := g.Steps["count_phase"]; !ok {
		t.Error("expected step 'count_phase' in compiled graph")
	}
}

// TestIteration_Testdata_WorkflowStepCompiles verifies iteration_workflow_step.hcl compiles.
func TestIteration_Testdata_WorkflowStepCompiles(t *testing.T) {
	src, err := os.ReadFile("testdata/iteration_workflow_step.hcl")
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	spec, diags := workflow.Parse("testdata/iteration_workflow_step.hcl", src)
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := workflow.Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	node, ok := g.Steps["run_items"]
	if !ok {
		t.Fatal("expected step 'run_items' in compiled graph")
	}
	if node.Body == nil {
		t.Error("expected Body to be set for type=workflow step")
	}
	if _, ok := node.Body.Steps["prepare"]; !ok {
		t.Error("body should contain step 'prepare'")
	}
	if _, ok := node.Body.Steps["verify"]; !ok {
		t.Error("body should contain step 'verify'")
	}
}

// --- B-13: Error-path compile tests ---

// TestStep_OnFailureOnNonIteratingStep_Fails verifies that on_failure is
// rejected at compile time when the step has neither for_each nor count.
func TestStep_OnFailureOnNonIteratingStep_Fails(t *testing.T) {
	compileExpectError(t, `
workflow "w" {
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
  step "run" {
    adapter    = "noop"
    on_failure = "continue"
    outcome "success" { transition_to = "done" }
  }
  state "done" { terminal = true }
}`, `on_failure requires for_each or count`)
}

// TestStep_WorkflowBody_NoContinuePath_Fails verifies that a type="workflow"
// step body that has no outcome targeting "_continue" is rejected at compile time.
func TestStep_WorkflowBody_NoContinuePath_Fails(t *testing.T) {
	compileExpectError(t, `
workflow "w" {
  version       = "0.1"
  initial_state = "outer"
  target_state  = "done"
  step "outer" {
    type     = "workflow"
    for_each = ["a"]
    workflow {
      step "body" {
        adapter = "noop"
        outcome "success" { transition_to = "end" }
      }
      state "end" {
        terminal = true
      }
    }
    outcome "all_succeeded" { transition_to = "done" }
    outcome "any_failed"    { transition_to = "done" }
  }
  state "done" { terminal = true }
}`, `no path to "_continue"`)
}

// TestStep_DuplicateOutputName_Fails verifies that two output blocks with the
// same name inside a type="workflow" step are rejected at compile time.
func TestStep_DuplicateOutputName_Fails(t *testing.T) {
	compileExpectError(t, `
workflow "w" {
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
  step "run" {
    type     = "workflow"
    for_each = ["a"]
    workflow {
      step "inner" {
        adapter = "noop"
        outcome "success" { transition_to = "_continue" }
      }
      output "result" {
        value = "first"
      }
      output "result" {
        value = "second"
      }
    }
    outcome "all_succeeded" { transition_to = "done" }
    outcome "any_failed"    { transition_to = "done" }
  }
  state "done" { terminal = true }
}`, `duplicate output name`)
}

// TestStep_TypeWorkflow_FileCycle_Fails verifies that workflow_file cycle
// detection (A → B → A) produces a compile error when SubWorkflowResolver is
// provided. The test simulates a self-cycle: the resolver for "cycle_a.hcl"
// returns a Spec whose only step also specifies workflow_file = "cycle_a.hcl".
func TestStep_TypeWorkflow_FileCycle_Fails(t *testing.T) {
	// The inner spec mirrors the outer — it has a step referencing
	// "cycle_a.hcl" again, creating a direct self-cycle.
	innerSrc := `
workflow "inner" {
  version       = "0.1"
  initial_state = "recurse"
  target_state  = "done"
  step "recurse" {
    type          = "workflow"
    workflow_file = "cycle_a.hcl"
    for_each      = ["x"]
    outcome "all_succeeded" { transition_to = "done" }
    outcome "any_failed"    { transition_to = "done" }
  }
  state "done" { terminal = true }
}`
	innerSpec, diags := workflow.Parse("cycle_a.hcl", []byte(innerSrc))
	if diags.HasErrors() {
		t.Fatalf("parse inner: %s", diags.Error())
	}

	// Resolver always returns the same spec (simulates cycle_a.hcl → itself).
	resolver := func(filePath, _ string) (*workflow.Spec, error) {
		return innerSpec, nil
	}

	outerSrc := `
workflow "outer" {
  version       = "0.1"
  initial_state = "step_a"
  target_state  = "done"
  step "step_a" {
    type          = "workflow"
    workflow_file = "cycle_a.hcl"
    for_each      = ["x"]
    outcome "all_succeeded" { transition_to = "done" }
    outcome "any_failed"    { transition_to = "done" }
  }
  state "done" { terminal = true }
}`
	outerSpec, diags := workflow.Parse("outer.hcl", []byte(outerSrc))
	if diags.HasErrors() {
		t.Fatalf("parse outer: %s", diags.Error())
	}

	_, compDiags := workflow.CompileWithOpts(outerSpec, nil, workflow.CompileOpts{
		SubWorkflowResolver: resolver,
	})
	if !compDiags.HasErrors() {
		t.Fatal("expected compile error for workflow_file cycle, got none")
	}
	if got := compDiags.Error(); !containsAny(got, "cycle", "load cycle") {
		t.Errorf("compile error = %q; expected cycle-related message", got)
	}
}

// containsAny returns true when s contains any of the provided substrings.
func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(s) >= len(sub) {
			for i := 0; i <= len(s)-len(sub); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
		}
	}
	return false
}

// TestStep_TypeWorkflow_MissingWorkflowBlock_Fails verifies that a step with
// type="workflow" that omits the inline workflow { ... } block is rejected at
// compile time.
func TestStep_TypeWorkflow_MissingWorkflowBlock_Fails(t *testing.T) {
	compileExpectError(t, `
workflow "w" {
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
  step "run" {
    type     = "workflow"
    for_each = ["a"]
    outcome "all_succeeded" { transition_to = "done" }
    outcome "any_failed"    { transition_to = "done" }
  }
  state "done" { terminal = true }
}`, `requires a workflow { ... } block`)
}

// TestStep_TypeWorkflow_BothWorkflowBlockAndFile_Fails verifies that providing
// both a workflow { } block and workflow_file is rejected at compile time.
func TestStep_TypeWorkflow_BothWorkflowBlockAndFile_Fails(t *testing.T) {
	innerSrc := `
workflow "inner" {
  version       = "0.1"
  initial_state = "body"
  target_state  = "_continue"
  step "body" {
    adapter = "noop"
    outcome "success" { transition_to = "_continue" }
  }
  state "_continue" { terminal = true }
}`
	innerSpec, diags := workflow.Parse("inner.hcl", []byte(innerSrc))
	if diags.HasErrors() {
		t.Fatalf("parse inner: %s", diags.Error())
	}
	resolver := func(_, _ string) (*workflow.Spec, error) { return innerSpec, nil }

	outerSrc := `
workflow "outer" {
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
  step "run" {
    type          = "workflow"
    workflow_file = "inner.hcl"
    for_each      = ["a"]
    workflow {
      step "body" {
        adapter = "noop"
        outcome "success" { transition_to = "_continue" }
      }
      state "_continue" { terminal = true }
    }
    outcome "all_succeeded" { transition_to = "done" }
    outcome "any_failed"    { transition_to = "done" }
  }
  state "done" { terminal = true }
}`
	outerSpec, diags := workflow.Parse("outer.hcl", []byte(outerSrc))
	if diags.HasErrors() {
		t.Fatalf("parse outer: %s", diags.Error())
	}
	_, compDiags := workflow.CompileWithOpts(outerSpec, nil, workflow.CompileOpts{
		SubWorkflowResolver: resolver,
	})
	if !compDiags.HasErrors() {
		t.Fatal("expected compile error for workflow_file + workflow{} conflict; got none")
	}
	if got := compDiags.Error(); !containsAny(got, "mutually exclusive") {
		t.Errorf("compile error = %q; want 'mutually exclusive'", got)
	}
}

// TestStep_EachRefs_NotInIteratingStep_ErrorMentionsBothForEachAndCount verifies
// that the each.* compile-time error message references both for_each and count.
func TestStep_EachRefs_NotInIteratingStep_ErrorMentionsBothForEachAndCount(t *testing.T) {
	src := `
workflow "w" {
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
  step "run" {
    adapter = "noop"
    input   { val = "${each.value}" }
    outcome "success" { transition_to = "done" }
  }
  state "done" { terminal = true }
}`
	spec, diags := workflow.Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, compDiags := workflow.Compile(spec, nil)
	if !compDiags.HasErrors() {
		t.Fatal("expected compile error for each.* on non-iterating step")
	}
	msg := compDiags.Error()
	if !containsAny(msg, "for_each or count") {
		t.Errorf("error = %q; want mention of 'for_each or count'", msg)
	}
	if !containsAny(msg, "each._idx") {
		t.Errorf("error = %q; want mention of 'each._idx'", msg)
	}
}

// TestForEachExprFoldsAtCompile_FilesValidated verifies that a for_each expression
// referencing a var with a fold-time value is validated at compile time when it
// involves a file() call with a missing file path.
func TestForEachExprFoldsAtCompile_FilesValidated(t *testing.T) {
	dir := t.TempDir()
	// "missing.txt" does not exist — fold pass on for_each expression should catch it.
	hclContent := `workflow "test" {
  version       = "0.1"
  initial_state = "step1"
  target_state  = "done"

  state "done" {
    terminal = true
    success  = true
  }

  variable "path" {
    type    = "string"
    default = "missing.txt"
  }

  adapter "noop" "a" {}

  step "step1" {
    adapter    = "noop.a"
    for_each = [file(var.path)]
    outcome "all_succeeded" { transition_to = "done" }
    outcome "any_failed"    { transition_to = "done" }
  }
}
`
	wfPath := filepath.Join(dir, "test.hcl")
	if err := os.WriteFile(wfPath, []byte(hclContent), 0o644); err != nil {
		t.Fatal(err)
	}
	spec, diags := workflow.Parse(wfPath, []byte(hclContent))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, compileDiags := workflow.CompileWithOpts(spec, nil, workflow.CompileOpts{WorkflowDir: dir})
	if !compileDiags.HasErrors() {
		t.Fatal("expected compile error for for_each with file(var.path) missing file; got none")
	}
	// Check all diagnostics for the missing file reference.
	found := false
	for _, d := range compileDiags {
		if strings.Contains(d.Summary, "missing.txt") || strings.Contains(d.Detail, "missing.txt") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected a diagnostic referencing the missing file path, got: %s", compileDiags.Error())
	}
}

// TestWorkflowOutput_BodyVarReference_AcceptedAtCompile verifies that an
// output value expression referencing a body-declared variable is accepted at
// compile time. The body graph is used for FoldExpr, so var.result (declared
// in the body) is in scope.
func TestWorkflowOutput_BodyVarReference_AcceptedAtCompile(t *testing.T) {
	src := `
workflow "w" {
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
  step "run" {
    type     = "workflow"
    for_each = ["a"]
    workflow {
      variable "result" {
        type    = "string"
        default = "ok"
      }
      step "inner" {
        adapter = "noop"
        input   { result = "success" }
        outcome "success" { transition_to = "_continue" }
      }
      output "out" {
        value = var.result
      }
    }
    outcome "all_succeeded" { transition_to = "done" }
    outcome "any_failed"    { transition_to = "done" }
  }
  state "done" { terminal = true }
}`
	spec, diags := workflow.Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse error: %s", diags.Error())
	}
	_, diags = workflow.Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("expected no compile error for body-scoped var in output; got: %s", diags.Error())
	}
}

// TestWorkflowOutput_ParentOnlyVarReference_RejectedAtCompile verifies that
// an output value expression referencing a parent-scope variable (not declared
// in the body) is rejected at compile time. The body graph is used for
// FoldExpr, so var.outer (only in the parent) is NOT in scope.
func TestWorkflowOutput_ParentOnlyVarReference_RejectedAtCompile(t *testing.T) {
	compileExpectError(t, `
workflow "w" {
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
  variable "outer" {
    type    = "string"
    default = "hello"
  }
  step "run" {
    type     = "workflow"
    for_each = ["a"]
    workflow {
      step "inner" {
        adapter = "noop"
        input   { result = "success" }
        outcome "success" { transition_to = "_continue" }
      }
      output "out" {
        value = var.outer
      }
    }
    outcome "all_succeeded" { transition_to = "done" }
    outcome "any_failed"    { transition_to = "done" }
  }
  state "done" { terminal = true }
}`, `"outer"`)
}

// TestWorkflowBody_TargetStateField_RejectedAtCompile verifies that
// target_state is no longer a valid attribute inside an inline workflow body
// block. After removing TargetState from BodySpec, it lands in Remain and
// fails when decoded into SpecContent (which has no target_state attribute).
func TestWorkflowBody_TargetStateField_RejectedAtCompile(t *testing.T) {
	compileExpectError(t, `
workflow "w" {
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
  step "run" {
    type     = "workflow"
    for_each = ["a"]
    workflow {
      target_state = "custom"
      step "inner" {
        adapter = "noop"
        outcome "success" { transition_to = "_continue" }
      }
    }
    outcome "all_succeeded" { transition_to = "done" }
    outcome "any_failed"    { transition_to = "done" }
  }
  state "done" { terminal = true }
}`, `target_state`)
}
