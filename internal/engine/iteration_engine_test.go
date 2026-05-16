package engine

// iteration_engine_test.go — engine-level tests for step-level for_each,
// count, on_failure semantics, and type="workflow" steps (W10).
//
// These tests use the same fakeAdapter/fakeSink/fakeLoader helpers from
// engine_test.go (same package).

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow"
)

// iterSink extends fakeSink to capture iteration events.
type iterSink struct {
	fakeSink
	iterationsStarted   []iterStartedEvent
	iterationsCompleted []iterCompletedEvent
}

type iterStartedEvent struct {
	node      string
	index     int
	value     string
	anyFailed bool
}

type iterCompletedEvent struct {
	node    string
	outcome string
	target  string
}

func (s *iterSink) OnStepIterationStarted(node string, index int, value string, anyFailed bool) {
	s.iterationsStarted = append(s.iterationsStarted, iterStartedEvent{node, index, value, anyFailed})
}

func (s *iterSink) OnStepIterationCompleted(node, outcome, target string) {
	s.iterationsCompleted = append(s.iterationsCompleted, iterCompletedEvent{node, outcome, target})
}

// multiOutcomeAdapter returns different outcomes on successive calls.
// outcomes[i] is returned on the (i+1)th call; the last entry is repeated.
type multiOutcomeAdapter struct {
	name     string
	outcomes []string
	call     int
}

func (p *multiOutcomeAdapter) Info(context.Context) (adapterhost.Info, error) {
	return adapterhost.Info{Name: p.name, Version: "test"}, nil
}
func (p *multiOutcomeAdapter) OpenSession(context.Context, string, map[string]string) error {
	return nil
}
func (p *multiOutcomeAdapter) Execute(_ context.Context, _ string, _ *workflow.StepNode, _ adapter.EventSink) (adapter.Result, error) {
	i := p.call
	if i >= len(p.outcomes) {
		i = len(p.outcomes) - 1
	}
	p.call++
	return adapter.Result{Outcome: p.outcomes[i]}, nil
}
func (p *multiOutcomeAdapter) Permit(context.Context, string, string, bool, string) error { return nil }
func (p *multiOutcomeAdapter) CloseSession(context.Context, string) error                 { return nil }
func (p *multiOutcomeAdapter) Kill()                                                      {}

// --- for_each tests ---

