package engine_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/engine"
	"github.com/brokenbots/criteria/internal/plugin"
	"github.com/brokenbots/criteria/workflow"
)

// --- forEachSink and friends ---

// forEachSink records events from for_each iterations and terminal state.
type forEachSink struct {
	mu         sync.Mutex
	entered    []feEnteredEvent
	iterations []feIterEvent
	outcomes   []feOutcomeEvent
	steps      []string
	terminal   string
	terminalOK bool
	cursorSets []string
}

type feEnteredEvent struct {
	node  string
	count int
}

type feIterEvent struct {
	node      string
	index     int
	value     string
	anyFailed bool
}

type feOutcomeEvent struct {
	node    string
	outcome string
	target  string
}

func (s *forEachSink) OnRunStarted(string, string) {}
func (s *forEachSink) OnRunCompleted(state string, ok bool) {
	s.mu.Lock()
	s.terminal = state
	s.terminalOK = ok
	s.mu.Unlock()
}
func (s *forEachSink) OnRunFailed(reason, _ string) {
	s.mu.Lock()
	s.terminal = "FAILED:" + reason
	s.mu.Unlock()
}
func (s *forEachSink) OnStepEntered(step, _ string, _ int) {
	s.mu.Lock()
	s.steps = append(s.steps, step)
	s.mu.Unlock()
}
func (s *forEachSink) OnStepOutcome(string, string, time.Duration, error)           {}
func (s *forEachSink) OnStepTransition(string, string, string)                      {}
func (s *forEachSink) OnStepResumed(string, int, string)                            {}
func (s *forEachSink) OnVariableSet(string, string, string)                         {}
func (s *forEachSink) OnStepOutputCaptured(string, map[string]string)               {}
func (s *forEachSink) OnRunPaused(string, string, string)                           {}
func (s *forEachSink) OnWaitEntered(string, string, string, string)                 {}
func (s *forEachSink) OnWaitResumed(string, string, string, map[string]string)      {}
func (s *forEachSink) OnApprovalRequested(string, []string, string)                 {}
func (s *forEachSink) OnApprovalDecision(string, string, string, map[string]string) {}
func (s *forEachSink) OnBranchEvaluated(string, string, string, string)             {}
func (s *forEachSink) OnForEachEntered(node string, count int) {
	s.mu.Lock()
	s.entered = append(s.entered, feEnteredEvent{node: node, count: count})
	s.mu.Unlock()
}
func (s *forEachSink) OnForEachIteration(node string, index int, value string, anyFailed bool) {
	s.mu.Lock()
	s.iterations = append(s.iterations, feIterEvent{node: node, index: index, value: value, anyFailed: anyFailed})
	s.mu.Unlock()
}
func (s *forEachSink) OnForEachOutcome(node, outcome, target string) {
	s.mu.Lock()
	s.outcomes = append(s.outcomes, feOutcomeEvent{node: node, outcome: outcome, target: target})
	s.mu.Unlock()
}
func (s *forEachSink) OnScopeIterCursorSet(v string) {
	s.mu.Lock()
	s.cursorSets = append(s.cursorSets, v)
	s.mu.Unlock()
}
func (s *forEachSink) StepEventSink(string) adapter.EventSink { return noopAdapterSink{} }

// --- Reusable fake plugin implementations ---

// staticPlugin always returns the same outcome.
type staticPlugin struct {
	name    string
	outcome string
}

func (p *staticPlugin) Info(context.Context) (plugin.Info, error) {
	return plugin.Info{Name: p.name, Version: "test"}, nil
}
func (p *staticPlugin) OpenSession(context.Context, string, map[string]string) error { return nil }
func (p *staticPlugin) Execute(_ context.Context, _ string, _ *workflow.StepNode, _ adapter.EventSink) (adapter.Result, error) {
	return adapter.Result{Outcome: p.outcome}, nil
}
func (p *staticPlugin) Permit(context.Context, string, string, bool, string) error { return nil }
func (p *staticPlugin) CloseSession(context.Context, string) error                 { return nil }
func (p *staticPlugin) Kill()                                                      {}

