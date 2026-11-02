package engine

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

// ceilingCaptureSink records the top-level RunState's ParallelCeiling at
// terminal time (clearLiveRunState has not yet run when OnRunCompleted fires).
type ceilingCaptureSink struct {
	fakeSink
	onEntered func()
}

func (s *ceilingCaptureSink) OnRunCompleted(finalState string, success bool) {
	s.fakeSink.OnRunCompleted(finalState, success)
}

func (s *ceilingCaptureSink) OnStepEntered(step, adapterName string, attempt int) {
	if s.onEntered != nil {
		s.onEntered()
	}
	s.fakeSink.OnStepEntered(step, adapterName, attempt)
}

// TestWithParallelCeilingSetsTopLevelRunState verifies the serve-adapter
// concurrency option (ADR-0008) lands in the top-level RunState so inline
// workflow bodies and subworkflows inherit the shared container budget.
func TestWithParallelCeilingSetsTopLevelRunState(t *testing.T) {
	g := compile(t, `
workflow {
  name = "t"
  version = "0.1"
  initial_state = "a"
  target_state  = "done"
}
step "a" {
  target = adapter.fake
  outcome "success" { next = state.done }
  outcome "error" { next = state.done }
}
state "done" { terminal = true }`)
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"fake": &fakeAdapter{name: "fake", outcome: "success"}}}

	capture := &ceilingCaptureSink{}
	e := New(g, loader, capture, WithParallelCeiling(3))
	capture.onEntered = func() {
		// liveRunState is the runLoop's RunState until clearLiveRunState
		// (deferred) runs after this callback returns.
		if got := e.liveRunState.ParallelCeiling; got != 3 {
			t.Errorf("RunState.ParallelCeiling = %d, want 3", got)
		}
	}
	if err := e.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if capture.terminal != "done" || !capture.terminalOK {
		t.Fatalf("terminal: %s ok=%v", capture.terminal, capture.terminalOK)
	}
}

func TestWithParallelCeilingClampsLeafSemaphore(t *testing.T) {
	// Unit level: the effective cap computation itself. Mechanics 4 of
	// ADR-0008: the serve-adapter container ceiling wins over the step's own
	// parallel_max; with no ceiling the step's own cap stays in force.
	if got := effectiveParallelMax(8, 2); got != 2 {
		t.Errorf("effectiveParallelMax(8, 2) = %d, want 2 (ceiling wins)", got)
	}
	if got := effectiveParallelMax(8, 0); got != 8 {
		t.Errorf("effectiveParallelMax(8, 0) = %d, want 8 (no ceiling: step cap)", got)
	}

	// Unit level: a capped step's semaphores are built with the clamped
	// capacity, including the shared leaf semaphore that all adapter
	// executions of the step contend on.
	n := &stepNode{step: &workflow.StepNode{ParallelMax: 8}}
	st := &RunState{ParallelCeiling: 2}
	launch, leaf, eff := n.parallelSemaphores(st)
	if eff != 2 {
		t.Errorf("effectiveMax = %d, want 2", eff)
	}
	if cap(launch) != 2 {
		t.Errorf("launch semaphore capacity = %d, want 2", cap(launch))
	}
	if cap(leaf) != 2 {
		t.Errorf("leaf semaphore capacity = %d, want 2 (clamped by the container ceiling)", cap(leaf))
	}

	// End-to-end: under a container ceiling of 2, eight parallel iterations
	// never reach more than two simultaneous adapter executions, while the
	// same workflow without the ceiling is not trivially serial — proving the
	// ceiling is the binding clamp, not an accidental serialization.
	list := cri39HCLList(8)
	steps := `
step "work" {
  target       = adapter.fake
  parallel     = ` + list + `
  parallel_max = 8
  outcome "all_succeeded" { next = step.done }
  outcome "any_failed"    { next = step.failed }
}`

	p := cri39NewAdapter(30 * time.Millisecond)
	g := compile(t, parallelWorkflowHCL(steps))
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"fake":         p,
		"fake.default": p,
	}}
	eng := NewTestEngine(g, loader, &fakeSink{}, WithWorkflowDir(g.WorkflowDir), WithParallelCeiling(2))
	if err := eng.Run(context.Background()); err != nil {
		t.Fatalf("capped run: %v", err)
	}
	if peak := cri39Peak(p); peak > 2 {
		t.Errorf("capped run executed %d adapter calls concurrently, want at most 2", peak)
	}

	p2 := cri39NewAdapter(30 * time.Millisecond)
	g2 := compile(t, parallelWorkflowHCL(steps))
	loader2 := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"fake":         p2,
		"fake.default": p2,
	}}
	eng2 := NewTestEngine(g2, loader2, &fakeSink{}, WithWorkflowDir(g2.WorkflowDir))
	if err := eng2.Run(context.Background()); err != nil {
		t.Fatalf("uncapped run: %v", err)
	}
	if peak := cri39Peak(p2); peak <= 2 {
		t.Errorf("uncapped run peaked at %d concurrent executions, want > 2 (the ceiling clamp must be what binds the capped run)", peak)
	}
}