// TestIteration_ForEach_AllSucceeded verifies that a for_each step iterates
// all items and emits all_succeeded when all iterations return "success".
func TestIteration_ForEach_AllSucceeded(t *testing.T) {
	g := compile(t, `
workflow "t" {
  version       = "0.1"
  initial_state = "items"
  target_state  = "done"
}
step "items" {
  target = adapter.fake
  for_each = ["alpha", "beta", "gamma"]
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
	if err := New(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	if sink.terminal != "done" || !sink.terminalOK {
		t.Errorf("terminal: %s ok=%v", sink.terminal, sink.terminalOK)
	}
	if len(sink.iterationsStarted) != 3 {
		t.Errorf("iterations started: got %d want 3", len(sink.iterationsStarted))
	}
	if len(sink.iterationsCompleted) != 1 {
		t.Fatalf("iterations completed: got %d want 1", len(sink.iterationsCompleted))
	}
	if sink.iterationsCompleted[0].outcome != "all_succeeded" {
		t.Errorf("aggregate outcome: got %q want \"all_succeeded\"", sink.iterationsCompleted[0].outcome)
	}
}

// TestIteration_ForEach_AnyFailed verifies that a for_each step emits
// any_failed when at least one iteration returns a non-success outcome.
func TestIteration_ForEach_AnyFailed(t *testing.T) {
	g := compile(t, `
workflow "t" {
  version       = "0.1"
  initial_state = "items"
  target_state  = "done"
}
step "items" {
  target = adapter.fake
  for_each = ["a", "b"]
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "done" }
}
state "done" {
  terminal = true
  success  = true
}`)
	sink := &iterSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		// First call returns "failure", subsequent calls return "success".
		"fake": &multiOutcomeAdapter{name: "fake", outcomes: []string{"failure", "success"}},
	}}
	if err := New(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	if len(sink.iterationsCompleted) != 1 {
		t.Fatalf("iterations completed: got %d want 1", len(sink.iterationsCompleted))
	}
	if sink.iterationsCompleted[0].outcome != "any_failed" {
		t.Errorf("aggregate outcome: got %q want \"any_failed\"", sink.iterationsCompleted[0].outcome)
	}
}

// TestIteration_ForEach_EmptyList verifies that a for_each step with an empty
// list emits all_succeeded immediately without running any iterations.
func TestIteration_ForEach_EmptyList(t *testing.T) {
	g := compile(t, `
workflow "t" {
  version       = "0.1"
  initial_state = "items"
  target_state  = "done"
}
step "items" {
  target = adapter.fake
  for_each = []
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
	if err := New(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	if len(sink.iterationsStarted) != 0 {
		t.Errorf("expected no iterations started; got %d", len(sink.iterationsStarted))
	}
	if len(sink.iterationsCompleted) != 1 {
		t.Fatalf("expected 1 completion; got %d", len(sink.iterationsCompleted))
	}
	if sink.iterationsCompleted[0].outcome != "all_succeeded" {
		t.Errorf("aggregate outcome: got %q want \"all_succeeded\"", sink.iterationsCompleted[0].outcome)
	}
}

// TestIteration_Count_AllSucceeded verifies that a count = N step iterates N
// times and emits all_succeeded when all succeed.
func TestIteration_Count_AllSucceeded(t *testing.T) {
	g := compile(t, `
workflow "t" {
  version       = "0.1"
  initial_state = "n"
  target_state  = "done"
}
step "n" {
  target = adapter.fake
  count   = 4
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
	if err := New(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	if len(sink.iterationsStarted) != 4 {
		t.Errorf("iterations started: got %d want 4", len(sink.iterationsStarted))
	}
	if sink.iterationsCompleted[0].outcome != "all_succeeded" {
		t.Errorf("aggregate: got %q want \"all_succeeded\"", sink.iterationsCompleted[0].outcome)
	}
}

// TestIteration_OnFailure_Abort verifies that on_failure="abort" stops after
// the first failed iteration and emits any_failed.
func TestIteration_OnFailure_Abort(t *testing.T) {
	g := compile(t, `
workflow "t" {
  version       = "0.1"
  initial_state = "items"
  target_state  = "done"
}
step "items" {
  target = adapter.fake
  for_each   = ["a", "b", "c"]
  on_failure = "abort"
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "done" }
}
state "done" {
  terminal = true
  success  = true
}`)
	sink := &iterSink{}
	// First call: failure; subsequent calls: success (should not be reached).
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"fake": &multiOutcomeAdapter{name: "fake", outcomes: []string{"failure", "success", "success"}},
	}}
	if err := New(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	// Only 1 iteration should have started (abort on first failure).
	if len(sink.iterationsStarted) != 1 {
		t.Errorf("iterations started: got %d want 1 (abort)", len(sink.iterationsStarted))
	}
	if sink.iterationsCompleted[0].outcome != "any_failed" {
		t.Errorf("aggregate: got %q want \"any_failed\"", sink.iterationsCompleted[0].outcome)
	}
}

// TestIteration_OnFailure_Ignore verifies that on_failure="ignore" runs all
// iterations and always emits all_succeeded regardless of individual failures.
func TestIteration_OnFailure_Ignore(t *testing.T) {
	g := compile(t, `
workflow "t" {
  version       = "0.1"
  initial_state = "items"
  target_state  = "done"
}
step "items" {
  target = adapter.fake
  for_each   = ["a", "b", "c"]
  on_failure = "ignore"
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "done" }
}
state "done" {
  terminal = true
  success  = true
}`)
	sink := &iterSink{}
	// All calls return "failure".
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"fake": &fakeAdapter{name: "fake", outcome: "failure"},
	}}
	if err := New(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	// All 3 iterations should run.
	if len(sink.iterationsStarted) != 3 {
		t.Errorf("iterations started: got %d want 3", len(sink.iterationsStarted))
	}
	// on_failure=ignore → always all_succeeded.
	if sink.iterationsCompleted[0].outcome != "all_succeeded" {
		t.Errorf("aggregate: got %q want \"all_succeeded\"", sink.iterationsCompleted[0].outcome)
	}
}

// TestIteration_IterationFollowedByStep verifies that after a for_each step
// the engine correctly continues to the next step.
func TestIteration_IterationFollowedByStep(t *testing.T) {
	g := compile(t, `
workflow "t" {
  version       = "0.1"
  initial_state = "items"
  target_state  = "done"
}
step "items" {
  target = adapter.fake
  for_each = ["a", "b"]
  outcome "all_succeeded" { next = "post" }
  outcome "any_failed"    { next = "post" }
}
step "post" {
  target = adapter.fake
  outcome "success" { next = "done" }
}
state "done" {
  terminal = true
  success  = true
}`)
	sink := &iterSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"fake": &fakeAdapter{name: "fake", outcome: "success"},
	}}
	if err := New(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	// Should see: items×2 iterations + post step.
	if sink.terminal != "done" {
		t.Errorf("terminal: %q", sink.terminal)
	}
	// items entered twice (2 iterations), post entered once.
	itemCount := 0
	postCount := 0
	for _, s := range sink.stepsRun {
		switch s {
		case "items":
			itemCount++
		case "post":
			postCount++
		}
	}
	if itemCount != 2 {
		t.Errorf("items iterations: got %d want 2; stepsRun=%v", itemCount, sink.stepsRun)
	}
	if postCount != 1 {
		t.Errorf("post step count: got %d want 1", postCount)
	}
}

// --- type="workflow" step tests ---

// TestIteration_WorkflowStep_RunsBodyPerIteration verifies that a
// type="workflow" step executes the inline body for each iteration item.
func TestIteration_WorkflowStep_RunsBodyPerIteration(t *testing.T) {
	t.Skip("test uses removed inline workflow body feature (W13); pending W14 subworkflow invocation support")
	g := compile(t, `
workflow "t" {
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
}
step "run" {
  type     = "workflow"
  for_each = ["a", "b"]
  workflow {
    step "body" {
      target = adapter.fake
      outcome "success" { next = "_continue" }
    }
  }
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
	if err := New(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	if sink.terminal != "done" || !sink.terminalOK {
		t.Errorf("terminal: %s ok=%v", sink.terminal, sink.terminalOK)
	}
	if len(sink.iterationsStarted) != 2 {
		t.Errorf("iterations started: got %d want 2", len(sink.iterationsStarted))
	}
	if len(sink.iterationsCompleted) != 1 {
		t.Fatalf("iterations completed: got %d want 1", len(sink.iterationsCompleted))
	}
	if sink.iterationsCompleted[0].outcome != "all_succeeded" {
		t.Errorf("aggregate outcome: got %q want \"all_succeeded\"", sink.iterationsCompleted[0].outcome)
	}
}

// TestIteration_WorkflowStep_MultiStepBody verifies that a type="workflow"
// step with a multi-step body executes all body steps per iteration.
func TestIteration_WorkflowStep_MultiStepBody(t *testing.T) {
	t.Skip("test uses removed inline workflow body feature (W13); pending W14 subworkflow invocation support")
	g := compile(t, `
workflow "t" {
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
}
step "run" {
  type     = "workflow"
  for_each = ["x"]
  workflow {
    step "prepare" {
      target = adapter.fake
      outcome "success" { next = "verify" }
    }
    step "verify" {
      target = adapter.fake
      outcome "success" { next = "_continue" }
    }
  }
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "done" }
}
state "done" {
  terminal = true
  success  = true
}`)
	sink := &iterSink{}
	// Track which body steps ran.
	p := &fakeAdapter{name: "fake", outcome: "success"}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"fake": p}}
	if err := New(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	// With 1 iteration, body has 2 steps (prepare + verify).
	if sink.terminal != "done" {
		t.Errorf("terminal: %q", sink.terminal)
	}
	// Both body steps must have been entered (they share the sink stepsRun).
	if len(sink.stepsRun) < 2 {
		t.Errorf("expected at least 2 body step entries; got %d: %v", len(sink.stepsRun), sink.stepsRun)
	}
}

// TestIteration_EachBindings_ValueAndIndex verifies that each.value and
// each._idx are correctly bound during iteration.
func TestIteration_EachBindings_ValueAndIndex(t *testing.T) {
	g := compile(t, `
workflow "t" {
  version       = "0.1"
  initial_state = "items"
  target_state  = "done"
}
step "items" {
  target = adapter.fake
  for_each = ["alpha", "beta"]
  input {
    label = "v:${each.value},i:${each._idx}"
  }
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "done" }
}
state "done" {
  terminal = true
  success  = true
}`)
	// capturePlugin records input labels.
	var capturedInputs []map[string]string
	capturePlugin := &captureInputAdapter{
		outcome: "success",
		capture: &capturedInputs,
	}
	sink := &iterSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"fake": capturePlugin}}
	if err := New(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	if len(capturedInputs) != 2 {
		t.Fatalf("expected 2 captured inputs; got %d", len(capturedInputs))
	}
	if got := capturedInputs[0]["label"]; got != "v:alpha,i:0" {
		t.Errorf("first item label: got %q want %q", got, "v:alpha,i:0")
	}
	if got := capturedInputs[1]["label"]; got != "v:beta,i:1" {
		t.Errorf("second item label: got %q want %q", got, "v:beta,i:1")
	}
}

// captureInputAdapter records the Input from each Execute call.
type captureInputAdapter struct {
	outcome string
	capture *[]map[string]string
}

func (p *captureInputAdapter) Info(context.Context) (adapterhost.Info, error) {
	return adapterhost.Info{Name: "fake", Version: "test"}, nil
}
func (p *captureInputAdapter) OpenSession(context.Context, string, map[string]string) error {
	return nil
}
func (p *captureInputAdapter) Execute(_ context.Context, _ string, step *workflow.StepNode, _ adapter.EventSink) (adapter.Result, error) {
	if p.capture != nil && step != nil {
		cp := make(map[string]string, len(step.Input))
		for k, v := range step.Input {
			cp[k] = v
		}
		*p.capture = append(*p.capture, cp)
	}
	return adapter.Result{Outcome: p.outcome}, nil
}
func (p *captureInputAdapter) Permit(context.Context, string, string, bool, string) error { return nil }
func (p *captureInputAdapter) CloseSession(context.Context, string) error                 { return nil }
func (p *captureInputAdapter) Kill()                                                      {}

// TestIteration_VarScope_SerializeRestore verifies that iteration cursor state
// can be serialized and restored (simulating a crash-resume scenario).
func TestIteration_VarScope_SerializeRestore(t *testing.T) {
	stack := []workflow.IterCursor{{
		StepName:   "my_step",
		Index:      1,
		Total:      3,
		InProgress: true,
		AnyFailed:  false,
	}}
	g := &workflow.FSMGraph{Variables: map[string]*workflow.VariableNode{}}
	vars := workflow.SeedVarsFromGraph(g)

	scopeJSON, err := workflow.SerializeVarScope(vars, stack)
	if err != nil {
		t.Fatalf("serialize: %v", err)
	}

	_, restored, err := workflow.RestoreVarScope(scopeJSON, g)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if len(restored) != 1 {
		t.Fatalf("restored stack len: got %d want 1", len(restored))
	}
	if restored[0].StepName != "my_step" {
		t.Errorf("StepName: got %q want %q", restored[0].StepName, "my_step")
	}
	if restored[0].Index != 1 {
		t.Errorf("Index: got %d want 1", restored[0].Index)
	}
	if !restored[0].InProgress {
		t.Error("InProgress: got false want true")
	}
}

// TestIteration_WithResumedIter verifies that the engine can be seeded with a
// pre-existing IterStack (simulating resume after crash mid-iteration).
func TestIteration_WithResumedIter(t *testing.T) {
	g := compile(t, `
workflow "t" {
  version       = "0.1"
  initial_state = "items"
  target_state  = "done"
}
step "items" {
  target = adapter.fake
  for_each = ["a", "b", "c"]
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

	// Simulate resume at index 1 (second item), with items pre-loaded.
	resumeStack := []workflow.IterCursor{{
		StepName:   "items",
		Index:      1,
		Total:      3,
		InProgress: true,
		AnyFailed:  false,
	}}

	eng := New(g, loader, sink,
		WithResumedIter(resumeStack),
	)
	// RunFrom with resume at "items" step, attempt 1.
	if err := eng.RunFrom(context.Background(), "items", 1); err != nil {
		t.Fatalf("run: %v", err)
	}

	if sink.terminal != "done" {
		t.Errorf("terminal: %q", sink.terminal)
	}
}

// TestIteration_RunState_PushPopCursor verifies the IterStack push/pop logic.
func TestIteration_RunState_PushPopCursor(t *testing.T) {
	st := &RunState{}

	if st.TopCursor() != nil {
		t.Error("expected nil cursor on empty stack")
	}

	c1 := workflow.IterCursor{StepName: "step1", Index: 0, InProgress: true}
	st.PushCursor(&c1)
	if top := st.TopCursor(); top == nil || top.StepName != "step1" {
		t.Errorf("after push: top=%v", top)
	}

	c2 := workflow.IterCursor{StepName: "step2", Index: 0, InProgress: true}
	st.PushCursor(&c2)
	if top := st.TopCursor(); top == nil || top.StepName != "step2" {
		t.Errorf("after second push: top=%v", top)
	}

	popped := st.PopCursor()
	if popped.StepName != "step2" {
		t.Errorf("popped: got %q want %q", popped.StepName, "step2")
	}
	if top := st.TopCursor(); top == nil || top.StepName != "step1" {
		t.Errorf("after pop: top=%v", top)
	}

	popped = st.PopCursor()
	if popped.StepName != "step1" {
		t.Errorf("popped second: got %q want %q", popped.StepName, "step1")
	}
	if st.TopCursor() != nil {
		t.Error("expected nil cursor after all pops")
	}
}

// TestIteration_RunState_PopEmpty verifies that popping an empty stack returns
// a zero-value cursor without panicking.
func TestIteration_RunState_PopEmpty(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("PopCursor on empty stack panicked: %v", r)
		}
	}()
	st := &RunState{}
	_ = st.PopCursor()
}

// Unused import guard — time is imported by the package via other test files.
var _ = time.Second

// --- B-12: New engine tests ---

// captureOutputAdapter is an adapter that captures input labels and returns
// per-call outputs, enabling tests that need both input inspection and output
// propagation in a single step.
type captureOutputAdapter struct {
	outcomes []string
	outputs  []map[string]string
	capture  *[]map[string]string
	call     int
}

func (p *captureOutputAdapter) Info(context.Context) (adapterhost.Info, error) {
	return adapterhost.Info{Name: "fake", Version: "test"}, nil
}
func (p *captureOutputAdapter) OpenSession(context.Context, string, map[string]string) error {
	return nil
}
func (p *captureOutputAdapter) Execute(_ context.Context, _ string, step *workflow.StepNode, _ adapter.EventSink) (adapter.Result, error) {
	i := p.call
	if i >= len(p.outcomes) {
		i = len(p.outcomes) - 1
	}
	if p.capture != nil && step != nil {
		cp := make(map[string]string, len(step.Input))
		for k, v := range step.Input {
			cp[k] = v
		}
		*p.capture = append(*p.capture, cp)
	}
	var outs map[string]string
	if i < len(p.outputs) {
		outs = p.outputs[i]
	}
	p.call++
	return adapter.Result{Outcome: p.outcomes[i], Outputs: outs}, nil
}
func (p *captureOutputAdapter) Permit(context.Context, string, string, bool, string) error {
	return nil
}
func (p *captureOutputAdapter) CloseSession(context.Context, string) error { return nil }
func (p *captureOutputAdapter) Kill()                                      {}

// TestIter_MapForEach_KeyAndTotal verifies that for_each over an HCL object map
// binds each.key to the map key string and each._total to the map size.
func TestIter_MapForEach_KeyAndTotal(t *testing.T) {
	g := compile(t, `
workflow "t" {
  version       = "0.1"
  initial_state = "items"
  target_state  = "done"
}
step "items" {
  target = adapter.fake
  for_each = { alpha = "A", beta = "B" }
  input {
    label = "k:${each.key},t:${each._total}"
  }
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "done" }
}
state "done" {
  terminal = true
  success  = true
}`)
	var capturedInputs []map[string]string
	cp := &captureInputAdapter{outcome: "success", capture: &capturedInputs}
	sink := &iterSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"fake": cp}}
	if err := New(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	if len(capturedInputs) != 2 {
		t.Fatalf("expected 2 captured inputs; got %d", len(capturedInputs))
	}
	// Both iterations must see _total=2 and a valid map key in each.key.
	for i, inp := range capturedInputs {
		got := inp["label"]
		if got != "k:alpha,t:2" && got != "k:beta,t:2" {
			t.Errorf("input[%d] label %q: expected k:alpha,t:2 or k:beta,t:2", i, got)
		}
	}
}

// TestIter_Prev_NullOnFirst_ObjectAfter verifies that each._prev is null for
// the first iteration and non-null for subsequent iterations (it holds the
// previous iteration's output object).
func TestIter_Prev_NullOnFirst_ObjectAfter(t *testing.T) {
	g := compile(t, `
workflow "t" {
  version       = "0.1"
  initial_state = "items"
  target_state  = "done"
}
step "items" {
  target = adapter.fake
  for_each = ["a", "b"]
  input {
    label = "${each.value},prevnull:${each._prev == null}"
  }
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "done" }
}
state "done" {
  terminal = true
  success  = true
}`)
	var capturedInputs []map[string]string
	combined := &captureOutputAdapter{
		outcomes: []string{"success", "success"},
		outputs:  []map[string]string{{"result": "first_out"}, nil},
		capture:  &capturedInputs,
	}
	sink := &iterSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"fake": combined}}
	if err := New(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	if len(capturedInputs) != 2 {
		t.Fatalf("expected 2 captured inputs; got %d", len(capturedInputs))
	}
	// First iteration: _prev is null → prevnull:true
	if got := capturedInputs[0]["label"]; got != "a,prevnull:true" {
		t.Errorf("iter 0 label: got %q want %q", got, "a,prevnull:true")
	}
	// Second iteration: _prev is the output from iter 0 (non-null) → prevnull:false
	if got := capturedInputs[1]["label"]; got != "b,prevnull:false" {
		t.Errorf("iter 1 label: got %q want %q", got, "b,prevnull:false")
	}
}

// TestIter_OnFailure_Continue_AggregatesAnyFailed verifies that on_failure="continue"
// runs all iterations even when one fails, then emits any_failed aggregate.
func TestIter_OnFailure_Continue_AggregatesAnyFailed(t *testing.T) {
	g := compile(t, `
workflow "t" {
  version       = "0.1"
  initial_state = "items"
  target_state  = "done"
}
step "items" {
  target = adapter.fake
  for_each   = ["a", "b", "c"]
  on_failure = "continue"
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "done" }
}
state "done" {
  terminal = true
  success  = true
}`)
	// Second iteration fails; others succeed.
	plug := &multiOutcomeAdapter{name: "fake", outcomes: []string{"success", "failure", "success"}}
	sink := &iterSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"fake": plug}}
	if err := New(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	if len(sink.iterationsCompleted) != 1 {
		t.Fatalf("expected 1 completion; got %d", len(sink.iterationsCompleted))
	}
	// All 3 iterations ran (continue); aggregate is any_failed.
	if sink.iterationsCompleted[0].outcome != "any_failed" {
		t.Errorf("aggregate: got %q want any_failed", sink.iterationsCompleted[0].outcome)
	}
	if len(sink.iterationsStarted) != 3 {
		t.Errorf("started: got %d want 3", len(sink.iterationsStarted))
	}
}

// TestIter_OnFailure_Abort_StopsAfterFirstFailure verifies that on_failure="abort"
// stops iteration immediately after the first failing iteration.
func TestIter_OnFailure_Abort_StopsAfterFirstFailure(t *testing.T) {
	g := compile(t, `