// staticLoader serves a fixed map of adapter name → plugin.
type staticLoader struct {
	plugins map[string]plugin.Plugin
}

func (l *staticLoader) Resolve(_ context.Context, name string) (plugin.Plugin, error) {
	p, ok := l.plugins[name]
	if !ok {
		return nil, fmt.Errorf("no plugin %q", name)
	}
	return p, nil
}
func (l *staticLoader) Shutdown(context.Context) error { return nil }

// compileForEach is a helper that compiles a for_each workflow.
func compileForEach(t *testing.T, src string) *workflow.FSMGraph {
	t.Helper()
	spec, diags := workflow.Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := workflow.Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	return g
}

const forEachWorkflow = `
workflow "t" {
  version       = "0.1"
  initial_state = "each_item"
  target_state  = "done"

  for_each "each_item" {
    items = ["apple", "banana", "cherry"]
    do    = "process"
    outcome "all_succeeded" { transition_to = "done" }
    outcome "any_failed"    { transition_to = "failed" }
  }

  step "process" {
    adapter = "noop"
    outcome "success" { transition_to = "_continue" }
  }

  state "done"   {
    terminal = true
    success  = true
  }
  state "failed" {
    terminal = true
    success  = false
  }
}
`

func TestForEachNode_IteratesAllItems(t *testing.T) {
	g := compileForEach(t, forEachWorkflow)
	vars := workflow.SeedVarsFromGraph(g)
	sink := &forEachSink{}
	loader := &staticLoader{plugins: map[string]plugin.Plugin{
		"noop": &staticPlugin{name: "noop", outcome: "success"},
	}}
	eng := engine.New(g, loader, sink, engine.WithResumedVars(vars))
	if err := eng.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	// Should have entered exactly once.
	if len(sink.entered) != 1 {
		t.Fatalf("entered events: got %d, want 1", len(sink.entered))
	}
	if sink.entered[0].node != "each_item" || sink.entered[0].count != 3 {
		t.Errorf("entered event = %+v, want {node:each_item count:3}", sink.entered[0])
	}

	// Should have 3 iteration events (indices 0, 1, 2).
	if len(sink.iterations) != 3 {
		t.Fatalf("iteration events: got %d, want 3", len(sink.iterations))
	}
	wantValues := []string{"apple", "banana", "cherry"}
	for i, ev := range sink.iterations {
		if ev.node != "each_item" || ev.index != i || ev.value != wantValues[i] {
			t.Errorf("iteration[%d] = %+v, want {node:each_item index:%d value:%q}", i, ev, i, wantValues[i])
		}
		if ev.anyFailed {
			t.Errorf("iteration[%d].anyFailed should be false for all-success run", i)
		}
	}

	// Step "process" should have run 3 times.
	processCalls := 0
	for _, s := range sink.steps {
		if s == "process" {
			processCalls++
		}
	}
	if processCalls != 3 {
		t.Errorf("process step called %d times, want 3", processCalls)
	}

	// Should emit all_succeeded outcome and transition to "done".
	if len(sink.outcomes) != 1 {
		t.Fatalf("outcome events: got %d, want 1", len(sink.outcomes))
	}
	oc := sink.outcomes[0]
	if oc.node != "each_item" || oc.outcome != "all_succeeded" || oc.target != "done" {
		t.Errorf("outcome event = %+v, want {node:each_item outcome:all_succeeded target:done}", oc)
	}
	if sink.terminal != "done" || !sink.terminalOK {
		t.Errorf("terminal = %q ok=%v, want done/true", sink.terminal, sink.terminalOK)
	}
}