// TestWithParallelCeilingNonPositiveUnbounded verifies that a non-positive
// override leaves the default unbounded posture (ParallelCeiling zero) intact.
func TestWithParallelCeilingNonPositiveUnbounded(t *testing.T) {
	g := compile(t, `
workflow {
  name = "t"
  version = "0.1"
  initial_state = "a"
  target_state  = "done"
}
step "a" {
  target = adapter.fake
  outcome "success" { next = state.done }
  outcome "error" { next = state.done }
}
state "done" { terminal = true }`)
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"fake": &fakeAdapter{name: "fake", outcome: "success"}}}

	for _, bad := range []int{0, -2} {
		capture := &ceilingCaptureSink{}
		e := New(g, loader, capture, WithParallelCeiling(bad))
		capture.onEntered = func() {
			if n := e.liveRunState.ParallelCeiling; n != 0 {
				t.Errorf("WithParallelCeiling(%d): RunState.ParallelCeiling = %d, want 0 (unbounded)", bad, n)
			}
		}
		if err := e.Run(context.Background()); err != nil {
			t.Fatalf("run (bad=%d): %v", bad, err)
		}
	}
}

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

// TestParallelCeiling_NestedParallelMaxBoundsSubtree (regression for CRI-39) verifies
// that an outer parallel_max=2 caps the whole subtree even when the inner parallel step
// declares parallel_max=8.
func TestParallelCeiling_NestedParallelMaxBoundsSubtree(t *testing.T) {
	p := cri39NewAdapter(5 * time.Millisecond)
	g := cri39Compile(t, map[string]string{
		"main.chcl":       cri39ParentHCL(cri39HCLList(8), 2),
		"child/main.chcl": cri39ChildHCL(cri39HCLList(8), 8),
	})
	elapsed := cri39Run(t, g, p)

	if peak := cri39Peak(p); peak > 2 {
		t.Errorf("peak concurrent executions = %d; want <= 2", peak)
	}
	if elapsed < 100*time.Millisecond {
		t.Errorf("elapsed = %s; want >= 100ms (ceiling should serialize the tree)", elapsed)
	}
}

// TestParallelCeiling_InnerCapLowerThanOuterCap (regression for CRI-39) verifies that
// when the inner parallel step has the lower cap, the subtree-wide ceiling is the inner cap.
func TestParallelCeiling_InnerCapLowerThanOuterCap(t *testing.T) {
	p := cri39NewAdapter(2 * time.Millisecond)
	g := cri39Compile(t, map[string]string{
		"main.chcl":       cri39ParentHCL(cri39HCLList(8), 2),
		"child/main.chcl": cri39ChildHCL(cri39HCLList(8), 1),
	})
	cri39Run(t, g, p)

	if peak := cri39Peak(p); peak > 1 {
		t.Errorf("peak concurrent executions = %d; want <= 1", peak)
	}
}

// TestParallelCeiling_OuterFourInnerFour (regression for CRI-39) verifies the ceiling
// is the minimum when both parallel steps declare the same cap.
func TestParallelCeiling_OuterFourInnerFour(t *testing.T) {
	p := cri39NewAdapter(2 * time.Millisecond)
	g := cri39Compile(t, map[string]string{
		"main.chcl":       cri39ParentHCL(cri39HCLList(4), 4),
		"child/main.chcl": cri39ChildHCL(cri39HCLList(4), 4),
	})
	cri39Run(t, g, p)

	if peak := cri39Peak(p); peak > 4 {
		t.Errorf("peak concurrent executions = %d; want <= 4", peak)
	}
}

