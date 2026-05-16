package engine

// while_iteration_test.go — engine-level tests for the while step modifier.
//
// Tests use the same helpers as iteration_engine_test.go: compile(), iterSink,
// fakeAdapter, multiOutcomeAdapter, captureInputAdapter, fakeLoader.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow"
)

// --- while condition false from the start ---

// TestWhile_ConditionFalseFromStart verifies that a while step with a
// statically false condition completes with all_succeeded and zero iterations.
func TestWhile_ConditionFalseFromStart(t *testing.T) {
	g := compile(t, `
workflow "t" {
  version       = "0.1"
  initial_state = "loop"
  target_state  = "done"
}
step "loop" {
  target = adapter.fake
  while  = false
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "done" }
}
state "done" {
  terminal = true
  success  = true
}`)
	sink := &iterSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"fake": &fakeAdapter{name: "fake", outcome: "success"},
	}}
	if err := NewTestEngine(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if sink.terminal != "done" || !sink.terminalOK {
		t.Errorf("terminal: %s ok=%v", sink.terminal, sink.terminalOK)
	}
	if len(sink.iterationsStarted) != 0 {
		t.Errorf("iterations started: got %d want 0", len(sink.iterationsStarted))
	}
	if len(sink.iterationsCompleted) != 1 {
		t.Fatalf("iterations completed: got %d want 1", len(sink.iterationsCompleted))
	}
	if sink.iterationsCompleted[0].outcome != "all_succeeded" {
		t.Errorf("aggregate: got %q want all_succeeded", sink.iterationsCompleted[0].outcome)
	}
}

// --- while condition driven by shared variable ---

// TestWhile_SharedVariableCountdown verifies that a while condition referencing
// a shared_variable iterates the correct number of times before the condition
// becomes false.
func TestWhile_SharedVariableCountdown(t *testing.T) {
	const src = `
workflow "t" {
  version       = "0.1"
  initial_state = "loop"
  target_state  = "done"
}

shared_variable "remaining" {
  type  = "number"
  value = 3
}

adapter "fake" "default" {}

step "loop" {
  target = adapter.fake.default
  while  = shared.remaining > 0
  outcome "success" {
    next          = "_continue"
    shared_writes = { remaining = "remaining" }
  }
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "done" }
}

state "done" {
  terminal = true
  success  = true
}
`
	spec, diags := workflow.Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %v", diags.Error())
	}
	g, diags := workflow.Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %v", diags.Error())
	}

	call := 0
	countdowns := []string{"2", "1", "0"}
	plug := &pluginFunc{fn: func(_ context.Context, _ string, _ *workflow.StepNode, _ adapter.EventSink) (adapter.Result, error) {
		out := countdowns[call]
		call++
		return adapter.Result{Outcome: "success", Outputs: map[string]string{"remaining": out}}, nil
	}}

	sink := &iterSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"fake": plug}}
	if err := NewTestEngine(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if sink.terminal != "done" {
		t.Errorf("terminal: %q", sink.terminal)
	}
	if len(sink.iterationsStarted) != 3 {
		t.Errorf("iterations started: got %d want 3", len(sink.iterationsStarted))
	}
	if len(sink.iterationsCompleted) != 1 {
		t.Fatalf("iterations completed: got %d want 1", len(sink.iterationsCompleted))
	}
	if sink.iterationsCompleted[0].outcome != "all_succeeded" {
		t.Errorf("aggregate: got %q want all_succeeded", sink.iterationsCompleted[0].outcome)
	}
}

// --- while.index in input expressions ---