workflow "t" {
  version       = "0.1"
  initial_state = "items"
  target_state  = "done"
}
step "items" {
  target = adapter.fake
  for_each   = ["a", "b", "c"]
  on_failure = "abort"
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "done" }
}
state "done" {
  terminal = true
  success  = true
}`)
	plug := &multiOutcomeAdapter{name: "fake", outcomes: []string{"failure", "success"}}
	sink := &iterSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"fake": plug}}
	if err := New(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	// Abort on first failure: only 1 iteration ran.
	if len(sink.iterationsStarted) != 1 {
		t.Errorf("started: got %d want 1", len(sink.iterationsStarted))
	}
	if sink.iterationsCompleted[0].outcome != "any_failed" {
		t.Errorf("aggregate: got %q want any_failed", sink.iterationsCompleted[0].outcome)
	}
}

// perIterSink extends fakeSink to capture OnStepOutputCaptured calls in order,
// enabling per-iteration output verification.
type perIterSink struct {
	fakeSink
	stepOutputs []map[string]string // per-call outputs in order
	stepNames   []string            // matching step names
}

func (s *perIterSink) OnStepOutputCaptured(step string, outputs map[string]string) {
	cp := make(map[string]string, len(outputs))
	for k, v := range outputs {
		cp[k] = v
	}
	s.stepNames = append(s.stepNames, step)
	s.stepOutputs = append(s.stepOutputs, cp)
}

// TestIter_IndexedOutputs_StoredInStepsVar verifies that adapter step outputs
// are emitted per-iteration via OnStepOutputCaptured so downstream steps can
// reference them. The test verifies that both iterations of a for_each emit
// the correct output key-value pairs in order.
func TestIter_IndexedOutputs_StoredInStepsVar(t *testing.T) {
	g := compile(t, `