// TestParallelCeiling_DeeplyNestedParallel (regression for CRI-39) verifies the ceiling
// propagates through three levels of nested parallel subworkflows.
func TestParallelCeiling_DeeplyNestedParallel(t *testing.T) {
	p := cri39NewAdapter(2 * time.Millisecond)
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

// TestParallelCeiling_SingleLevelParallel verifies single-level parallel_max enforcement.
func TestParallelCeiling_SingleLevelParallel(t *testing.T) {
	p := cri39NewAdapter(5 * time.Millisecond)
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
	if elapsed < 15*time.Millisecond {
		t.Errorf("elapsed = %s; want >= 15ms", elapsed)
	}
}

// TestParallelCeiling_SequentialSubworkflowInsideParallelParent verifies that a
// sequential (for_each) subworkflow inside a parallel parent is bounded by the parent's cap.
func TestParallelCeiling_SequentialSubworkflowInsideParallelParent(t *testing.T) {
	p := cri39NewAdapter(2 * time.Millisecond)
	g := cri39Compile(t, map[string]string{
		"main.chcl":       cri39ParentHCL(cri39HCLList(4), 2),
		"child/main.chcl": cri39SequentialChildHCL(cri39HCLList(8)),
	})
	cri39Run(t, g, p)

	if peak := cri39Peak(p); peak > 2 {
		t.Errorf("peak concurrent executions = %d; want <= 2", peak)
	}
}

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

// TestParallelCeiling_SiblingStepsRespectSubtreeCeiling verifies that sibling parallel
// adapter steps inside a parallel subworkflow share the inherited subtree ceiling.
func TestParallelCeiling_SiblingStepsRespectSubtreeCeiling(t *testing.T) {
	p := cri39NewAdapter(5 * time.Millisecond)
	g := cri39Compile(t, map[string]string{
		"main.chcl":       cri39ParentHCL(cri39HCLList(4), 2),
		"child/main.chcl": cri39SiblingChildHCL(cri39HCLList(3), 2, 1),
	})
	cri39Run(t, g, p)

	if peak := cri39Peak(p); peak > 2 {
		t.Errorf("peak concurrent executions = %d; want <= 2", peak)
	}
}

func cri39SubworkflowSiblingChildHCL(list string, aMax int) string {
	return `workflow {
  name          = "child"
  version       = "0.1"
  initial_state = "a"
  target_state  = "done"
}

adapter "fake" "default" {}

subworkflow "grandchild" {
  source = "./grandchild"
}

step "a" {
  target       = adapter.fake.default
  parallel     = ` + list + `
  parallel_max = ` + strconv.Itoa(aMax) + `
  outcome "all_succeeded" { next = state.b }
  outcome "any_failed" { next = state.failed }
}

step "b" {
  target       = subworkflow.grandchild
  parallel     = ` + list + `
  parallel_max = 1
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

// TestParallelCeiling_SubworkflowSiblingRespectsSubtreeCeiling verifies the subworkflow
// variant of the sibling case.
func TestParallelCeiling_SubworkflowSiblingRespectsSubtreeCeiling(t *testing.T) {
	p := cri39NewAdapter(5 * time.Millisecond)
	g := cri39Compile(t, map[string]string{
		"main.chcl":                  cri39ParentHCL(cri39HCLList(4), 2),
		"child/main.chcl":            cri39SubworkflowSiblingChildHCL(cri39HCLList(3), 2),
		"child/grandchild/main.chcl": cri39ChildHCL(cri39HCLList(8), 8),
	})
	cri39Run(t, g, p)

	if peak := cri39Peak(p); peak > 2 {
		t.Errorf("peak concurrent executions = %d; want <= 2", peak)
	}
}

// TestParallelCeiling_SequentialOuterParallelInner verifies that a sequential
// outer step does not impose a ceiling on the inner parallel step.
func TestParallelCeiling_SequentialOuterParallelInner(t *testing.T) {
	p := cri39NewAdapter(2 * time.Millisecond)
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