// TestWhile_IndexInInput verifies that while.index is correctly bound to the
// current iteration count and is accessible in step input expressions.
func TestWhile_IndexInInput(t *testing.T) {
	const src = `
workflow "t" {
  version       = "0.1"
  initial_state = "loop"
  target_state  = "done"
}
adapter "fake" "default" {}
step "loop" {
  target = adapter.fake.default
  while  = true
  input {
    idx = while.index
  }
  outcome "success" { next = "_continue" }
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "done" }
}
state "done" {
  terminal = true
  success  = true
}
`
	spec, diags := workflow.Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %v", diags.Error())
	}
	g, diags := workflow.Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %v", diags.Error())
	}

	var capturedInputs []map[string]string
	call := 0
	outcomes := []string{"success", "success", "success"}
	plug := &pluginFunc{fn: func(_ context.Context, _ string, step *workflow.StepNode, _ adapter.EventSink) (adapter.Result, error) {
		inp := make(map[string]string)
		for k, v := range step.Input {
			inp[k] = v
		}
		capturedInputs = append(capturedInputs, inp)
		o := outcomes[call]
		if call < len(outcomes)-1 {
			call++
		}
		// On third call return all_succeeded to end the loop.
		if call == len(outcomes)-1 {
			return adapter.Result{Outcome: "all_succeeded"}, nil
		}
		return adapter.Result{Outcome: o}, nil
	}}

	sink := &iterSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"fake": plug}}
	_ = NewTestEngine(g, loader, sink).Run(context.Background())

	// Check that each captured input has the exact iteration index.
	for i, inp := range capturedInputs {
		want := fmt.Sprintf("%d", i)
		if inp["idx"] != want {
			t.Errorf("iter %d: idx = %q, want %q", i, inp["idx"], want)
		}
	}
}

// --- while.first binding ---

// TestWhile_FirstBinding verifies that while.first is true only on index 0.
func TestWhile_FirstBinding(t *testing.T) {
	const src = `
workflow "t" {
  version       = "0.1"
  initial_state = "loop"
  target_state  = "done"
}
adapter "fake" "default" {}
step "loop" {
  target = adapter.fake.default
  while  = true
  input {
    is_first = while.first
  }
  outcome "success"       { next = "_continue" }
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "done" }
}
state "done" {
  terminal = true
  success  = true
}
`
	spec, diags := workflow.Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %v", diags.Error())
	}
	g, diags := workflow.Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %v", diags.Error())
	}

	var capturedInputs []map[string]string
	n := 0
	plug := &pluginFunc{fn: func(_ context.Context, _ string, step *workflow.StepNode, _ adapter.EventSink) (adapter.Result, error) {
		inp := make(map[string]string)
		for k, v := range step.Input {
			inp[k] = v
		}
		capturedInputs = append(capturedInputs, inp)
		n++
		if n >= 3 {
			return adapter.Result{Outcome: "all_succeeded"}, nil
		}
		return adapter.Result{Outcome: "success"}, nil
	}}
	sink := &iterSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"fake": plug}}
	_ = NewTestEngine(g, loader, sink).Run(context.Background())

	if len(capturedInputs) == 0 {
		t.Fatal("no iterations ran")
	}
	if capturedInputs[0]["is_first"] != "true" {
		t.Errorf("iter 0 is_first: got %q want 'true'", capturedInputs[0]["is_first"])
	}
	if len(capturedInputs) > 1 && capturedInputs[1]["is_first"] != "false" {
		t.Errorf("iter 1 is_first: got %q want 'false'", capturedInputs[1]["is_first"])
	}
}

// --- on_failure modes ---

// TestWhile_OnFailureAbort verifies that on_failure=abort stops the loop
// after the first failing iteration and emits any_failed.
func TestWhile_OnFailureAbort(t *testing.T) {
	g := compile(t, `
workflow "t" {
  version       = "0.1"
  initial_state = "loop"
  target_state  = "done"
}
step "loop" {
  target     = adapter.fake
  while      = true
  on_failure = "abort"
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "done" }
}
state "done" {
  terminal = true
  success  = true
}`)
	sink := &iterSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		// First call fails; second would succeed but should not be reached.
		"fake": &multiOutcomeAdapter{name: "fake", outcomes: []string{"failure", "success"}},
	}}
	if err := NewTestEngine(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(sink.iterationsCompleted) != 1 {
		t.Fatalf("iterations completed: got %d want 1", len(sink.iterationsCompleted))
	}
	if sink.iterationsCompleted[0].outcome != "any_failed" {
		t.Errorf("aggregate: got %q want any_failed", sink.iterationsCompleted[0].outcome)
	}
	// Only one iteration should have started.
	if len(sink.iterationsStarted) != 1 {
		t.Errorf("iterations started: got %d want 1", len(sink.iterationsStarted))
	}
}