workflow "t" {
  version       = "0.1"
  initial_state = "produce"
  target_state  = "done"
}
step "produce" {
  target = adapter.fake_produce
  for_each = ["x", "y"]
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "done" }
}
state "done" {
  terminal = true
  success  = true
}`)
	producePlug := &captureOutputAdapter{
		outcomes: []string{"success", "success"},
		outputs:  []map[string]string{{"val": "result_x"}, {"val": "result_y"}},
	}
	sink := &perIterSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"fake_produce": producePlug,
	}}
	if err := New(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	// Both iterations should emit OnStepOutputCaptured for "produce".
	if len(sink.stepOutputs) != 2 {
		t.Fatalf("OnStepOutputCaptured called %d times; want 2", len(sink.stepOutputs))
	}
	if got := sink.stepOutputs[0]["val"]; got != "result_x" {
		t.Errorf("iter 0 output val: got %q want %q", got, "result_x")
	}
	if got := sink.stepOutputs[1]["val"]; got != "result_y" {
		t.Errorf("iter 1 output val: got %q want %q", got, "result_y")
	}
}

// TestIter_CrashResume_RebindEach verifies that a resumed-from-crash
// iteration re-evaluates the for_each expression and re-binds each.* so the
// step input reflects the correct iteration item.
func TestIter_CrashResume_RebindEach(t *testing.T) {
	g := compile(t, `
