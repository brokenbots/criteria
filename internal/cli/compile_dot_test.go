package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// compileDOT compiles the workflow at path and returns the DOT output string.
func compileDOT(t *testing.T, path string) string {
	t.Helper()
	out, err := compileWorkflowOutput(context.Background(), path, "dot", nil)
	if err != nil {
		t.Fatalf("compileWorkflowOutput: %v", err)
	}
	return string(out)
}

// writeDOTFixture writes hcl content to dir/main.hcl and returns dir.
func writeDOTFixture(t *testing.T, dir, hcl string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "main.hcl"), []byte(hcl), 0o644); err != nil {
		t.Fatalf("write HCL: %v", err)
	}
	return dir
}

// TestRenderDOT_PlainStepNoAnnotation verifies that a plain adapter step renders
// as [shape=box] with no label attribute.
func TestRenderDOT_PlainStepNoAnnotation(t *testing.T) {
	dir := writeDOTFixture(t, t.TempDir(), `
workflow "plain_test" {
  version       = "1"
  initial_state = "run"
  target_state  = "done"
}

adapter "noop" "default" {}

step "run" {
  target = adapter.noop.default
  input { command = "echo hi" }
  outcome "success" { next = "done" }
}

state "done" {
  terminal = true
  success  = true
}
`)
	dot := compileDOT(t, dir)

	if !strings.Contains(dot, `"run" [shape=box];`) {
		t.Errorf("expected plain step to render as [shape=box]; got:\n%s", dot)
	}
	if strings.Contains(dot, `"run" [shape=box, label=`) {
		t.Errorf("plain step must not have a label attribute; got:\n%s", dot)
	}
}

// TestRenderDOT_ForEachStepAnnotation verifies that a step with for_each renders
// with [for_each] in its node label.
func TestRenderDOT_ForEachStepAnnotation(t *testing.T) {
	dir := writeDOTFixture(t, t.TempDir(), `
workflow "foreach_test" {
  version       = "1"
  initial_state = "process"
  target_state  = "done"
}

adapter "noop" "default" {}

step "process" {
  target   = adapter.noop.default
  for_each = ["a", "b"]
  input { command = "echo" }
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "done" }
}

state "done" {
  terminal = true
  success  = true
}
`)
	dot := compileDOT(t, dir)

	if !strings.Contains(dot, "[for_each]") {
		t.Errorf("expected [for_each] annotation in DOT output; got:\n%s", dot)
	}
	if !strings.Contains(dot, `shape=box`) {
		t.Errorf("expected shape=box for for_each step; got:\n%s", dot)
	}
}

// TestRenderDOT_CountStepAnnotation verifies that a step with count renders
// with [count] in its node label.
func TestRenderDOT_CountStepAnnotation(t *testing.T) {
	dir := t.TempDir()
	content := `workflow "count_test" {
  version       = "1"
  initial_state = "run"
  target_state  = "done"
}

adapter "noop" "default" {}

step "run" {
  target = adapter.noop.default
  count  = 3
  input { command = "echo" }
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "done" }
}

state "done" {
  terminal = true
  success  = true
}
`
	if err := os.WriteFile(filepath.Join(dir, "main.hcl"), []byte(content), 0o644); err != nil {
		t.Fatalf("write HCL: %v", err)
	}
	dot := compileDOT(t, dir)

	if !strings.Contains(dot, "[count]") {
		t.Errorf("expected [count] annotation in DOT output; got:\n%s", dot)
	}
	if !strings.Contains(dot, `shape=box`) {
		t.Errorf("expected shape=box for count step; got:\n%s", dot)
	}
}

// TestRenderDOT_ParallelStepAnnotation verifies that a step with parallel renders
// with [parallel] in its node label.
func TestRenderDOT_ParallelStepAnnotation(t *testing.T) {
	dir := t.TempDir()
	content := `workflow "parallel_test" {
  version       = "1"
  initial_state = "fan_out"
  target_state  = "done"
}

adapter "noop" "default" {}

step "fan_out" {
  target   = adapter.noop.default
  parallel = ["x", "y"]
  input { command = "echo" }
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "done" }
}

state "done" {
  terminal = true
  success  = true
}
`
	if err := os.WriteFile(filepath.Join(dir, "main.hcl"), []byte(content), 0o644); err != nil {
		t.Fatalf("write HCL: %v", err)
	}
	dot := compileDOT(t, dir)

	if !strings.Contains(dot, "[parallel]") {
		t.Errorf("expected [parallel] annotation in DOT output; got:\n%s", dot)
	}
	if !strings.Contains(dot, `shape=box`) {
		t.Errorf("expected shape=box for parallel step; got:\n%s", dot)
	}
}

// TestRenderDOT_SubworkflowStepAnnotation verifies that a subworkflow-targeted
// step renders with shape=component and [→ <subwf_name>] in its label.
func TestRenderDOT_SubworkflowStepAnnotation(t *testing.T) {
	dir := t.TempDir()
	writeCallee(t, dir, "inner", nil)

	content := `workflow "subwf_test" {
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
}

state "done" {
  terminal = true
  success  = true
}
`
	if err := os.WriteFile(filepath.Join(dir, "main.hcl"), []byte(content), 0o644); err != nil {
		t.Fatalf("write HCL: %v", err)
	}
	dot := compileDOT(t, dir)

	if !strings.Contains(dot, "shape=component") {
		t.Errorf("expected shape=component for subworkflow step; got:\n%s", dot)
	}
	if !strings.Contains(dot, "[→ inner]") {
		t.Errorf("expected [→ inner] annotation in DOT output; got:\n%s", dot)
	}
}

// TestRenderDOT_IteratingSubworkflowStep verifies that a for_each step targeting
// a subworkflow renders with both [for_each] and [→ <subwf_name>] annotations.
func TestRenderDOT_IteratingSubworkflowStep(t *testing.T) {
	dir := t.TempDir()
	writeCallee(t, dir, "processor", nil)

	content := `workflow "iter_subwf_test" {
  version       = "1"
  initial_state = "run_each"
  target_state  = "done"
}

subworkflow "processor" {
  source = "./processor"
}

step "run_each" {
  target   = subworkflow.processor
  for_each = ["item_a", "item_b"]
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "done" }
}

state "done" {
  terminal = true
  success  = true
}
`
	if err := os.WriteFile(filepath.Join(dir, "main.hcl"), []byte(content), 0o644); err != nil {
		t.Fatalf("write HCL: %v", err)
	}
	dot := compileDOT(t, dir)

	if !strings.Contains(dot, "[for_each]") {
		t.Errorf("expected [for_each] annotation; got:\n%s", dot)
	}
	if !strings.Contains(dot, "[→ processor]") {
		t.Errorf("expected [→ processor] annotation; got:\n%s", dot)
	}
	if !strings.Contains(dot, "shape=component") {
		t.Errorf("expected shape=component for iterating subworkflow step; got:\n%s", dot)
	}
}