// TestWhile_OnFailureContinue verifies that on_failure=continue runs all
// iterations even when one fails, and emits any_failed as the aggregate.
func TestWhile_OnFailureContinue(t *testing.T) {
	// 3 iterations: fail, succeed, then the condition becomes false.
	const src = `
workflow "t" {
  version       = "0.1"
  initial_state = "loop"
  target_state  = "done"
}

shared_variable "remaining" {
  type  = "number"
  value = 2
}

adapter "fake" "default" {}

step "loop" {
  target     = adapter.fake.default
  while      = shared.remaining > 0
  on_failure = "continue"
  outcome "failure" {
    next          = "_continue"
    shared_writes = { remaining = "remaining" }
  }
  outcome "success" {
    next          = "_continue"
    shared_writes = { remaining = "remaining" }
  }
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "done" }
}

state "done" {
  terminal = true
  success  = true
}
`
	spec, diags := workflow.Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %v", diags.Error())
	}
	g, diags := workflow.Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %v", diags.Error())
	}

	call := 0
	outcomes := []struct{ o, rem string }{
		{"failure", "1"}, // iteration 0 fails but continues
		{"success", "0"}, // iteration 1 succeeds; remaining → 0
	}
	plug := &pluginFunc{fn: func(_ context.Context, _ string, _ *workflow.StepNode, _ adapter.EventSink) (adapter.Result, error) {
		r := outcomes[call]
		call++
		return adapter.Result{Outcome: r.o, Outputs: map[string]string{"remaining": r.rem}}, nil
	}}

	sink := &iterSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"fake": plug}}
	if err := NewTestEngine(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(sink.iterationsStarted) != 2 {
		t.Errorf("iterations started: got %d want 2", len(sink.iterationsStarted))
	}
	if len(sink.iterationsCompleted) != 1 {
		t.Fatalf("iterations completed: got %d want 1", len(sink.iterationsCompleted))
	}
	if sink.iterationsCompleted[0].outcome != "any_failed" {
		t.Errorf("aggregate: got %q want any_failed", sink.iterationsCompleted[0].outcome)
	}
}

// TestWhile_OnFailureIgnore verifies that on_failure=ignore causes any_failed
// iterations to be suppressed, resulting in all_succeeded aggregate.
func TestWhile_OnFailureIgnore(t *testing.T) {
	const src = `
workflow "t" {
  version       = "0.1"
  initial_state = "loop"
  target_state  = "done"
}

shared_variable "remaining" {
  type  = "number"
  value = 2
}

adapter "fake" "default" {}

step "loop" {
  target     = adapter.fake.default
  while      = shared.remaining > 0
  on_failure = "ignore"
  outcome "failure" {
    next          = "_continue"
    shared_writes = { remaining = "remaining" }
  }
  outcome "success" {
    next          = "_continue"
    shared_writes = { remaining = "remaining" }
  }
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "done" }
}

state "done" {
  terminal = true
  success  = true
}
`
	spec, diags := workflow.Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %v", diags.Error())
	}
	g, diags := workflow.Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %v", diags.Error())
	}

	call := 0
	outcomes := []struct{ o, rem string }{
		{"failure", "1"},
		{"success", "0"},
	}
	plug := &pluginFunc{fn: func(_ context.Context, _ string, _ *workflow.StepNode, _ adapter.EventSink) (adapter.Result, error) {
		r := outcomes[call]
		call++
		return adapter.Result{Outcome: r.o, Outputs: map[string]string{"remaining": r.rem}}, nil
	}}

	sink := &iterSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"fake": plug}}
	if err := NewTestEngine(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if sink.iterationsCompleted[0].outcome != "all_succeeded" {
		t.Errorf("aggregate: got %q want all_succeeded (on_failure=ignore)", sink.iterationsCompleted[0].outcome)
	}
}

// --- crash-resume ---

