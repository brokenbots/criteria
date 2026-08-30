package engine

// cri39_repro_test.go — regression tests for nested parallel_max ceiling
// propagation across subworkflow boundaries (CRI-39).
//
// Each test uses a concurrencyTrackingAdapter that records the peak number of
// concurrent Execute calls. The adapter is reused across the whole tree so the
// peak reflects the aggregate leaf concurrency, exactly what the reported bug
// over-counts.

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow"
)

// cri39HCLList returns an HCL list literal with n string items.
func cri39HCLList(n int) string {
	var b strings.Builder
	b.WriteString("[")
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(`"`)
		b.WriteString(strconv.Itoa(i))
		b.WriteString(`"`)
	}
	b.WriteString("]")
	return b.String()
}

// cri39Compile creates a temporary workflow directory, writes the supplied files
// (relative paths), and compiles the root workflow.
func cri39Compile(t *testing.T, files map[string]string) *workflow.FSMGraph {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		writeFile(t, filepath.Join(root, rel), content)
	}
	return compileWorkflowDir(t, root)
}

// cri39NewAdapter returns a concurrency-tracking fake adapter with the given
// per-execution sleep duration.
func cri39NewAdapter(d time.Duration) *concurrencyTrackingAdapter {
	mu := &sync.Mutex{}
	active := 0
	peak := 0
	return &concurrencyTrackingAdapter{
		name:          "fake",
		outcome:       "success",
		mu:            mu,
		active:        &active,
		peakActive:    &peak,
		executionTime: d,
	}
}

// cri39Peak returns the observed peak concurrency for the adapter.
func cri39Peak(p *concurrencyTrackingAdapter) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return *p.peakActive
}

// cri39Run executes the compiled workflow with the supplied adapter and
// returns the wall-clock elapsed time.
func cri39Run(t *testing.T, g *workflow.FSMGraph, p *concurrencyTrackingAdapter) time.Duration {
	t.Helper()
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"fake":         p,
		"fake.default": p,
	}}
	sink := &fakeSink{}
	start := time.Now()
	eng := NewTestEngine(g, loader, sink, WithWorkflowDir(g.WorkflowDir))
	if err := eng.Run(context.Background()); err != nil {
		t.Fatalf("engine run failed: %v", err)
	}
	return time.Since(start)
}

// cri39ParentHCL returns the root workflow HCL that invokes the "child"
// subworkflow in parallel with the supplied list and parallel_max value.
func cri39ParentHCL(parallelList string, parallelMax int) string {
	return `workflow {
  name          = "root"
  version       = "0.1"
  initial_state = "work"
  target_state  = "done"
}

subworkflow "child" {
  source = "./child"
}

step "work" {
  target       = subworkflow.child
  parallel     = ` + parallelList + `
  parallel_max = ` + strconv.Itoa(parallelMax) + `
  outcome "all_succeeded" { next = state.done }
  outcome "any_failed" { next = state.failed }
}

state "done" {
  terminal = true
}

state "failed" {
  terminal = true
  success  = false
}
`
}

// cri39ChildHCL returns a child workflow HCL that runs the fake adapter in
// parallel with the given list and cap.
func cri39ChildHCL(parallelList string, parallelMax int) string {
	return `workflow {
  name          = "child"
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
}

adapter "fake" "default" {}

step "run" {
  target       = adapter.fake.default
  parallel     = ` + parallelList + `
  parallel_max = ` + strconv.Itoa(parallelMax) + `
  outcome "all_succeeded" { next = state.done }
  outcome "any_failed" { next = state.failed }
}

state "done" {
  terminal = true
}

state "failed" {
  terminal = true
  success  = false
}
`
}

// cri39SequentialChildHCL returns a child workflow that uses for_each (not
// parallel) over the fake adapter.
func cri39SequentialChildHCL(list string) string {
	return `workflow {
  name          = "child"
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
}

adapter "fake" "default" {}

step "run" {
  target   = adapter.fake.default
  for_each = ` + list + `
  outcome "all_succeeded" { next = state.done }
  outcome "any_failed" { next = state.failed }
}

state "done" {
  terminal = true
}

state "failed" {
  terminal = true
  success  = false
}
`
}

