package engine

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/brokenbots/overlord/overseer/internal/adapter"
	"github.com/brokenbots/overlord/workflow"
)

// fakeSink records engine callbacks for assertion.
type fakeSink struct {
	mu          sync.Mutex
	stepsRun    []string
	transitions []string
	terminal    string
	terminalOK  bool
	failure     string
}

func (s *fakeSink) OnRunStarted(string, string) {}
func (s *fakeSink) OnRunCompleted(state string, ok bool) {
	s.mu.Lock()
	s.terminal = state
	s.terminalOK = ok
	s.mu.Unlock()
}
func (s *fakeSink) OnRunFailed(reason, step string) { s.failure = reason }
func (s *fakeSink) OnStepEntered(step, _ string, _ int) {
	s.mu.Lock()
	s.stepsRun = append(s.stepsRun, step)
	s.mu.Unlock()
}
func (s *fakeSink) OnStepOutcome(string, string, time.Duration, error) {}
func (s *fakeSink) OnStepTransition(from, to, via string) {
	s.mu.Lock()
	s.transitions = append(s.transitions, from+"->"+to)
	s.mu.Unlock()
}
func (s *fakeSink) OnStepResumed(string, int, string)           {}
func (s *fakeSink) StepEventSink(step string) adapter.EventSink { return noopSink{} }

type noopSink struct{}

func (noopSink) Log(string, []byte)  {}
func (noopSink) Adapter(string, any) {}

// fakeAdapter returns a programmable outcome.
type fakeAdapter struct {
	name    string
	outcome string
	err     error
}

func (a *fakeAdapter) Name() string { return a.name }
func (a *fakeAdapter) Execute(_ context.Context, _ *workflow.StepNode, _ adapter.EventSink) (adapter.Result, error) {
	return adapter.Result{Outcome: a.outcome}, a.err
}

type fakeDispatcher map[string]adapter.Adapter

func (d fakeDispatcher) Adapter(name string) (adapter.Adapter, bool) {
	a, ok := d[name]
	return a, ok
}

func compile(t *testing.T, src string) *workflow.FSMGraph {
	t.Helper()
	spec, diags := workflow.Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := workflow.Compile(spec)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	return g
}

func TestEngineHappyPath(t *testing.T) {
	g := compile(t, `
workflow "t" {
  version = "0.1"
  initial_state = "a"
  target_state  = "done"
  step "a" {
    adapter = "fake"
    outcome "success" { transition_to = "b" }
  }
  step "b" {
    adapter = "fake"
    outcome "success" { transition_to = "done" }
  }
  state "done" { terminal = true }
}`)
	sink := &fakeSink{}
	disp := fakeDispatcher{"fake": &fakeAdapter{name: "fake", outcome: "success"}}
	if err := New(g, disp, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if sink.terminal != "done" || !sink.terminalOK {
		t.Errorf("terminal: %s ok=%v", sink.terminal, sink.terminalOK)
	}
	if len(sink.stepsRun) != 2 {
		t.Errorf("steps run: %v", sink.stepsRun)
	}
}

func TestEngineErrorMappedToFailureOutcome(t *testing.T) {
	g := compile(t, `
workflow "t" {
  version = "0.1"
  initial_state = "a"
  target_state  = "fail"
  step "a" {
    adapter = "fake"
    outcome "success" { transition_to = "ok" }
    outcome "failure" { transition_to = "fail" }
  }
  state "ok" { terminal = true }
  state "fail" {
    terminal = true
    success  = false
  }
}`)
	sink := &fakeSink{}
	disp := fakeDispatcher{"fake": &fakeAdapter{name: "fake", outcome: "", err: errors.New("boom")}}
	if err := New(g, disp, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if sink.terminal != "fail" || sink.terminalOK {
		t.Errorf("terminal: %s ok=%v", sink.terminal, sink.terminalOK)
	}
}

func TestEngineMaxStepsGuard(t *testing.T) {
	g := compile(t, `
workflow "t" {
  version = "0.1"
  initial_state = "a"
  target_state  = "done"
  step "a" {
    adapter = "fake"
    outcome "again" { transition_to = "a" }
  }
  state "done" { terminal = true }
  policy { max_total_steps = 3 }
}`)
	sink := &fakeSink{}
	disp := fakeDispatcher{"fake": &fakeAdapter{name: "fake", outcome: "again"}}
	err := New(g, disp, sink).Run(context.Background())
	if err == nil {
		t.Fatal("expected loop guard error")
	}
}