workflow "t" {
  version       = "0.1"
  initial_state = "items"
  target_state  = "done"
}
step "items" {
  target = adapter.fake
  for_each = ["a", "b", "c"]
  input {
    label = "${each.value}"
  }
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "done" }
}
state "done" {
  terminal = true
  success  = true
}`)
	var capturedInputs []map[string]string
	capturePlugin := &captureInputAdapter{outcome: "success", capture: &capturedInputs}
	sink := &iterSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"fake": capturePlugin}}

	// Resume at index=1 with no Items (crash path: Items is nil).
	resumeStack := []workflow.IterCursor{{
		StepName:   "items",
		Index:      1,
		Total:      3,
		InProgress: true,
	}}
	eng := New(g, loader, sink, WithResumedIter(resumeStack))
	if err := eng.RunFrom(context.Background(), "items", 1); err != nil {
		t.Fatalf("run: %v", err)
	}

	// Should complete iterations 1 and 2 (index 1, 2 — "b" and "c").
	if len(capturedInputs) != 2 {
		t.Fatalf("expected 2 iterations from resume; got %d", len(capturedInputs))
	}
	if got := capturedInputs[0]["label"]; got != "b" {
		t.Errorf("resume iter 1 label: got %q want %q", got, "b")
	}
	if got := capturedInputs[1]["label"]; got != "c" {
		t.Errorf("resume iter 2 label: got %q want %q", got, "c")
	}
}

// TestIter_NestedIteration_WorkflowBody verifies that a type="workflow" step
// with for_each correctly executes the body once per iteration item, binding
// each.value inside the body.
func TestIter_NestedIteration_WorkflowBody(t *testing.T) {
	t.Skip("test uses removed inline workflow body feature (W13); pending W14 subworkflow invocation support")
	g := compile(t, `
workflow "t" {
  version       = "0.1"
  initial_state = "outer"
  target_state  = "done"
}
step "outer" {
  type     = "workflow"
  for_each = ["x", "y"]
  workflow {
    step "inner" {
      target = adapter.fake
      input   { label = "${each.value}" }
      outcome "success" { next = "_continue" }
    }
  }
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "done" }
}
state "done" {
  terminal = true
  success  = true
}`)
	var capturedInputs []map[string]string
	capturePlugin := &captureInputAdapter{outcome: "success", capture: &capturedInputs}
	sink := &iterSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"fake": capturePlugin}}
	if err := New(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	if sink.terminal != "done" || !sink.terminalOK {
		t.Errorf("terminal: %s ok=%v", sink.terminal, sink.terminalOK)
	}
	if len(sink.iterationsStarted) != 2 {
		t.Errorf("iterations started: got %d want 2", len(sink.iterationsStarted))
	}
	// Each body iteration runs "inner" with each.value bound to the outer item.
	if len(capturedInputs) != 2 {
		t.Fatalf("captured inputs: got %d want 2", len(capturedInputs))
	}
	if got := capturedInputs[0]["label"]; got != "x" {
		t.Errorf("iter 0 inner label: got %q want %q", got, "x")
	}
	if got := capturedInputs[1]["label"]; got != "y" {
		t.Errorf("iter 1 inner label: got %q want %q", got, "y")
	}
}

// TestIter_EarlyExit_OutsideBody_TerminatesLoop verifies that when a
// type="workflow" body step transitions to a non-_continue terminal state
// (early-exit path) with on_failure="abort", the iteration loop terminates
// after that iteration rather than continuing to the next item.
func TestIter_EarlyExit_OutsideBody_TerminatesLoop(t *testing.T) {
	t.Skip("test uses removed inline workflow body feature (W13); pending W14 subworkflow invocation support")
	g := compile(t, `
workflow "t" {
  version       = "0.1"
  initial_state = "outer"
  target_state  = "done"
}

step "outer" {
  type       = "workflow"
  on_failure = "abort"
  for_each   = ["x", "y", "z"]
  workflow {
    step "body" {
      target = adapter.fake
      input   { label = "${each.value}" }
      outcome "success" { next = "_continue" }
      outcome "failure" { next = "aborted" }
    }
    state "aborted" {
      terminal = true
      success  = false
    }
  }
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "done" }
}

state "done" {
  terminal = true
  success  = true
}`)
	// Return success for first item, failure for second — iteration must stop
	// after the second item because on_failure="abort".
	var capturedInputs []map[string]string
	mp := &multiOutcomeAdapter{name: "fake", outcomes: []string{"success", "failure", "success"}}
	cp := &captureInputAdapter{outcome: "", capture: &capturedInputs}
	// Wire both: mp for outcome routing, cp for input capture.
	combined := &combinedAdapter{captureInputAdapter: cp, outcomeAdapter: mp}
	sink := &iterSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"fake": combined}}
	if err := New(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	// Only 2 iterations should execute (x succeeds, y fails+aborts loop).
	if len(capturedInputs) != 2 {
		t.Errorf("captured inputs: got %d, want 2 (abort after first failure)", len(capturedInputs))
	}
	if len(sink.iterationsCompleted) != 1 {
		t.Fatalf("iterations completed: got %d, want 1", len(sink.iterationsCompleted))
	}
	if sink.iterationsCompleted[0].outcome != "any_failed" {
		t.Errorf("aggregate outcome: got %q want %q", sink.iterationsCompleted[0].outcome, "any_failed")
	}
}

// combinedAdapter wires a captureInputAdapter for input recording and a
// multiOutcomeAdapter for outcome routing.
type combinedAdapter struct {
	*captureInputAdapter
	outcomeAdapter *multiOutcomeAdapter
}

func (c *combinedAdapter) Execute(ctx context.Context, runID string, step *workflow.StepNode, sink adapter.EventSink) (adapter.Result, error) {
	// Record input via captureInputAdapter.
	if c.captureInputAdapter.capture != nil && step != nil {
		cp := make(map[string]string, len(step.Input))
		for k, v := range step.Input {
			cp[k] = v
		}
		*c.captureInputAdapter.capture = append(*c.captureInputAdapter.capture, cp)
	}
	// Outcome from multiOutcomeAdapter.
	return c.outcomeAdapter.Execute(ctx, runID, step, sink)
}

// TestIter_OutputBlocks_OnlyDeclaredVisible verifies that a type="workflow"
// step with output { } blocks makes declared values available to downstream
// steps via steps.<name>[idx].<key>, and that the output block is evaluated
// against the body's final variable state.
func TestIter_OutputBlocks_OnlyDeclaredVisible(t *testing.T) {
	t.Skip("test uses removed inline workflow body feature (W13); pending W14 subworkflow invocation support")
	g := compile(t, `