// TestCRI39_NestedParallelMaxBoundsSubtree (Shape A) verifies that an outer
// parallel_max=2 caps the whole subtree even when the inner parallel step
// declares parallel_max=8.
func TestCRI39_NestedParallelMaxBoundsSubtree(t *testing.T) {
	p := cri39NewAdapter(50 * time.Millisecond)
	g := cri39Compile(t, map[string]string{
		"main.chcl":       cri39ParentHCL(cri39HCLList(8), 2),
		"child/main.chcl": cri39ChildHCL(cri39HCLList(8), 8),
	})
	elapsed := cri39Run(t, g, p)

	if peak := cri39Peak(p); peak > 2 {
		t.Errorf("peak concurrent executions = %d; want <= 2", peak)
	}
	if elapsed < 1500*time.Millisecond {
		t.Errorf("elapsed = %s; want >= 1.5s (ceiling should serialize the tree)", elapsed)
	}
}

// TestCRI39_InnerCapLowerThanOuterCap (Shape B) verifies that when the inner
// parallel step has the lower cap, the subtree-wide ceiling is the inner cap.
func TestCRI39_InnerCapLowerThanOuterCap(t *testing.T) {
	p := cri39NewAdapter(25 * time.Millisecond)
	g := cri39Compile(t, map[string]string{
		"main.chcl":       cri39ParentHCL(cri39HCLList(8), 2),
		"child/main.chcl": cri39ChildHCL(cri39HCLList(8), 1),
	})
	cri39Run(t, g, p)

	if peak := cri39Peak(p); peak > 1 {
		t.Errorf("peak concurrent executions = %d; want <= 1", peak)
	}
}

// TestCRI39_OuterFourInnerFour (Shape C) verifies the ceiling is the minimum
// when both parallel steps declare the same cap.
func TestCRI39_OuterFourInnerFour(t *testing.T) {
	p := cri39NewAdapter(25 * time.Millisecond)
	g := cri39Compile(t, map[string]string{
		"main.chcl":       cri39ParentHCL(cri39HCLList(4), 4),
		"child/main.chcl": cri39ChildHCL(cri39HCLList(4), 4),
	})
	cri39Run(t, g, p)

	if peak := cri39Peak(p); peak > 4 {
		t.Errorf("peak concurrent executions = %d; want <= 4", peak)
	}
}

// TestCRI39_DeeplyNestedParallel (Shape F) verifies the ceiling propagates
// through three levels of nested parallel subworkflows.
func TestCRI39_DeeplyNestedParallel(t *testing.T) {
	p := cri39NewAdapter(25 * time.Millisecond)
	g := cri39Compile(t, map[string]string{
		"main.chcl": cri39ParentHCL(cri39HCLList(2), 2),
		"child/main.chcl": `workflow {
  name          = "child"
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
}

subworkflow "grandchild" {
  source = "./grandchild"
}

adapter "fake" "default" {}

step "run" {
  target       = subworkflow.grandchild
  parallel     = ` + cri39HCLList(2) + `
  parallel_max = 2
  outcome "all_succeeded" { next = state.done }
  outcome "any_failed" { next = state.failed }
}

state "done" {
  terminal = true
}

state "failed" {
  terminal = true
  success  = false
}
`,
		"child/grandchild/main.chcl": cri39ChildHCL(cri39HCLList(2), 2),
	})
	cri39Run(t, g, p)

	if peak := cri39Peak(p); peak > 2 {
		t.Errorf("peak concurrent executions = %d; want <= 2", peak)
	}
}

// TestCRI39_SingleLevelParallel (Shape D) verifies the existing single-level
// parallel_max enforcement is preserved.
func TestCRI39_SingleLevelParallel(t *testing.T) {
	p := cri39NewAdapter(50 * time.Millisecond)
	g := cri39Compile(t, map[string]string{
		"main.chcl": `workflow {
  name          = "root"
  version       = "0.1"
  initial_state = "work"
  target_state  = "done"
}

adapter "fake" "default" {}

step "work" {
  target       = adapter.fake.default
  parallel     = ` + cri39HCLList(8) + `
  parallel_max = 2
  outcome "all_succeeded" { next = state.done }
  outcome "any_failed" { next = state.failed }
}

state "done" {
  terminal = true
}

state "failed" {
  terminal = true
  success  = false
}
`,
	})
	elapsed := cri39Run(t, g, p)

	if peak := cri39Peak(p); peak > 2 {
		t.Errorf("peak concurrent executions = %d; want <= 2", peak)
	}
	if elapsed < 150*time.Millisecond {
		t.Errorf("elapsed = %s; want >= 150ms", elapsed)
	}
}

