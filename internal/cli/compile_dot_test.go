package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brokenbots/criteria/workflow"
)

// compileDOTFromHCL writes hclContent to a temp dir and compiles it to DOT.
func compileDOTFromHCL(t *testing.T, hclContent string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.hcl"), []byte(hclContent), 0o644); err != nil {
		t.Fatalf("write hcl: %v", err)
	}
	out, err := compileWorkflowOutput(context.Background(), dir, "dot", nil)
	if err != nil {
		t.Fatalf("compile dot: %v", err)
	}
	return string(out)
}

// TestRenderDOT_PlainStepNoAnnotation verifies that a plain adapter step renders
// with shape=box and no label attribute.
func TestRenderDOT_PlainStepNoAnnotation(t *testing.T) {
	const hcl = `
workflow "test_plain" {
  version       = "1"
  initial_state = "do_work"
  target_state  = "done"
}
adapter "noop" "default" {}
step "do_work" {
  target = adapter.noop.default
  outcome "success" { next = "done" }
}
state "done" {
  terminal = true
  success  = true
}
`
	dot := compileDOTFromHCL(t, hcl)
	if !strings.Contains(dot, `"do_work" [shape=box];`) {
		t.Errorf("expected plain step to have shape=box only, got:\n%s", dot)
	}
	if strings.Contains(dot, `"do_work" [shape=box, label=`) {
		t.Errorf("plain step must not have a label attribute, got:\n%s", dot)
	}
}

// TestRenderDOT_ForEachStepAnnotation verifies that a for_each step gets the
// [for_each] annotation in its label.
func TestRenderDOT_ForEachStepAnnotation(t *testing.T) {
	const hcl = `
workflow "test_for_each" {
  version       = "1"
  initial_state = "fan_out"
  target_state  = "done"
}
adapter "noop" "default" {}
step "fan_out" {
  target   = adapter.noop.default
  for_each = ["a", "b", "c"]
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "done" }
}
state "done" {
  terminal = true
  success  = true
}
`
	dot := compileDOTFromHCL(t, hcl)
	if !strings.Contains(dot, "[for_each]") {
		t.Errorf("expected [for_each] annotation in DOT output, got:\n%s", dot)
	}
	if !strings.Contains(dot, "shape=box") {
		t.Errorf("for_each step must still use shape=box, got:\n%s", dot)
	}
}

// TestRenderDOT_CountStepAnnotation verifies that a count step gets the [count]
// annotation in its label.
func TestRenderDOT_CountStepAnnotation(t *testing.T) {
	const hcl = `
workflow "test_count" {
  version       = "1"
  initial_state = "repeat"
  target_state  = "done"
}
adapter "noop" "default" {}
step "repeat" {
  target = adapter.noop.default
  count  = 5
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "done" }
}
state "done" {
  terminal = true
  success  = true
}
`
	dot := compileDOTFromHCL(t, hcl)
	if !strings.Contains(dot, "[count]") {
		t.Errorf("expected [count] annotation in DOT output, got:\n%s", dot)
	}
	if !strings.Contains(dot, "shape=box") {
		t.Errorf("count step must still use shape=box, got:\n%s", dot)
	}
}

// TestRenderDOT_ParallelStepAnnotation verifies that a parallel step gets the
// [parallel] annotation in its label.
func TestRenderDOT_ParallelStepAnnotation(t *testing.T) {
	const hcl = `
workflow "test_parallel" {
  version       = "1"
  initial_state = "concurrent"
  target_state  = "done"
}
adapter "noop" "default" {}
step "concurrent" {
  target   = adapter.noop.default
  parallel = ["x", "y", "z"]
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "done" }
}
state "done" {
  terminal = true
  success  = true
}
`
	dot := compileDOTFromHCL(t, hcl)
	if !strings.Contains(dot, "[parallel]") {
		t.Errorf("expected [parallel] annotation in DOT output, got:\n%s", dot)
	}
	if !strings.Contains(dot, "shape=box") {
		t.Errorf("parallel step must still use shape=box, got:\n%s", dot)
	}
}