// TestWhile_CrashResume verifies that a while cursor is serialised to JSON and
// back, and that a resumed run picks up from the right index.
func TestWhile_CrashResume(t *testing.T) {
	const src = `
workflow "t" {
  version       = "0.1"
  initial_state = "loop"
  target_state  = "done"
}
adapter "fake" "default" {}
step "loop" {
  target = adapter.fake.default
  while  = while.index < 5
  outcome "success"       { next = "_continue" }
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "done" }
}
state "done" {
  terminal = true
  success  = true
}
`
	spec, diags := workflow.Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %v", diags.Error())
	}
	g, diags := workflow.Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %v", diags.Error())
	}

	// Simulate resume at index=3 (iterations 0,1,2 already done).
	resumeStack := []workflow.IterCursor{{
		StepName:   "loop",
		Index:      3,
		Total:      -1, // while sentinel
		InProgress: true,
	}}

	plug := &pluginFunc{fn: func(_ context.Context, _ string, _ *workflow.StepNode, _ adapter.EventSink) (adapter.Result, error) {
		return adapter.Result{Outcome: "success"}, nil
	}}

	sink := &iterSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"fake": plug}}
	eng := New(g, loader, sink, WithResumedIter(resumeStack))
	if err := eng.RunFrom(context.Background(), "loop", 1); err != nil {
		t.Fatalf("run: %v", err)
	}
	if sink.terminal != "done" {
		t.Errorf("terminal: %q", sink.terminal)
	}
	// Resumed at index 3, ran iterations 3 and 4 (condition: index < 5).
	if len(sink.iterationsStarted) != 2 {
		t.Errorf("iterations started after resume: got %d want 2", len(sink.iterationsStarted))
	}
	// Verify the first started iteration has index=3.
	if len(sink.iterationsStarted) > 0 && sink.iterationsStarted[0].index != 3 {
		t.Errorf("first resumed iteration index: got %d want 3", sink.iterationsStarted[0].index)
	}
}

// TestWhile_CursorSerialisation verifies that an IterCursor with Total=-1
// (IsWhile sentinel) round-trips through JSON correctly.
func TestWhile_CursorSerialisation(t *testing.T) {
	cur := workflow.IterCursor{
		StepName:   "myloop",
		Index:      5,
		Total:      -1,
		InProgress: true,
		AnyFailed:  true,
		OnFailure:  "continue",
	}

	data, err := workflow.SerializeIterCursor(&cur)
	if err != nil {
		t.Fatalf("serialize: %v", err)
	}

	restored, err := workflow.DeserializeIterCursor(data)
	if err != nil {
		t.Fatalf("deserialize: %v", err)
	}

	if !restored.IsWhile() {
		t.Error("restored cursor: IsWhile() should be true for Total=-1")
	}
	if restored.Index != 5 {
		t.Errorf("Index: got %d want 5", restored.Index)
	}
	if !restored.AnyFailed {
		t.Error("AnyFailed not preserved")
	}
	if restored.OnFailure != "continue" {
		t.Errorf("OnFailure: got %q want 'continue'", restored.OnFailure)
	}
}

// --- aggregate outcome routing ---

// TestWhile_AggregateRoutesToDone verifies that all_succeeded routes to the
// declared next node.
func TestWhile_AggregateRoutesToDone(t *testing.T) {
	g := compile(t, `
workflow "t" {
  version       = "0.1"
  initial_state = "loop"
  target_state  = "completed"
}
step "loop" {
  target = adapter.fake
  while  = false
  outcome "all_succeeded" { next = "completed" }
  outcome "any_failed"    { next = "failed" }
}
state "completed" {
  terminal = true
  success  = true
}
state "failed" {
  terminal = true
  success  = false
}`)
	sink := &iterSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"fake": &fakeAdapter{name: "fake", outcome: "success"},
	}}
	if err := NewTestEngine(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if sink.terminal != "completed" {
		t.Errorf("terminal: got %q want 'completed'", sink.terminal)
	}
	if !sink.terminalOK {
		t.Error("terminal success: got false want true")
	}
}

// --- routeIteratingStep skips while cursors ---

// TestWhile_RoutingSkipsWhileCursor verifies that the for_each/count routing
// loop does not intercept a while cursor and prematurely pop it.
func TestWhile_RoutingSkipsWhileCursor(t *testing.T) {
	const src = `
workflow "t" {
  version       = "0.1"
  initial_state = "loop"
  target_state  = "done"
}

shared_variable "n" {
  type  = "number"
  value = 2
}

adapter "fake" "default" {}

step "loop" {
  target = adapter.fake.default
  while  = shared.n > 0
  outcome "success" {
    next          = "_continue"
    shared_writes = { n = "n_out" }
  }
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "done" }
}

state "done" {
  terminal = true
  success  = true
}
`
	spec, diags := workflow.Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %v", diags.Error())
	}
	g, diags := workflow.Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %v", diags.Error())
	}

	call := 0
	decrements := []string{"1", "0"}
	plug := &pluginFunc{fn: func(_ context.Context, _ string, _ *workflow.StepNode, _ adapter.EventSink) (adapter.Result, error) {
		out := decrements[call]
		if call < len(decrements)-1 {
			call++
		}
		return adapter.Result{Outcome: "success", Outputs: map[string]string{"n_out": out}}, nil
	}}

	sink := &iterSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"fake": plug}}
	if err := NewTestEngine(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if sink.terminal != "done" {
		t.Errorf("terminal: %q", sink.terminal)
	}
	// Exactly 2 iterations should have run.
	if len(sink.iterationsStarted) != 2 {
		t.Errorf("iterations started: got %d want 2", len(sink.iterationsStarted))
	}
}