workflow "t" {
  version       = "0.1"
  initial_state = "produce"
  target_state  = "done"
}

step "produce" {
  type     = "workflow"
  for_each = ["item"]
  workflow {
    output "score" {
      value = "42"
    }
    step "body" {
      target = adapter.fake
      outcome "success" { next = "_continue" }
    }
  }
  outcome "all_succeeded" { next = "consume" }
  outcome "any_failed"    { next = "done" }
}

step "consume" {
  target = adapter.fake
  input {
    got = "${steps.produce[0].score}"
  }
  outcome "success" { next = "done" }
}

state "done" {
  terminal = true
  success  = true
}`)
	var capturedConsume []map[string]string
	sink := &iterSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"fake": &captureInputAdapter{outcome: "success", capture: &capturedConsume},
	}}
	if err := New(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	// consume step runs once (not iterating), so we expect exactly 1 capture
	// for the consume step (the body step also calls Execute, so total >= 2).
	// Find the consume call (has "got" key).
	var consumeInput map[string]string
	for _, inp := range capturedConsume {
		if _, ok := inp["got"]; ok {
			consumeInput = inp
			break
		}
	}
	if consumeInput == nil {
		t.Fatal("consume step never executed or did not capture 'got' input")
	}
	if got := consumeInput["got"]; got != "42" {
		t.Errorf("consume 'got': want %q, got %q", "42", got)
	}
}

// TestIter_OutputBlocks_NoneDeclared_AdapterStep verifies the plan Step 8 requirement:
// an adapter step with for_each (no output {} block) accumulates adapter response
// outputs under steps.<name>[idx].<key>, and a downstream step can resolve
// steps.<name>[0].<key> through the cty expression evaluator. This test would fail
// if WithIndexedStepOutput stored values under a different key format or if the
// expression evaluator could not resolve numeric-indexed adapter outputs.
func TestIter_OutputBlocks_NoneDeclared_AdapterStep(t *testing.T) {
	g := compile(t, `
workflow "t" {
  version       = "0.1"
  initial_state = "produce"
  target_state  = "done"
}

step "produce" {
  target = adapter.fake_produce
  for_each = ["x", "y"]
  outcome "all_succeeded" { next = "consume" }
  outcome "any_failed"    { next = "done" }
}

step "consume" {
  target = adapter.fake_consume
  input {
    first_val  = "${steps.produce[0].val}"
    second_val = "${steps.produce[1].val}"
  }
  outcome "success" { next = "done" }
}

state "done" {
  terminal = true
  success  = true
}`)
	producePlug := &captureOutputAdapter{
		outcomes: []string{"success", "success"},
		outputs:  []map[string]string{{"val": "result_x"}, {"val": "result_y"}},
	}
	var consumeInputs []map[string]string
	consumePlug := &captureInputAdapter{outcome: "success", capture: &consumeInputs}
	sink := &iterSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"fake_produce": producePlug,
		"fake_consume": consumePlug,
	}}
	if err := New(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	if len(consumeInputs) != 1 {
		t.Fatalf("consume ran %d times; want 1", len(consumeInputs))
	}
	inp := consumeInputs[0]
	if got := inp["first_val"]; got != "result_x" {
		t.Errorf("first_val: got %q want %q", got, "result_x")
	}
	if got := inp["second_val"]; got != "result_y" {
		t.Errorf("second_val: got %q want %q", got, "result_y")
	}
}

// TestIter_Prev_PopulatedAfterFailedIterationContinue verifies that under
// on_failure="continue", each._prev on iteration N+1 contains the step outputs
// from iteration N even when iteration N failed. This is the plan Risks section
// guarantee: authors building accumulation patterns can rely on _prev being
// non-null after any completed iteration, regardless of its outcome.
func TestIter_Prev_PopulatedAfterFailedIterationContinue(t *testing.T) {
	g := compile(t, `
workflow "t" {
  version       = "0.1"
  initial_state = "items"
  target_state  = "done"
}

step "items" {
  target = adapter.fake
  for_each   = ["a", "b"]
  on_failure = "continue"
  input {
    label      = "${each.value}"
    prev_null  = "${each._prev == null}"
  }
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "done" }
}

state "done" {
  terminal = true
  success  = true
}`)
	// Iteration 0 fails but returns outputs; iteration 1 should see _prev from iter 0.
	combined := &captureOutputAdapter{
		outcomes: []string{"failure", "success"},
		outputs:  []map[string]string{{"result": "fail_out"}, nil},
	}
	var capturedInputs []map[string]string
	combined.capture = &capturedInputs
	sink := &iterSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"fake": combined}}
	if err := New(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	if len(capturedInputs) != 2 {
		t.Fatalf("expected 2 captured inputs; got %d", len(capturedInputs))
	}
	// Iteration 0: _prev is null (first iteration).
	if got := capturedInputs[0]["prev_null"]; got != "true" {
		t.Errorf("iter 0 prev_null: got %q want %q", got, "true")
	}
	// Iteration 1: _prev is from iter 0's adapter outputs (even though iter 0 failed).
	if got := capturedInputs[1]["prev_null"]; got != "false" {
		t.Errorf("iter 1 prev_null: got %q want %q (expected _prev populated after failed iter 0)", got, "false")
	}
}

// whose body contains its own for_each step pushes two cursors onto the
// RunState.IterStack (outer for_each + inner for_each), producing the correct
// number of inner-step executions.
func TestIter_NestedIteration_CursorStack(t *testing.T) {
	t.Skip("test uses removed inline workflow body feature (W13); pending W14 subworkflow invocation support")
	g := compile(t, `
workflow "t" {
  version       = "0.1"
  initial_state = "outer"
  target_state  = "done"
}

step "outer" {
  type     = "workflow"
  for_each = ["a", "b"]
  workflow {
    step "inner" {
      target = adapter.fake
      for_each = ["x", "y"]
      input    { label = "${each.value}" }
      outcome "all_succeeded" { next = "_continue" }
      outcome "any_failed"    { next = "_continue" }
    }
  }
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "done" }
}