func TestForEachNode_AnyFailedOnFailureOutcome(t *testing.T) {
	// The "process" step returns "failure" for the second item.
	// Both outcomes map to _continue so iteration completes.
	// Because second step result != "success", AnyFailed should flip.
	src := `
workflow "t" {
  version       = "0.1"
  initial_state = "each_item"
  target_state  = "done"

  for_each "each_item" {
    items = ["a", "b", "c"]
    do    = "process"
    outcome "all_succeeded" { transition_to = "done" }
    outcome "any_failed"    { transition_to = "failed" }
  }

  step "process" {
    adapter = "noop"
    outcome "success" { transition_to = "_continue" }
    outcome "failure" { transition_to = "_continue" }
  }

  state "done"   {
    terminal = true
    success  = true
  }
  state "failed" {
    terminal = true
    success  = false
  }
}
`
	g := compileForEach(t, src)
	vars := workflow.SeedVarsFromGraph(g)
	sink := &forEachSink{}

	callCount := 0
	loader := &staticLoader{plugins: map[string]plugin.Plugin{
		"noop": &trackingPlugin{
			name: "noop",
			fn: func() string {
				callCount++
				if callCount == 2 {
					return "failure" // second item fails
				}
				return "success"
			},
		},
	}}
	eng := engine.New(g, loader, sink, engine.WithResumedVars(vars))
	if err := eng.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	// All 3 items should be iterated.
	if len(sink.iterations) != 3 {
		t.Fatalf("iterations: got %d, want 3", len(sink.iterations))
	}
	// Third iteration should see anyFailed=true (set after second item failed).
	if !sink.iterations[2].anyFailed {
		t.Error("third iteration should see anyFailed=true after second item failure")
	}

	// Outcome should be any_failed → failed state.
	if len(sink.outcomes) != 1 {
		t.Fatalf("outcomes: got %d, want 1", len(sink.outcomes))
	}
	oc := sink.outcomes[0]
	if oc.outcome != "any_failed" || oc.target != "failed" {
		t.Errorf("outcome = %+v, want {outcome:any_failed target:failed}", oc)
	}
	if sink.terminal != "failed" {
		t.Errorf("terminal = %q, want 'failed'", sink.terminal)
	}
}

func TestForEachNode_EmptyListEndsImmediately(t *testing.T) {
	src := `
workflow "t" {
  version       = "0.1"
  initial_state = "each_item"
  target_state  = "done"

  for_each "each_item" {
    items = []
    do    = "process"
    outcome "all_succeeded" { transition_to = "done" }
  }

  step "process" {
    adapter = "noop"
    outcome "success" { transition_to = "_continue" }
  }

  state "done" {
    terminal = true
    success  = true
  }
}
`
	g := compileForEach(t, src)
	vars := workflow.SeedVarsFromGraph(g)
	sink := &forEachSink{}
	loader := &staticLoader{plugins: map[string]plugin.Plugin{
		"noop": &staticPlugin{name: "noop", outcome: "success"},
	}}
	eng := engine.New(g, loader, sink, engine.WithResumedVars(vars))
	if err := eng.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	// No iterations, no step calls.
	if len(sink.iterations) != 0 {
		t.Errorf("expected 0 iterations, got %d", len(sink.iterations))
	}
	if len(sink.steps) != 0 {
		t.Errorf("expected 0 steps, got %v", sink.steps)
	}

	// Still emits entered + outcome.
	if len(sink.entered) != 1 || sink.entered[0].count != 0 {
		t.Errorf("entered = %+v, want [{each_item 0}]", sink.entered)
	}
	if len(sink.outcomes) != 1 || sink.outcomes[0].outcome != "all_succeeded" {
		t.Errorf("outcomes = %+v, want [{each_item all_succeeded done}]", sink.outcomes)
	}
	if sink.terminal != "done" || !sink.terminalOK {
		t.Errorf("terminal = %q ok=%v, want done/true", sink.terminal, sink.terminalOK)
	}
}