// --- IsWhile() sentinel ---

// TestWhile_IsWhileSentinel verifies that IsWhile returns true iff Total < 0.
func TestWhile_IsWhileSentinel(t *testing.T) {
	tests := []struct {
		total   int
		isWhile bool
	}{
		{-1, true},
		{-2, true},
		{0, false},
		{3, false},
	}
	for _, tc := range tests {
		cur := workflow.IterCursor{Total: tc.total}
		if got := cur.IsWhile(); got != tc.isWhile {
			t.Errorf("Total=%d IsWhile()=%v want %v", tc.total, got, tc.isWhile)
		}
	}
}

// TestWhile_MaxVisitsEnforced verifies that max_visits is respected across
// while iterations (each iteration counts as one visit).
func TestWhile_MaxVisitsEnforced(t *testing.T) {
	const src = `
workflow "t" {
  version       = "0.1"
  initial_state = "loop"
  target_state  = "done"
}
adapter "fake" "default" {}
step "loop" {
  target     = adapter.fake.default
  max_visits = 2
  while      = true
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "done" }
}
state "done" {
  terminal = true
  success  = true
}
`
	spec, diags := workflow.Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %v", diags.Error())
	}
	g, diags := workflow.Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %v", diags.Error())
	}

	call := 0
	plug := &pluginFunc{fn: func(_ context.Context, _ string, _ *workflow.StepNode, _ adapter.EventSink) (adapter.Result, error) {
		call++
		return adapter.Result{Outcome: "success"}, nil
	}}
	sink := &iterSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"fake": plug}}
	err := NewTestEngine(g, loader, sink).Run(context.Background())
	if err != nil {
		if !strings.Contains(err.Error(), "max_visits") {
			t.Errorf("unexpected error (want max_visits): %v", err)
		}
	} else if call > 2 {
		t.Errorf("max_visits not enforced: got %d calls, want <= 2", call)
	}
}

// --- policy.max_total_steps with while ---

// TestWhile_MaxTotalStepsEnforced verifies that while loops are bounded by the
// workflow-level policy.max_total_steps limit, which is checked at the top of
// every stepNode.Evaluate call including while re-entries.
func TestWhile_MaxTotalStepsEnforced(t *testing.T) {
	const src = `
workflow "t" {
  version       = "0.1"
  initial_state = "loop"
  target_state  = "done"
}
policy {
  max_total_steps = 3
}
adapter "fake" "default" {}
step "loop" {
  target = adapter.fake.default
  while  = true
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "done" }
}
state "done" {
  terminal = true
  success  = true
}
`
	spec, diags := workflow.Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %v", diags.Error())
	}
	g, diags := workflow.Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %v", diags.Error())
	}

	call := 0
	plug := &pluginFunc{fn: func(_ context.Context, _ string, _ *workflow.StepNode, _ adapter.EventSink) (adapter.Result, error) {
		call++
		return adapter.Result{Outcome: "success"}, nil
	}}
	sink := &iterSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"fake": plug}}
	err := NewTestEngine(g, loader, sink).Run(context.Background())
	if err == nil {
		t.Fatal("expected error for max_total_steps exceeded, got nil")
	}
	if !strings.Contains(err.Error(), "max_total_steps") {
		t.Errorf("error = %q; want mention of 'max_total_steps'", err.Error())
	}
	if call != 3 {
		t.Errorf("adapter calls: got %d want 3", call)
	}
}

// --- timeout with while ---

