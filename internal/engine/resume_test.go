package engine

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/brokenbots/criteria/internal/adapterhost"
)

const multiStepWorkflow = `
workflow "t" {
  version = "0.1"
  initial_state = "step1"
  target_state  = "done"
}
step "step1" {
  target = adapter.fake
  outcome "success" { next = "step2" }
}
step "step2" {
  target = adapter.fake
  outcome "success" { next = "done" }
}
state "done" { terminal = true }
policy { max_step_retries = 2 }`

// trackSink extends fakeSink to record step-resumed calls.
type trackSink struct {
	fakeSink
	resumedMu sync.Mutex
	resumed   []string // "step:attempt"
}

func (s *trackSink) OnStepResumed(step string, attempt int, reason string) {
	s.resumedMu.Lock()
	s.resumed = append(s.resumed, step)
	s.resumedMu.Unlock()
}

func TestResume_HappyPath(t *testing.T) {
	g := compile(t, multiStepWorkflow)
	sink := &trackSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"fake": &fakeAdapter{name: "fake", outcome: "success"}}}

	eng := New(g, loader, sink)
	// Simulate crash at step2 attempt 1 — resume from step2 at attempt 2.
	// RunFrom does NOT call OnRunStarted; it starts execution from step2.
	if err := eng.RunFrom(context.Background(), "step2", 2); err != nil {
		t.Fatalf("RunFrom: %v", err)
	}
	if sink.terminal != "done" || !sink.terminalOK {
		t.Errorf("terminal=%q ok=%v want done/true", sink.terminal, sink.terminalOK)
	}
	// Only step2 should have been run (step1 was already done before the crash).
	if len(sink.stepsRun) != 1 {
		t.Errorf("stepsRun=%v want [step2]", sink.stepsRun)
	}
	if sink.stepsRun[0] != "step2" {
		t.Errorf("stepsRun[0]=%q want step2", sink.stepsRun[0])
	}
	// The attempt number should be 2 (the value passed to RunFrom).
	// We verify through the OnStepEntered call count — just 1 entry at attempt 2.
	// (fakeSink records steps but not attempts; let's check via a custom sink)
}

func TestResume_RunFrom_AttemptOffsetApplied(t *testing.T) {
	g := compile(t, multiStepWorkflow) // max_step_retries = 2, so maxAttempts = 3

	type entry struct {
		step    string
		attempt int
	}
	var mu sync.Mutex
	var entries []entry

	sink := &attemptTrackSink{
		fakeSink: fakeSink{},
		onStepEntered: func(step string, attempt int) {
			mu.Lock()
			entries = append(entries, entry{step, attempt})
			mu.Unlock()
		},
	}

	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"fake": &fakeAdapter{name: "fake", outcome: "success"}}}
	eng := New(g, loader, sink)
	// Resume from step1 at attempt 2 (already failed once).
	if err := eng.RunFrom(context.Background(), "step1", 2); err != nil {
		t.Fatalf("RunFrom: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	// step1 attempt=2, step2 attempt=1
	if len(entries) != 2 {
		t.Fatalf("expected 2 step entries, got %d: %v", len(entries), entries)
	}
	if entries[0].step != "step1" || entries[0].attempt != 2 {
		t.Errorf("entries[0]=%v want {step1 2}", entries[0])
	}
	if entries[1].step != "step2" || entries[1].attempt != 1 {
		t.Errorf("entries[1]=%v want {step2 1}", entries[1])
	}
}

func TestResume_RespectsMaxRetries(t *testing.T) {
	g := compile(t, multiStepWorkflow) // max_step_retries = 2, maxAttempts = 3

	sink := &fakeSink{}
	// Adapter always fails.
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"fake": &fakeAdapter{name: "fake", err: errors.New("always fails")}}}

	eng := New(g, loader, sink)
	// Resume at attempt 3 (= maxAttempts): only 1 attempt allowed; it fails.
	err := eng.RunFrom(context.Background(), "step1", 3)
	if err == nil {
		t.Fatal("expected error when last attempt fails")
	}
	if sink.failure == "" {
		t.Fatal("expected OnRunFailed to be called")
	}
}

func TestResume_ExceedsMaxRetries_FailsImmediately(t *testing.T) {
	g := compile(t, multiStepWorkflow) // max_step_retries = 2, maxAttempts = 3

	sink := &fakeSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"fake": &fakeAdapter{name: "fake", outcome: "success"}}}

	eng := New(g, loader, sink)
	// Resume at attempt 4 (exceeds maxAttempts=3): no attempts possible.
	err := eng.RunFrom(context.Background(), "step1", 4)
	if err == nil {
		t.Fatal("expected error when attempt exceeds max")
	}
}

// attemptTrackSink wraps fakeSink and calls onStepEntered with step+attempt.
type attemptTrackSink struct {
	fakeSink
	onStepEntered func(step string, attempt int)
}

func (s *attemptTrackSink) OnStepEntered(step, adapter string, attempt int) {
	s.fakeSink.OnStepEntered(step, adapter, attempt)
	if s.onStepEntered != nil {
		s.onStepEntered(step, attempt)
	}
}

func (s *attemptTrackSink) OnStepResumed(string, int, string) {}

// Ensure attemptTrackSink also satisfies OnStepOutcome from the base.
func (s *attemptTrackSink) OnStepOutcome(step, outcome string, dur time.Duration, err error) {
	s.fakeSink.OnStepOutcome(step, outcome, dur, err)
}