state "done" {
  terminal = true
  success  = true
}`)
	var capturedInputs []map[string]string
	cp := &captureInputAdapter{outcome: "success", capture: &capturedInputs}
	sink := &iterSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"fake": cp}}
	if err := New(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	// 2 outer iterations × 2 inner iterations = 4 inner step executions.
	if len(capturedInputs) != 4 {
		t.Fatalf("inner step executions: got %d, want 4 (2 outer × 2 inner)", len(capturedInputs))
	}
	// 4 inner-iteration started events (2 per outer iteration) + 2 outer ones.
	// At minimum the inner step must have produced 4 starts.
	innerStarts := 0
	for _, ev := range sink.iterationsStarted {
		if ev.node == "inner" {
			innerStarts++
		}
	}
	if innerStarts != 4 {
		t.Errorf("inner iteration started events: got %d, want 4", innerStarts)
	}
}

// TestIter_CrashResume_PrevRestoredFromJSON verifies that IterCursor.Prev
// survives a serialize → deserialize round-trip so that each._prev is non-null
// on the resumed iteration (B-14 / B-15 acceptance criterion). The test:
//  1. Builds a cursor with Prev set to a real step-output object.
//  2. Serializes the cursor via SerializeIterCursor.
//  3. Resumes the engine using WithResumedIter with the deserialized cursor.
//  4. Asserts that the resumed step receives each._prev == "first_out" (not null).
func TestIter_CrashResume_PrevRestoredFromJSON(t *testing.T) {
	g := compile(t, `
workflow "t" {
  version       = "0.1"
  initial_state = "items"
  target_state  = "done"
}
step "items" {
  target = adapter.fake
  for_each = ["a", "b"]
  input {
    prev_null = "${each._prev == null}"
  }
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "done" }
}
state "done" {
  terminal = true
  success  = true
}`)

	// Build a cursor for the second iteration (index=1) with Prev set to a
	// non-nil cty object simulating the first iteration's adapter output.
	prevObj := cty.ObjectVal(map[string]cty.Value{
		"result": cty.StringVal("first_out"),
	})
	cursor := &workflow.IterCursor{
		StepName:   "items",
		Index:      1,
		Total:      2,
		InProgress: true,
		Prev:       prevObj,
	}

	// Serialize and deserialize the cursor to validate the round-trip (B-14).
	data, err := workflow.SerializeIterCursor(cursor)
	if err != nil {
		t.Fatalf("serialize: %v", err)
	}
	restored, err := workflow.DeserializeIterCursor(data)
	if err != nil {
		t.Fatalf("deserialize: %v", err)
	}
	if restored.Prev == cty.NilVal {
		t.Fatal("deserialized Prev is cty.NilVal; B-14 fix not effective")
	}

	// Resume the engine with the deserialized cursor (Items intentionally nil).
	var capturedInputs []map[string]string
	capturePlugin := &captureInputAdapter{outcome: "success", capture: &capturedInputs}
	sink := &iterSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"fake": capturePlugin}}

	eng := New(g, loader, sink, WithResumedIter([]workflow.IterCursor{*restored}))
	if err := eng.RunFrom(context.Background(), "items", 1); err != nil {
		t.Fatalf("run: %v", err)
	}

	// Only iteration 1 (index=1) should execute (resume from mid-run).
	if len(capturedInputs) != 1 {
		t.Fatalf("expected 1 resumed iteration; got %d", len(capturedInputs))
	}
	// each._prev must be non-null: prev_null should be "false".
	if got := capturedInputs[0]["prev_null"]; got != "false" {
		t.Errorf("each._prev on resume: prev_null=%q, want \"false\" (B-15)", got)
	}
}

// TestIter_WorkflowBody_EarlyExit_StopsLoop verifies that when a type="workflow"
// step body reaches a terminal state other than "_continue", the entire iteration
// loop stops immediately (early-exit semantics) rather than advancing to the
// next iteration.
func TestIter_WorkflowBody_EarlyExit_StopsLoop(t *testing.T) {
	t.Skip("test uses removed inline workflow body feature (W13); pending W14 subworkflow invocation support")
	n := 0
	seqPlugin := &callbackAdapter{fn: func(_ map[string]string) (string, map[string]string) {
		n++
		if n == 1 {
			return "success", nil
		}
		return "failure", nil
	}}
	g := compile(t, `
workflow "t" {
  version       = "0.1"
  initial_state = "loop"
  target_state  = "done"
}
step "loop" {
  type     = "workflow"
  for_each = ["a", "b", "c"]
  workflow {
    step "body" {
      target = adapter.seq
      outcome "success" { next = "_continue" }
      outcome "failure" { next = "bail" }
    }
    state "bail" { terminal = true }
  }
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "done" }
}
state "done" {
  terminal = true
  success  = true
}`)
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"seq": seqPlugin}}
	eng := New(g, loader, &fakeSink{})
	if err := eng.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	// Only 2 iterations should have run: iteration 0 (success → _continue)
	// and iteration 1 (failure → bail → early-exit). Iteration 2 must not run.
	if n != 2 {
		t.Errorf("body executed %d times; want 2 (early-exit after second iteration)", n)
	}
}

// TestIter_MapForEach_UsesKeyForIndexedOutput verifies that map-based for_each
// populates steps.<name>["key"] rather than steps.<name>[0].
func TestIter_MapForEach_UsesKeyForIndexedOutput(t *testing.T) {
	outPlugin := &outputAdapter{outcome: "success", outputs: map[string]string{"val": "out"}}
	g := compile(t, `
