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
// step is rendered as a subgraph cluster (not a shape=component placeholder).
func TestRenderDOT_SubworkflowStepAnnotation(t *testing.T) {
	tmpDir := t.TempDir()

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

	// Subworkflow step must produce a cluster, not a shape=component placeholder.
	if strings.Contains(dot, "shape=component") {
		t.Errorf("subworkflow step must NOT render as shape=component; got:\n%s", dot)
	}
	// Cluster ID is based on the step name ("delegate"), not the subworkflow ref ("inner").
	if !strings.Contains(dot, "subgraph cluster_delegate") {
		t.Errorf("expected subgraph cluster_delegate in DOT output, got:\n%s", dot)
	}
	if !strings.Contains(dot, `"delegate/__start__"`) {
		t.Errorf("expected namespaced delegate/__start__ node, got:\n%s", dot)
	}
}

// TestRenderDOT_IteratingSubworkflowStep verifies that a for_each step targeting
// a subworkflow renders as a cluster with the [for_each] annotation in its label.
func TestRenderDOT_IteratingSubworkflowStep(t *testing.T) {
	tmpDir := t.TempDir()

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

	// Iterating subworkflow step renders as a cluster (no shape=component placeholder).
	if strings.Contains(dot, "shape=component") {
		t.Errorf("iterating subworkflow step must NOT render as shape=component; got:\n%s", dot)
	}
	// Cluster ID is based on the step name ("process_all"), not the subworkflow ref ("processor").
	if !strings.Contains(dot, "subgraph cluster_process_all") {
		t.Errorf("expected subgraph cluster_process_all in DOT output, got:\n%s", dot)
	}
	// Iteration annotation appears in the cluster label.
	if !strings.Contains(dot, "[for_each]") {
		t.Errorf("cluster label must include [for_each] annotation, got:\n%s", dot)
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

// writeTempSubworkflow creates dir/name/main.hcl with a minimal subworkflow
// containing an optional adapter step. If stepName is non-empty, a noop step
// is added with an outcome that terminates.
func writeTempSubworkflow(t *testing.T, parent, name, stepName string) {
	t.Helper()
	dir := filepath.Join(parent, name)
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	var sb strings.Builder
	termState := "done"
	initial := termState
	if stepName != "" {
		initial = stepName
	}
	sb.WriteString("workflow " + `"` + name + `"` + " {\n")
	sb.WriteString("  version       = \"1\"\n")
	sb.WriteString("  initial_state = \"" + initial + "\"\n")
	sb.WriteString("  target_state  = \"" + termState + "\"\n")
	sb.WriteString("}\n")
	if stepName != "" {
		sb.WriteString("adapter \"noop\" \"default\" {}\n")
		sb.WriteString("step \"" + stepName + "\" {\n")
		sb.WriteString("  target = adapter.noop.default\n")
		sb.WriteString("  outcome \"success\" { next = \"" + termState + "\" }\n")
		sb.WriteString("}\n")
	}
	sb.WriteString("state \"" + termState + "\" {\n  terminal = true\n  success  = true\n}\n")
	if err := os.WriteFile(filepath.Join(dir, "main.hcl"), []byte(sb.String()), 0o644); err != nil {
		t.Fatalf("write %s/main.hcl: %v", name, err)
	}
}

// TestRenderDOT_SubworkflowCluster verifies that a workflow with a subworkflow
// step produces a subgraph cluster_ block with the subworkflow's nodes
// namespaced as "<subwf_name>/<node_name>".
func TestRenderDOT_SubworkflowCluster(t *testing.T) {
	tmpDir := t.TempDir()
	writeTempSubworkflow(t, tmpDir, "inner", "do_inner")

	parentHCL := `
workflow "parent" {
  version       = "1"
  initial_state = "delegate"
  target_state  = "done"
}
subworkflow "inner" { source = "./inner" }
step "delegate" {
  target = subworkflow.inner
  outcome "success" { next = "done" }
}
state "done" {
  terminal = true
  success  = true
}
`
	if err := os.WriteFile(filepath.Join(tmpDir, "main.hcl"), []byte(parentHCL), 0o644); err != nil {
		t.Fatalf("write parent: %v", err)
	}

	out, err := compileWorkflowOutput(context.Background(), tmpDir, "dot", nil)
	if err != nil {
		t.Fatalf("compile dot: %v", err)
	}
	dot := string(out)

	// Cluster ID is derived from the step name ("delegate"), not the subworkflow ref ("inner").
	if !strings.Contains(dot, "subgraph cluster_delegate") {
		t.Errorf("expected subgraph cluster_delegate, got:\n%s", dot)
	}
	// Nodes inside the cluster must be namespaced by step name.
	if !strings.Contains(dot, `"delegate/do_inner"`) {
		t.Errorf("expected namespaced node delegate/do_inner, got:\n%s", dot)
	}
	if !strings.Contains(dot, `"delegate/done"`) {
		t.Errorf("expected namespaced node delegate/done, got:\n%s", dot)
	}
	if !strings.Contains(dot, `"delegate/__start__"`) {
		t.Errorf("expected namespaced delegate/__start__, got:\n%s", dot)
	}
}

// TestRenderDOT_SubworkflowClusterEdges verifies that parent edges are rewired
// to/from the cluster boundary: the initial edge points into the cluster's
// __start__ node, terminal-state exit edges point to the parent targets, and
// no dangling shape=component placeholder node remains.
func TestRenderDOT_SubworkflowClusterEdges(t *testing.T) {
	tmpDir := t.TempDir()
	writeTempSubworkflow(t, tmpDir, "inner", "do_inner")

	parentHCL := `
workflow "parent" {
  version       = "1"
  initial_state = "delegate"
  target_state  = "done"
}
subworkflow "inner" { source = "./inner" }
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
		t.Fatalf("write parent: %v", err)
	}

	out, err := compileWorkflowOutput(context.Background(), tmpDir, "dot", nil)
	if err != nil {
		t.Fatalf("compile dot: %v", err)
	}
	dot := string(out)

	// No shape=component placeholder.
	if strings.Contains(dot, "shape=component") {
		t.Errorf("shape=component placeholder must not appear; got:\n%s", dot)
	}
	// Initial edge must point into cluster entry (keyed by step name "delegate").
	if !strings.Contains(dot, `"__start__" -> "delegate/__start__"`) {
		t.Errorf("__start__ must route to delegate/__start__, got:\n%s", dot)
	}
	// Exit edges from terminal state to parent outcomes.
	if !strings.Contains(dot, `"delegate/done" -> "done" [label="failure"]`) {
		t.Errorf("expected exit edge delegate/done -> done (failure), got:\n%s", dot)
	}
	if !strings.Contains(dot, `"delegate/done" -> "done" [label="success"]`) {
		t.Errorf("expected exit edge delegate/done -> done (success), got:\n%s", dot)
	}
}

// TestRenderDOT_NestedSubworkflowCluster verifies that a subworkflow which
// itself contains a subworkflow step produces nested subgraph cluster_ blocks
// with correctly doubly-namespaced node IDs.
func TestRenderDOT_NestedSubworkflowCluster(t *testing.T) {
	tmpDir := t.TempDir()

	// Innermost subworkflow (leaf): "leaf" with one step "leaf_work".
	outerDir := filepath.Join(tmpDir, "outer")
	if err := os.Mkdir(outerDir, 0o755); err != nil {
		t.Fatalf("mkdir outer: %v", err)
	}
	writeTempSubworkflow(t, outerDir, "leaf", "leaf_work")

	// Middle subworkflow "outer": has a step "run_leaf" targeting "leaf".
	outerHCL := `
workflow "outer" {
  version       = "1"
  initial_state = "run_leaf"
  target_state  = "done"
}
subworkflow "leaf" { source = "./leaf" }
step "run_leaf" {
  target = subworkflow.leaf
  outcome "success" { next = "done" }
}
state "done" {
  terminal = true
  success  = true
}
`
	if err := os.WriteFile(filepath.Join(outerDir, "main.hcl"), []byte(outerHCL), 0o644); err != nil {
		t.Fatalf("write outer/main.hcl: %v", err)
	}

	// Root workflow: step "run_outer" targets "outer".
	parentHCL := `
workflow "root" {
  version       = "1"
  initial_state = "run_outer"
  target_state  = "done"
}
subworkflow "outer" { source = "./outer" }
step "run_outer" {
  target = subworkflow.outer
  outcome "success" { next = "done" }
}
state "done" {
  terminal = true
  success  = true
}
`
	if err := os.WriteFile(filepath.Join(tmpDir, "main.hcl"), []byte(parentHCL), 0o644); err != nil {
		t.Fatalf("write main.hcl: %v", err)
	}

	out, err := compileWorkflowOutput(context.Background(), tmpDir, "dot", nil)
	if err != nil {
		t.Fatalf("compile dot: %v", err)
	}
	dot := string(out)

	// Outer cluster (keyed by root step name "run_outer").
	if !strings.Contains(dot, "subgraph cluster_run_outer") {
		t.Errorf("expected subgraph cluster_run_outer, got:\n%s", dot)
	}
	// Nested cluster for leaf: sanitizeDotID("run_outer/" + "run_leaf") = "run_outer_run_leaf".
	if !strings.Contains(dot, "subgraph cluster_run_outer_run_leaf") {
		t.Errorf("expected nested subgraph cluster_run_outer_run_leaf, got:\n%s", dot)
	}
	// Doubly-namespaced leaf nodes.
	if !strings.Contains(dot, `"run_outer/run_leaf/leaf_work"`) {
		t.Errorf("expected doubly-namespaced run_outer/run_leaf/leaf_work, got:\n%s", dot)
	}
	if !strings.Contains(dot, `"run_outer/run_leaf/__start__"`) {
		t.Errorf("expected doubly-namespaced run_outer/run_leaf/__start__, got:\n%s", dot)
	}
	// Root cluster entry edge.
	if !strings.Contains(dot, `"__start__" -> "run_outer/__start__"`) {
		t.Errorf("root __start__ must route to run_outer/__start__, got:\n%s", dot)
	}
}

// TestRenderDOT_RepeatedSubworkflowSameDeclaration verifies that two parent
// steps targeting the same subworkflow declaration produce two distinct cluster
// blocks with distinct namespaced node IDs (keyed by step name, not subworkflow
// ref), so a shared subworkflow is inlined separately for each call site.
func TestRenderDOT_RepeatedSubworkflowSameDeclaration(t *testing.T) {
	tmpDir := t.TempDir()
	writeTempSubworkflow(t, tmpDir, "shared", "shared_work")

	parentHCL := `
workflow "parent" {
  version       = "1"
  initial_state = "first_call"
  target_state  = "done"
}
subworkflow "shared" { source = "./shared" }
step "first_call" {
  target = subworkflow.shared
  outcome "success" { next = "second_call" }
}
step "second_call" {
  target = subworkflow.shared
  outcome "success" { next = "done" }
}
state "done" {
  terminal = true
  success  = true
}
`
	if err := os.WriteFile(filepath.Join(tmpDir, "main.hcl"), []byte(parentHCL), 0o644); err != nil {
		t.Fatalf("write main.hcl: %v", err)
	}

	out, err := compileWorkflowOutput(context.Background(), tmpDir, "dot", nil)
	if err != nil {
		t.Fatalf("compile dot: %v", err)
	}
	dot := string(out)

	// Each call site gets its own cluster (keyed by step name).
	if !strings.Contains(dot, "subgraph cluster_first_call") {
		t.Errorf("expected subgraph cluster_first_call, got:\n%s", dot)
	}
	if !strings.Contains(dot, "subgraph cluster_second_call") {
		t.Errorf("expected subgraph cluster_second_call, got:\n%s", dot)
	}
	// Node IDs must be distinct: namespaced by step name.
	if !strings.Contains(dot, `"first_call/__start__"`) {
		t.Errorf("expected first_call/__start__, got:\n%s", dot)
	}
	if !strings.Contains(dot, `"second_call/__start__"`) {
		t.Errorf("expected second_call/__start__, got:\n%s", dot)
	}
	if !strings.Contains(dot, `"first_call/shared_work"`) {
		t.Errorf("expected first_call/shared_work, got:\n%s", dot)
	}
	if !strings.Contains(dot, `"second_call/shared_work"`) {
		t.Errorf("expected second_call/shared_work, got:\n%s", dot)
	}
	// No duplicate cluster IDs: both must appear as separate subgraph declarations.
	// Count occurrences of "subgraph cluster_" — must be exactly 2.
	clusterCount := strings.Count(dot, "subgraph cluster_")
	if clusterCount != 2 {
		t.Errorf("expected exactly 2 subgraph cluster_ blocks, got %d:\n%s", clusterCount, dot)
	}
	// Edge chain: __start__ → first_call entry, first_call exit → second_call entry.
	if !strings.Contains(dot, `"__start__" -> "first_call/__start__"`) {
		t.Errorf("root __start__ must route to first_call/__start__, got:\n%s", dot)
	}
	if !strings.Contains(dot, `"first_call/done" -> "second_call/__start__"`) {
		t.Errorf("first_call exit must route to second_call/__start__, got:\n%s", dot)
	}
	if !strings.Contains(dot, `"second_call/done" -> "done"`) {
		t.Errorf("second_call exit must route to done, got:\n%s", dot)
	}
}