// trackingPlugin calls fn() to determine the outcome for each execution.
type trackingPlugin struct {
	name string
	fn   func() string
}

func (p *trackingPlugin) Info(context.Context) (plugin.Info, error) {
	return plugin.Info{Name: p.name, Version: "test"}, nil
}
func (p *trackingPlugin) OpenSession(context.Context, string, map[string]string) error { return nil }
func (p *trackingPlugin) Execute(_ context.Context, _ string, _ *workflow.StepNode, _ adapter.EventSink) (adapter.Result, error) {
	return adapter.Result{Outcome: p.fn()}, nil
}
func (p *trackingPlugin) Permit(context.Context, string, string, bool, string) error { return nil }
func (p *trackingPlugin) CloseSession(context.Context, string) error                 { return nil }
func (p *trackingPlugin) Kill()                                                      {}

// TestForEachNode_NonContinueOutcomeAbortsIteration verifies that when a step
// inside a for_each returns an outcome whose transition_to is NOT "_continue",
// the engine aborts the loop immediately (does not process remaining items),
// emits a ForEachOutcome event, clears the cursor, and terminates at the
// designated abort target state.
func TestForEachNode_NonContinueOutcomeAbortsIteration(t *testing.T) {
	src := `
workflow "t" {
  version       = "0.1"
  initial_state = "each_item"
  target_state  = "done"

  for_each "each_item" {
    items = ["a", "b", "c"]
    do    = "process"
    outcome "all_succeeded" { transition_to = "done" }
    outcome "any_failed"    { transition_to = "rollback" }
  }

  step "process" {
    adapter = "noop"
    outcome "success" { transition_to = "_continue" }
    outcome "failure" { transition_to = "rollback" }
  }

  state "done" {
    terminal = true
    success  = true
  }
  state "rollback" {
    terminal = true
    success  = false
  }
}
`
	g := compileForEach(t, src)
	vars := workflow.SeedVarsFromGraph(g)
	sink := &forEachSink{}

	callCount := 0
	loader := &staticLoader{plugins: map[string]plugin.Plugin{
		"noop": &trackingPlugin{
			name: "noop",
			fn: func() string {
				callCount++
				if callCount == 2 {
					return "failure" // second item triggers abort
				}
				return "success"
			},
		},
	}}
	eng := engine.New(g, loader, sink, engine.WithResumedVars(vars))
	if err := eng.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	// Only 2 steps should run (first succeeds, second aborts).
	processCalls := 0
	for _, s := range sink.steps {
		if s == "process" {
			processCalls++
		}
	}
	if processCalls != 2 {
		t.Errorf("process ran %d times, want 2 (aborts after second item)", processCalls)
	}

	// Only 2 iteration events (third item is never dispatched).
	if len(sink.iterations) != 2 {
		t.Errorf("iteration events: got %d, want 2", len(sink.iterations))
	}

	// ForEachOutcome should indicate any_failed → rollback.
	if len(sink.outcomes) != 1 {
		t.Fatalf("outcome events: got %d, want 1", len(sink.outcomes))
	}
	oc := sink.outcomes[0]
	if oc.node != "each_item" || oc.outcome != "any_failed" || oc.target != "rollback" {
		t.Errorf("outcome = %+v, want {node:each_item outcome:any_failed target:rollback}", oc)
	}

	// Cursor must be cleared (empty string) after abort.
	cleared := false
	for _, v := range sink.cursorSets {
		if v == "" {
			cleared = true
		}
	}
	if !cleared {
		t.Errorf("cursor was never cleared after abort; cursorSets = %v", sink.cursorSets)
	}

	// Engine should reach "rollback", not "done".
	if sink.terminal != "rollback" {
		t.Errorf("terminal = %q, want \"rollback\"", sink.terminal)
	}
	if sink.terminalOK {
		t.Error("terminalOK should be false for rollback (success=false) state")
	}
}
