package engine

import (
	"context"
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