workflow "t" {
  version       = "0.1"
  initial_state = "produce"
  target_state  = "consume"
}
step "produce" {
  target = adapter.out
  for_each = { alpha = "a", beta = "b" }
  outcome "all_succeeded" { next = "consume" }
  outcome "any_failed"    { next = "consume" }
}
step "consume" {
  target = adapter.capture
  input {
    got_alpha = "${steps.produce.alpha.val}"
  }
  outcome "success" { next = "done" }
}
state "done" {
  terminal = true
  success  = true
}`)
	var capturedInputs []map[string]string
	capturePlugin := &captureInputAdapter{outcome: "success", capture: &capturedInputs}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"out":     outPlugin,
		"capture": capturePlugin,
	}}
	eng := New(g, loader, &fakeSink{})
	if err := eng.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(capturedInputs) == 0 {
		t.Fatal("consume step never ran")
	}
	if got := capturedInputs[0]["got_alpha"]; got != "out" {
		t.Errorf("steps.produce[\"alpha\"].val = %q; want %q", got, "out")
	}
}

// TestIter_Prev_NonStringValues_RoundTrip verifies that non-string cty values
// (e.g. numbers) in Prev survive the serialize/deserialize round-trip without
// being silently dropped.
func TestIter_Prev_NonStringValues_RoundTrip(t *testing.T) {
	prevObj := cty.ObjectVal(map[string]cty.Value{
		"label": cty.StringVal("hello"),
		"count": cty.NumberIntVal(42),
	})
	cursor := &workflow.IterCursor{
		StepName:   "step",
		Index:      1,
		Total:      3,
		InProgress: true,
		Prev:       prevObj,
	}
	data, err := workflow.SerializeIterCursor(cursor)
	if err != nil {
		t.Fatalf("serialize: %v", err)
	}
	restored, err := workflow.DeserializeIterCursor(data)
	if err != nil {
		t.Fatalf("deserialize: %v", err)
	}
	if restored.Prev == cty.NilVal {
		t.Fatal("deserialized Prev is cty.NilVal")
	}
	// Both the string and number attributes must be faithfully restored.
	if v := restored.Prev.GetAttr("label"); v.AsString() != "hello" {
		t.Errorf("label = %q; want %q", v.AsString(), "hello")
	}
	countVal, _ := restored.Prev.GetAttr("count").AsBigFloat().Int64()
	if countVal != 42 {
		t.Errorf("count = %d; want 42", countVal)
	}
}

// callbackAdapter is a test adapter whose Execute outcome is determined by an
// arbitrary function, letting tests control per-call behavior.
type callbackAdapter struct {
	fn func(map[string]string) (string, map[string]string)
}

func (p *callbackAdapter) Info(context.Context) (adapterhost.Info, error) {
	return adapterhost.Info{Name: "callback", Version: "test"}, nil
}
func (p *callbackAdapter) OpenSession(context.Context, string, map[string]string) error { return nil }
func (p *callbackAdapter) Execute(_ context.Context, _ string, step *workflow.StepNode, _ adapter.EventSink) (adapter.Result, error) {
	outcome, outputs := p.fn(step.Input)
	return adapter.Result{Outcome: outcome, Outputs: outputs}, nil
}
func (p *callbackAdapter) Permit(context.Context, string, string, bool, string) error { return nil }
func (p *callbackAdapter) CloseSession(context.Context, string) error                 { return nil }
func (p *callbackAdapter) Kill()                                                      {}

// outputAdapter is a test adapter that always returns a fixed outcome and outputs map.
type outputAdapter struct {
	outcome string
	outputs map[string]string
}

func (p *outputAdapter) Info(context.Context) (adapterhost.Info, error) {
	return adapterhost.Info{Name: "output", Version: "test"}, nil
}
func (p *outputAdapter) OpenSession(context.Context, string, map[string]string) error { return nil }
func (p *outputAdapter) Execute(_ context.Context, _ string, _ *workflow.StepNode, _ adapter.EventSink) (adapter.Result, error) {
	return adapter.Result{Outcome: p.outcome, Outputs: p.outputs}, nil
}
func (p *outputAdapter) Permit(context.Context, string, string, bool, string) error { return nil }
func (p *outputAdapter) CloseSession(context.Context, string) error                 { return nil }
func (p *outputAdapter) Kill()                                                      {}

// represent map iteration keys stored in the W07/W10 cursor JSON so the SDK
// can expose each.key on resume.
func TestIter_Keys_SerializeRestore(t *testing.T) {
	cursor := &workflow.IterCursor{
		StepName:   "map_step",
		Index:      1,
		Total:      3,
		InProgress: true,
		Keys: []cty.Value{
			cty.StringVal("alpha"),
			cty.StringVal("beta"),
			cty.StringVal("gamma"),
		},
	}

	data, err := workflow.SerializeIterCursor(cursor)
	if err != nil {
		t.Fatalf("serialize: %v", err)
	}
	if data == "" {
		t.Fatal("serialize returned empty string")
	}

	// Verify the serialized JSON contains the keys array.
	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(data), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	keysRaw, ok := raw["keys"]
	if !ok {
		t.Fatal("serialized JSON missing 'keys' field")
	}
	keysSlice, ok := keysRaw.([]interface{})
	if !ok {
		t.Fatalf("'keys' field is not an array: %T", keysRaw)
	}
	if len(keysSlice) != 3 {
		t.Fatalf("keys length: got %d want 3", len(keysSlice))
	}
	want := []string{"alpha", "beta", "gamma"}
	for i, w := range want {
		if got, _ := keysSlice[i].(string); got != w {
			t.Errorf("keys[%d]: got %q want %q", i, got, w)
		}
	}
}

// TestIter_AggregateOutcome_ReturnOutputProjection verifies that when an
// iterating step's aggregate outcome declares output = { ... } and next =
// "return", the projected outputs are correctly evaluated and emitted via
// OnRunOutputs on the top-level return path.
//
// Prior to the fix, finishIterationInGraph returned co.Next without evaluating
// co.OutputExpr, so st.ReturnOutputs was never populated and the run exited
// with no outputs.
func TestIter_AggregateOutcome_ReturnOutputProjection(t *testing.T) {
	// parseExprIter is a local helper so this test file stays standalone.
	parseExprIter := func(src string) hcl.Expression {
		t.Helper()
		expr, diags := hclsyntax.ParseExpression([]byte(src), "test.hcl", hcl.Pos{Line: 1, Column: 1})
		if diags.HasErrors() {
			t.Fatalf("parseExprIter(%q): %s", src, diags.Error())
		}
		return expr
	}

	step := &workflow.StepNode{
		Name:       "looper",
		TargetKind: workflow.StepTargetAdapter,
		AdapterRef: "fake.default",
		Input:      map[string]string{},
		ForEach:    parseExprIter(`["a", "b"]`),
		Outcomes: map[string]*workflow.CompiledOutcome{
			"success": {
				Name: "success",
				Next: "_continue",
			},
			"all_succeeded": {
				Name:       "all_succeeded",
				Next:       workflow.ReturnSentinel,
				OutputExpr: parseExprIter(`{ done = "yes" }`),
			},
		},
	}
	graph := &workflow.FSMGraph{
		Name:         "t",
		InitialState: "looper",
		TargetState:  "done",
		Policy:       workflow.DefaultPolicy,
		Steps:        map[string]*workflow.StepNode{"looper": step},
		States:       map[string]*workflow.StateNode{"done": {Name: "done", Terminal: true, Success: true}},
		Adapters:     map[string]*workflow.AdapterNode{"fake.default": {Type: "fake", Name: "default"}},
		AdapterOrder: []string{"fake.default"},
		Subworkflows: map[string]*workflow.SubworkflowNode{},
		Variables:    map[string]*workflow.VariableNode{},
		Environments: map[string]*workflow.EnvironmentNode{},
	}

	sink := &outcomeSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"fake":         &fakeAdapter{name: "fake", outcome: "success"},
		"fake.default": &fakeAdapter{name: "fake", outcome: "success"},
	}}
	if err := NewTestEngine(graph, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !sink.terminalOK {
		t.Error("expected terminalOK=true")
	}

	outputMap := make(map[string]string)
	for _, o := range sink.outputs {
		outputMap[o["name"]] = o["value"]
	}
	if got, want := outputMap["done"], `"yes"`; got != want {
		t.Errorf("aggregate return output: done = %q, want %q", got, want)
	}
}