// TestWhile_TimeoutEnforced verifies that a step-level timeout cancels a
// blocked iteration and on_failure=abort causes the loop to terminate with
// aggregate outcome "any_failed" and Run() returning nil (not an error).
func TestWhile_TimeoutEnforced(t *testing.T) {
	const src = `
workflow "t" {
  version       = "0.1"
  initial_state = "loop"
  target_state  = "done"
}
adapter "fake" "default" {}
step "loop" {
  target     = adapter.fake.default
  while      = true
  timeout    = "1ms"
  on_failure = "abort"
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "done" }
}
state "done" {
  terminal = true
  success  = true
}
`
	spec, diags := workflow.Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %v", diags.Error())
	}
	g, diags := workflow.Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %v", diags.Error())
	}

	plug := &pluginFunc{fn: func(ctx context.Context, _ string, _ *workflow.StepNode, _ adapter.EventSink) (adapter.Result, error) {
		<-ctx.Done()
		return adapter.Result{}, ctx.Err()
	}}
	sink := &iterSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"fake": plug}}
	err := NewTestEngine(g, loader, sink).Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v (want nil: timeout should abort loop, not crash run)", err)
	}
	if sink.terminal != "done" || !sink.terminalOK {
		t.Errorf("terminal: %q ok=%v, want done/true", sink.terminal, sink.terminalOK)
	}
	if len(sink.iterationsCompleted) != 1 {
		t.Fatalf("iterations completed: got %d want 1", len(sink.iterationsCompleted))
	}
	if sink.iterationsCompleted[0].outcome != "any_failed" {
		t.Errorf("aggregate: got %q want any_failed", sink.iterationsCompleted[0].outcome)
	}
}

// TestWhile_DefaultOnFailure_ContinuesPastExecErr verifies that omitting
// on_failure (the default) causes the loop to continue past a non-fatal execErr,
// matching for_each default semantics. This is a regression test for the bug
// where the empty-string default was treated the same as "abort".
func TestWhile_DefaultOnFailure_ContinuesPastExecErr(t *testing.T) {
	const src = `
workflow "t" {
  version       = "0.1"
  initial_state = "loop"
  target_state  = "done"
}
adapter "fake" "default" {}
step "loop" {
  target = adapter.fake.default
  while  = while.index < 3
  # no on_failure — default should behave like "continue"
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "done" }
}
state "done" {
  terminal = true
  success  = true
}
`
	spec, diags := workflow.Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %v", diags.Error())
	}
	g, diags := workflow.Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %v", diags.Error())
	}

	call := 0
	// Iteration 1 returns an execErr; iterations 2 and 3 succeed.
	plug := &pluginFunc{fn: func(_ context.Context, _ string, _ *workflow.StepNode, _ adapter.EventSink) (adapter.Result, error) {
		call++
		if call == 1 {
			return adapter.Result{}, fmt.Errorf("transient error")
		}
		return adapter.Result{Outcome: "success"}, nil
	}}
	sink := &iterSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"fake": plug}}
	err := NewTestEngine(g, loader, sink).Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v (want nil: default on_failure should not abort loop)", err)
	}
	if call != 3 {
		t.Errorf("adapter calls: got %d want 3 (loop must not abort on first execErr)", call)
	}
	if sink.terminal != "done" || !sink.terminalOK {
		t.Errorf("terminal: %q ok=%v, want done/true", sink.terminal, sink.terminalOK)
	}
	if len(sink.iterationsCompleted) != 1 {
		t.Fatalf("iterations completed: got %d want 1", len(sink.iterationsCompleted))
	}
	// execErr on iteration 1 sets AnyFailed, so aggregate is any_failed.
	if sink.iterationsCompleted[0].outcome != "any_failed" {
		t.Errorf("aggregate: got %q want any_failed", sink.iterationsCompleted[0].outcome)
	}
}

// --- while + subworkflow dispatch ---