// TestCRI39_SequentialSubworkflowInsideParallelParent (Shape E) verifies that a
// sequential (for_each) subworkflow inside a parallel parent is bounded by the
// parent's cap.
func TestCRI39_SequentialSubworkflowInsideParallelParent(t *testing.T) {
	p := cri39NewAdapter(25 * time.Millisecond)
	g := cri39Compile(t, map[string]string{
		"main.chcl":       cri39ParentHCL(cri39HCLList(4), 2),
		"child/main.chcl": cri39SequentialChildHCL(cri39HCLList(8)),
	})
	cri39Run(t, g, p)

	if peak := cri39Peak(p); peak > 2 {
		t.Errorf("peak concurrent executions = %d; want <= 2", peak)
	}
}

// cri39SiblingChildHCL returns a child workflow with two sequential parallel
// adapter steps. step_a uses parallel_max=aMax and transitions to step_b, which
// uses parallel_max=bMax.
func cri39SiblingChildHCL(list string, aMax, bMax int) string {
	return `workflow {
  name          = "child"
  version       = "0.1"
  initial_state = "a"
  target_state  = "done"
}

adapter "fake" "default" {}

step "a" {
  target       = adapter.fake.default
  parallel     = ` + list + `
  parallel_max = ` + strconv.Itoa(aMax) + `
  outcome "all_succeeded" { next = state.b }
  outcome "any_failed" { next = state.failed }
}

step "b" {
  target       = adapter.fake.default
  parallel     = ` + list + `
  parallel_max = ` + strconv.Itoa(bMax) + `
  outcome "all_succeeded" { next = state.done }
  outcome "any_failed" { next = state.failed }
}

state "done" {
  terminal = true
}

state "failed" {
  terminal = true
  success  = false
}
`
}

// TestCRI39_SiblingStepsRespectSubtreeCeiling verifies that sibling parallel
// adapter steps inside a parallel subworkflow share the inherited subtree
// ceiling. The outer step caps the subtree at 2; step_a matches the ceiling and
// reuses the subtree semaphore, while step_b lowers its own cap to 1 and uses
// a per-step semaphore. Without also contending on the subtree semaphore,
// step_b leaves running in different parent iterations could push aggregate
// concurrency above 2.
func TestCRI39_SiblingStepsRespectSubtreeCeiling(t *testing.T) {
	p := cri39NewAdapter(50 * time.Millisecond)
	g := cri39Compile(t, map[string]string{
		"main.chcl":       cri39ParentHCL(cri39HCLList(4), 2),
		"child/main.chcl": cri39SiblingChildHCL(cri39HCLList(3), 2, 1),
	})
	cri39Run(t, g, p)

	if peak := cri39Peak(p); peak > 2 {
		t.Errorf("peak concurrent executions = %d; want <= 2", peak)
	}
}

// TestCRI39_SequentialOuterParallelInner (Neg shape) verifies that a sequential
// outer step does not impose a ceiling on the inner parallel step.
func TestCRI39_SequentialOuterParallelInner(t *testing.T) {
	p := cri39NewAdapter(25 * time.Millisecond)
	g := cri39Compile(t, map[string]string{
		"main.chcl": `workflow {
  name          = "root"
  version       = "0.1"
  initial_state = "work"
  target_state  = "done"
}

subworkflow "child" {
  source = "./child"
}

step "work" {
  target   = subworkflow.child
  for_each = ` + cri39HCLList(4) + `
  outcome "all_succeeded" { next = state.done }
  outcome "any_failed" { next = state.failed }
}

state "done" {
  terminal = true
}

state "failed" {
  terminal = true
  success  = false
}
`,
		"child/main.chcl": cri39ChildHCL(cri39HCLList(8), 8),
	})
	cri39Run(t, g, p)

	if peak := cri39Peak(p); peak > 8 {
		t.Errorf("peak concurrent executions = %d; want <= 8", peak)
	}
}
