package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/brokenbots/overseer/internal/plugin"
	engineruntime "github.com/brokenbots/overseer/internal/engine/runtime"
)

func TestNodeForDispatchesStepStateAndUnknown(t *testing.T) {
	g := compile(t, `
workflow "t" {
  version = "0.1"
  initial_state = "a"
  target_state  = "done"
  step "a" {
    adapter = "fake"
    outcome "success" { transition_to = "done" }
  }
  state "done" { terminal = true }
}`)

	node, err := nodeFor(g, "a")
	if err != nil {
		t.Fatalf("step dispatch: %v", err)
	}
	if _, ok := node.(*stepNode); !ok {
		t.Fatalf("step dispatch type = %T, want *stepNode", node)
	}

	node, err = nodeFor(g, "done")
	if err != nil {
		t.Fatalf("state dispatch: %v", err)
	}
	if _, ok := node.(*stateNode); !ok {
		t.Fatalf("state dispatch type = %T, want *stateNode", node)
	}

	_, err = nodeFor(g, "missing")
	var unknown *UnknownNodeError
	if !errors.As(err, &unknown) {
		t.Fatalf("missing node error type = %T, want *UnknownNodeError", err)
	}
}

func TestRunFromUnknownNodeSurfacesTypedFailure(t *testing.T) {
	g := compile(t, `
workflow "t" {
  version = "0.1"
  initial_state = "a"
  target_state  = "done"
  step "a" {
    adapter = "fake"
    outcome "success" { transition_to = "done" }
  }
  state "done" { terminal = true }
}`)

	sink := &fakeSink{}
	loader := &fakeLoader{plugins: map[string]plugin.Plugin{"fake": &fakePlugin{name: "fake", outcome: "success"}}}
	err := New(g, loader, sink).RunFrom(context.Background(), "missing", 1)
	if err == nil {
		t.Fatal("expected unknown node error")
	}
	var unknown *UnknownNodeError
	if !errors.As(err, &unknown) {
		t.Fatalf("error type = %T, want *UnknownNodeError", err)
	}
	if sink.failure != err.Error() {
		t.Fatalf("sink failure = %q, want %q", sink.failure, err.Error())
	}
}

func TestRunStateTotalStepsIncrementsOnlyForStepEvaluation(t *testing.T) {
	g := compile(t, `
workflow "t" {
  version = "0.1"
  initial_state = "a"
  target_state  = "done"
  step "a" {
    adapter = "fake"
    outcome "success" { transition_to = "done" }
  }
  state "done" { terminal = true }
}`)

	sink := &fakeSink{}
	loader := &fakeLoader{plugins: map[string]plugin.Plugin{"fake": &fakePlugin{name: "fake", outcome: "success"}}}
	sessions := plugin.NewSessionManager(loader)
	t.Cleanup(func() { sessions.Shutdown(context.Background()) })

	deps := Deps{Sessions: sessions, Sink: sink}
	st := &RunState{Current: "a", firstStep: true, firstStepAttempt: 1}

	step, err := nodeFor(g, "a")
	if err != nil {
		t.Fatalf("step node: %v", err)
	}
	next, err := step.Evaluate(context.Background(), st, deps)
	if err != nil {
		t.Fatalf("step evaluate: %v", err)
	}
	if next != "done" {
		t.Fatalf("next=%q want done", next)
	}
	if st.TotalSteps != 1 {
		t.Fatalf("after step total steps=%d want 1", st.TotalSteps)
	}

	st.Current = "done"
	state, err := nodeFor(g, "done")
	if err != nil {
		t.Fatalf("state node: %v", err)
	}
	_, err = state.Evaluate(context.Background(), st, deps)
	if !errors.Is(err, engineruntime.ErrTerminal) {
		t.Fatalf("state evaluate err=%v want ErrTerminal", err)
	}
	if st.TotalSteps != 1 {
		t.Fatalf("after state total steps=%d want 1", st.TotalSteps)
	}
}

func TestInterpreterMaxTotalStepsGuardUsesExistingReason(t *testing.T) {
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
	loader := &fakeLoader{plugins: map[string]plugin.Plugin{"fake": &fakePlugin{name: "fake", outcome: "again"}}}
	err := New(g, loader, sink).Run(context.Background())
	if err == nil {
		t.Fatal("expected max_total_steps error")
	}
	want := "policy.max_total_steps exceeded (3)"
	if sink.failure != want {
		t.Fatalf("failure reason = %q, want %q", sink.failure, want)
	}
}

func TestInterpreterCompletesOnTerminalState(t *testing.T) {
	g := compile(t, `
workflow "t" {
  version = "0.1"
  initial_state = "a"
  target_state  = "done"
  step "a" {
    adapter = "fake"
    outcome "success" { transition_to = "done" }
  }
  state "done" {
    terminal = true
    success = true
  }
}`)

	sink := &fakeSink{}
	loader := &fakeLoader{plugins: map[string]plugin.Plugin{"fake": &fakePlugin{name: "fake", outcome: "success"}}}
	err := New(g, loader, sink).Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if sink.terminal != "done" || !sink.terminalOK {
		t.Fatalf("terminal=%q ok=%v want done/true", sink.terminal, sink.terminalOK)
	}
}