// TestRenderDOT_SubworkflowStepAnnotation verifies that a subworkflow-targeted
// step uses shape=component and includes [→ <name>] in its label.
func TestRenderDOT_SubworkflowStepAnnotation(t *testing.T) {
	tmpDir := t.TempDir()

	// Write the callee subworkflow.
	calleeHCL := `
workflow "inner" {
  version       = "1"
  initial_state = "done"
  target_state  = "done"
}
state "done" {
  terminal = true
  success  = true
}
`
	calleeDir := filepath.Join(tmpDir, "inner")
	if err := os.Mkdir(calleeDir, 0o755); err != nil {
		t.Fatalf("mkdir callee: %v", err)
	}
	if err := os.WriteFile(filepath.Join(calleeDir, "main.hcl"), []byte(calleeHCL), 0o644); err != nil {
		t.Fatalf("write callee hcl: %v", err)
	}

	// Write the parent workflow.
	parentHCL := `
workflow "parent" {
  version       = "1"
  initial_state = "delegate"
  target_state  = "done"
}
subworkflow "inner" {
  source = "./inner"
}
step "delegate" {
  target = subworkflow.inner
  outcome "success" { next = "done" }
  outcome "failure" { next = "done" }
}
state "done" {
  terminal = true
  success  = true
}
`
	if err := os.WriteFile(filepath.Join(tmpDir, "main.hcl"), []byte(parentHCL), 0o644); err != nil {
		t.Fatalf("write parent hcl: %v", err)
	}

	out, err := compileWorkflowOutput(context.Background(), tmpDir, "dot", nil)
	if err != nil {
		t.Fatalf("compile dot: %v", err)
	}
	dot := string(out)

	if !strings.Contains(dot, "shape=component") {
		t.Errorf("subworkflow step must use shape=component, got:\n%s", dot)
	}
	if !strings.Contains(dot, "[→ inner]") {
		t.Errorf("subworkflow step must include [→ inner] annotation, got:\n%s", dot)
	}
}

// TestRenderDOT_IteratingSubworkflowStep verifies that a for_each step targeting
// a subworkflow shows both [for_each] and [→ <name>] annotations.
func TestRenderDOT_IteratingSubworkflowStep(t *testing.T) {
	tmpDir := t.TempDir()

	// Write the callee subworkflow.
	calleeHCL := `
workflow "processor" {
  version       = "1"
  initial_state = "done"
  target_state  = "done"
}
variable "item" { type = "string" }
state "done" {
  terminal = true
  success  = true
}
`
	calleeDir := filepath.Join(tmpDir, "processor")
	if err := os.Mkdir(calleeDir, 0o755); err != nil {
		t.Fatalf("mkdir callee: %v", err)
	}
	if err := os.WriteFile(filepath.Join(calleeDir, "main.hcl"), []byte(calleeHCL), 0o644); err != nil {
		t.Fatalf("write callee hcl: %v", err)
	}

	// Write the parent workflow with a for_each targeting the subworkflow.
	parentHCL := `
workflow "parent_iter" {
  version       = "1"
  initial_state = "process_all"
  target_state  = "done"
}
subworkflow "processor" {
  source = "./processor"
  input = {
    item = each.value
  }
}
step "process_all" {
  target   = subworkflow.processor
  for_each = ["alpha", "beta"]
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "done" }
}
state "done" {
  terminal = true
  success  = true
}
`
	if err := os.WriteFile(filepath.Join(tmpDir, "main.hcl"), []byte(parentHCL), 0o644); err != nil {
		t.Fatalf("write parent hcl: %v", err)
	}

	out, err := compileWorkflowOutput(context.Background(), tmpDir, "dot", nil)
	if err != nil {
		t.Fatalf("compile dot: %v", err)
	}
	dot := string(out)

	if !strings.Contains(dot, "[for_each]") {
		t.Errorf("iterating subworkflow step must include [for_each], got:\n%s", dot)
	}
	if !strings.Contains(dot, "[→ processor]") {
		t.Errorf("iterating subworkflow step must include [→ processor], got:\n%s", dot)
	}
	if !strings.Contains(dot, "shape=component") {
		t.Errorf("iterating subworkflow step must use shape=component, got:\n%s", dot)
	}
}

// TestDotStepAttrs_PlainAdapter verifies the attribute string for a plain step.
func TestDotStepAttrs_PlainAdapter(t *testing.T) {
	st := &workflow.StepNode{Name: "step1", AdapterRef: "noop.default"}
	got := dotStepAttrs("step1", st)
	if got != "shape=box" {
		t.Errorf("got %q, want %q", got, "shape=box")
	}
}

// TestDotStepAttrs_SubworkflowOnly verifies shape=component and label for a
// plain subworkflow step (no iteration).
func TestDotStepAttrs_SubworkflowOnly(t *testing.T) {
	st := &workflow.StepNode{Name: "delegate", SubworkflowRef: "review"}
	got := dotStepAttrs("delegate", st)
	if !strings.Contains(got, "shape=component") {
		t.Errorf("expected shape=component in %q", got)
	}
	if !strings.Contains(got, "[→ review]") {
		t.Errorf("expected [→ review] in %q", got)
	}
}