// TestWhile_Subworkflow_Success verifies that a while-modified step targeting a
// subworkflow runs the callee for each iteration until the condition becomes false.
func TestWhile_Subworkflow_Success(t *testing.T) {
	calleeBody := calleeBodyWithStep("fake")
	calleeNode := subworkflowNodeFor("sub", calleeBody)
	whileExpr := parseExpr(t, "while.index < 3")

	parentGraph := &workflow.FSMGraph{
		InitialState: "loop",
		TargetState:  "done",
		Steps: map[string]*workflow.StepNode{
			"loop": {
				Name:           "loop",
				TargetKind:     workflow.StepTargetSubworkflow,
				SubworkflowRef: "sub",
				While:          whileExpr,
				Outcomes: map[string]*workflow.CompiledOutcome{
					"all_succeeded": {Next: "done"},
					"any_failed":    {Next: "done"},
				},
			},
		},
		States:       map[string]*workflow.StateNode{"done": {Name: "done", Terminal: true, Success: true}},
		Subworkflows: map[string]*workflow.SubworkflowNode{"sub": calleeNode},
		Variables:    map[string]*workflow.VariableNode{},
		Adapters:     map[string]*workflow.AdapterNode{},
		Environments: map[string]*workflow.EnvironmentNode{},
		Policy:       workflow.DefaultPolicy,
	}

	call := 0
	plug := &pluginFunc{fn: func(_ context.Context, _ string, _ *workflow.StepNode, _ adapter.EventSink) (adapter.Result, error) {
		call++
		return adapter.Result{Outcome: "success"}, nil
	}}
	sink := &iterSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"fake": plug}}
	err := NewTestEngine(parentGraph, loader, sink).Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if sink.terminal != "done" || !sink.terminalOK {
		t.Errorf("terminal: %q ok=%v", sink.terminal, sink.terminalOK)
	}
	if call != 3 {
		t.Errorf("callee adapter calls: got %d want 3", call)
	}
	if len(sink.iterationsCompleted) != 1 {
		t.Fatalf("iterations completed: got %d want 1", len(sink.iterationsCompleted))
	}
	if sink.iterationsCompleted[0].outcome != "all_succeeded" {
		t.Errorf("aggregate: got %q want all_succeeded", sink.iterationsCompleted[0].outcome)
	}
}

// TestWhile_Subworkflow_FailureAborts verifies that when a while-modified
// subworkflow step has on_failure=abort and the callee fails, the loop
// terminates immediately with aggregate outcome "any_failed" and Run() returns nil.
func TestWhile_Subworkflow_FailureAborts(t *testing.T) {
	calleeBody := calleeBodyWithStep("fake")
	calleeNode := subworkflowNodeFor("sub", calleeBody)
	whileExpr := parseExpr(t, "true")

	parentGraph := &workflow.FSMGraph{
		InitialState: "loop",
		TargetState:  "done",
		Steps: map[string]*workflow.StepNode{
			"loop": {
				Name:           "loop",
				TargetKind:     workflow.StepTargetSubworkflow,
				SubworkflowRef: "sub",
				While:          whileExpr,
				OnFailure:      "abort",
				Outcomes: map[string]*workflow.CompiledOutcome{
					"all_succeeded": {Next: "done"},
					"any_failed":    {Next: "done"},
				},
			},
		},
		States:       map[string]*workflow.StateNode{"done": {Name: "done", Terminal: true, Success: true}},
		Subworkflows: map[string]*workflow.SubworkflowNode{"sub": calleeNode},
		Variables:    map[string]*workflow.VariableNode{},
		Adapters:     map[string]*workflow.AdapterNode{},
		Environments: map[string]*workflow.EnvironmentNode{},
		Policy:       workflow.DefaultPolicy,
	}

	call := 0
	plug := &pluginFunc{fn: func(_ context.Context, _ string, _ *workflow.StepNode, _ adapter.EventSink) (adapter.Result, error) {
		call++
		return adapter.Result{}, fmt.Errorf("callee failure")
	}}
	sink := &iterSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"fake": plug}}
	err := NewTestEngine(parentGraph, loader, sink).Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v (want nil: abort should not propagate callee error)", err)
	}
	if sink.terminal != "done" || !sink.terminalOK {
		t.Errorf("terminal: %q ok=%v", sink.terminal, sink.terminalOK)
	}
	if call != 1 {
		t.Errorf("callee adapter calls: got %d want 1 (abort after first failure)", call)
	}
	if len(sink.iterationsCompleted) != 1 {
		t.Fatalf("iterations completed: got %d want 1", len(sink.iterationsCompleted))
	}
	if sink.iterationsCompleted[0].outcome != "any_failed" {
		t.Errorf("aggregate: got %q want any_failed", sink.iterationsCompleted[0].outcome)
	}
}
