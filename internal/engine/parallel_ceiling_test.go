package engine

import (
	"context"
	"testing"

	"github.com/brokenbots/criteria/internal/adapterhost"
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
