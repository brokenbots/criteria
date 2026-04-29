package engine_test

// node_for_each_multistep_test.go — W08 engine-level tests for multi-step
// for_each iteration bodies (tests 9–14 from the workstream specification).

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/engine"
	"github.com/brokenbots/criteria/internal/plugin"
	"github.com/brokenbots/criteria/workflow"
)

// multiSink extends forEachSink to also record OnForEachStep events, which
// forEachSink discards. Tests 9–13 need this detail.
type multiSink struct {
	forEachSink
	feSteps []feStepEvent
}

type feStepEvent struct {
	node  string
	index int
	step  string
}

func (s *multiSink) OnForEachStep(node string, index int, step string) {
	s.mu.Lock()
	s.feSteps = append(s.feSteps, feStepEvent{node: node, index: index, step: step})
	s.mu.Unlock()
}

// multiStepLoader is removed — use newPerStepLoader directly.

// perStepPlugin routes to per-step outcome functions and optionally captures
// the resolved input value ("value" key) passed to each step execution.
type perStepPlugin struct {
	name     string
	outcomes map[string]func() string // step name → outcome fn
	fallback string
	captured map[string][]string // step name → list of "value" inputs received
}

func (p *perStepPlugin) Info(context.Context) (plugin.Info, error) {
	return plugin.Info{Name: p.name, Version: "test"}, nil
}
func (p *perStepPlugin) OpenSession(context.Context, string, map[string]string) error { return nil }
func (p *perStepPlugin) Execute(_ context.Context, _ string, step *workflow.StepNode, _ adapter.EventSink) (adapter.Result, error) {
	if p.captured != nil {
		if v, ok := step.Input["value"]; ok {
			p.captured[step.Name] = append(p.captured[step.Name], v)
		}
	}
	if fn, ok := p.outcomes[step.Name]; ok {
		return adapter.Result{Outcome: fn()}, nil
	}
	return adapter.Result{Outcome: p.fallback}, nil
}
func (p *perStepPlugin) Permit(context.Context, string, string, bool, string) error { return nil }
func (p *perStepPlugin) CloseSession(context.Context, string) error                 { return nil }
func (p *perStepPlugin) Kill()                                                      {}

func newPerStepLoader(outcomes map[string]func() string) *staticLoader {
	p := &perStepPlugin{name: "noop", outcomes: outcomes, fallback: "success"}
	return &staticLoader{plugins: map[string]plugin.Plugin{"noop": p}}
}

// newCapturingLoader returns a loader backed by a perStepPlugin that records
// the "value" input key for each step call. The plugin pointer is returned so
// tests can inspect captured values after the run.
func newCapturingLoader(outcomes map[string]func() string) (*staticLoader, *perStepPlugin) {
	p := &perStepPlugin{
		name:     "noop",
		outcomes: outcomes,
		fallback: "success",
		captured: make(map[string][]string),
	}
	return &staticLoader{plugins: map[string]plugin.Plugin{"noop": p}}, p
}

// loadHCL reads and compiles a fixture from testdata/for_each/.
func loadHCL(t *testing.T, name string) *workflow.FSMGraph {
	t.Helper()
	src, err := os.ReadFile("testdata/for_each/" + name)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	spec, diags := workflow.Parse(name, src)
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := workflow.Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	return g
}

// --- Test 9: multi-step iteration runs end-to-end ---

func TestForEachMultiStep_EndToEnd(t *testing.T) {
	g := loadHCL(t, "multi_step.hcl")
	vars := workflow.SeedVarsFromGraph(g)
	sink := &multiSink{}
	loader, plug := newCapturingLoader(map[string]func() string{
		"execute": func() string { return "success" },
		"review":  func() string { return "success" },
		"cleanup": func() string { return "success" },
	})

	eng := engine.New(g, loader, sink, engine.WithResumedVars(vars))
	if err := eng.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	// 3 items × 3 steps = 9 step entries; 3 iterations; 1 outcome.
	if len(sink.iterations) != 3 {
		t.Fatalf("iterations: got %d, want 3", len(sink.iterations))
	}

	// Each iteration should have its 3 steps: execute, review, cleanup.
	wantSteps := []string{"execute", "review", "cleanup", "execute", "review", "cleanup", "execute", "review", "cleanup"}
	if len(sink.steps) != len(wantSteps) {
		t.Fatalf("steps: got %v, want %v", sink.steps, wantSteps)
	}
	for i, want := range wantSteps {
		if sink.steps[i] != want {
			t.Errorf("step[%d] = %q, want %q", i, sink.steps[i], want)
		}
	}

	// OnForEachStep should fire for review and cleanup (not execute — it's the do-step).
	// 3 items × 2 non-do steps = 6 ForEachStep events.
	if len(sink.feSteps) != 6 {
		t.Fatalf("ForEachStep events: got %d, want 6; events: %v", len(sink.feSteps), sink.feSteps)
	}
	for i, ev := range sink.feSteps {
		if ev.node != "loop" {
			t.Errorf("feStep[%d].node = %q, want 'loop'", i, ev.node)
		}
		wantStep := []string{"review", "cleanup"}[i%2]
		if ev.step != wantStep {
			t.Errorf("feStep[%d].step = %q, want %q", i, ev.step, wantStep)
		}
		wantIdx := i / 2
		if ev.index != wantIdx {
			t.Errorf("feStep[%d].index = %d, want %d", i, ev.index, wantIdx)
		}
	}

	// Verify each.value is correctly bound in every step, not just execute.
	// The fixture passes `value = each.value` in all three steps.
	items := []string{"a", "b", "c"}
	for _, stepName := range []string{"execute", "review", "cleanup"} {
		got := plug.captured[stepName]
		if len(got) != len(items) {
			t.Errorf("%s: captured %d values, want %d; got %v", stepName, len(got), len(items), got)
			continue
		}
		for i, want := range items {
			if got[i] != want {
				t.Errorf("%s call[%d]: each.value = %q, want %q", stepName, i, got[i], want)
			}
		}
	}

	// Outcome should be all_succeeded → done.
	if len(sink.outcomes) != 1 {
		t.Fatalf("outcomes: got %d, want 1", len(sink.outcomes))
	}
	if sink.outcomes[0].outcome != "all_succeeded" {
		t.Errorf("outcome = %q, want 'all_succeeded'", sink.outcomes[0].outcome)
	}
	if sink.terminal != "done" || !sink.terminalOK {
		t.Errorf("terminal = %q ok=%v, want done/true", sink.terminal, sink.terminalOK)
	}
}

// --- Test 10: mid-iteration failure continues to cleanup but sets AnyFailed ---

func TestForEachMultiStep_MidIterationFailureContinuesToCleanup(t *testing.T) {
	g := loadHCL(t, "multi_step.hcl")
	vars := workflow.SeedVarsFromGraph(g)
	sink := &multiSink{}
	callCount := 0
	loader := newPerStepLoader(map[string]func() string{
		"execute": func() string { return "success" },
		"review": func() string {
			callCount++
			if callCount == 2 { // second iteration's review fails
				return "failure"
			}
			return "success"
		},
		"cleanup": func() string { return "success" },
	})

	eng := engine.New(g, loader, sink, engine.WithResumedVars(vars))
	if err := eng.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	// All 3 iterations should complete (failure doesn't abort loop; only
	// exits via non-subgraph transition would abort).
	if len(sink.iterations) != 3 {
		t.Fatalf("iterations: got %d, want 3", len(sink.iterations))
	}

	// Third iteration should see anyFailed=true after second review failure.
	if !sink.iterations[2].anyFailed {
		t.Error("third iteration should see anyFailed=true after second review failure")
	}

	// Outcome: any_failed because review returned "failure" (not "success").
	if len(sink.outcomes) != 1 {
		t.Fatalf("outcomes: got %d, want 1", len(sink.outcomes))
	}
	if sink.outcomes[0].outcome != "any_failed" {
		t.Errorf("outcome = %q, want 'any_failed'", sink.outcomes[0].outcome)
	}
}

// --- Test 11: early-exit via transition to step outside the subgraph ---

func TestForEachMultiStep_EarlyExitViaOutOfSubgraphTransition(t *testing.T) {
	g := loadHCL(t, "early_exit.hcl")
	vars := workflow.SeedVarsFromGraph(g)
	sink := &multiSink{}
	// First item: execute → review → escalate (early-exit on first item).
	callCount := 0
	loader := newPerStepLoader(map[string]func() string{
		"execute": func() string { return "success" },
		"review": func() string {
			callCount++
			if callCount == 1 {
				return "escalate" // triggers early-exit on first iteration
			}
			return "success"
		},
		"escalate": func() string { return "success" },
	})

	eng := engine.New(g, loader, sink, engine.WithResumedVars(vars))
	if err := eng.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	// Only 1 iteration event (loop aborted after first item).
	if len(sink.iterations) != 1 {
		t.Fatalf("iterations: got %d, want 1", len(sink.iterations))
	}

	// Outcome should be any_failed (early-exit).
	if len(sink.outcomes) != 1 {
		t.Fatalf("outcomes: got %d, want 1", len(sink.outcomes))
	}
	if sink.outcomes[0].outcome != "any_failed" {
		t.Errorf("outcome = %q, want 'any_failed'", sink.outcomes[0].outcome)
	}
	if sink.outcomes[0].node != "loop" {
		t.Errorf("outcome node = %q, want 'loop'", sink.outcomes[0].node)
	}

	// Escalate step should have run.
	found := false
	for _, s := range sink.steps {
		if s == "escalate" {
			found = true
		}
	}
	if !found {
		t.Errorf("escalate step never ran; steps = %v", sink.steps)
	}

	// Run should still reach a terminal state.
	if sink.terminal == "" {
		t.Error("run did not reach a terminal state")
	}
}

// --- Test 12: crash-resume mid-iteration (at review) ---

func TestForEachMultiStep_CrashResumeMidIteration(t *testing.T) {
	g := loadHCL(t, "multi_step.hcl")
	vars := workflow.SeedVarsFromGraph(g)

	// Simulate crash: cursor at index 1 ("b"), mid-iteration at review.
	// Items=nil because they are not persisted in checkpoints; rebindEachOnResume
	// must re-evaluate them so each.value is correctly set to "b".
	crashCursor := &workflow.IterCursor{
		NodeName:   "loop",
		Index:      1,
		AnyFailed:  false,
		InProgress: true,
		Items:      nil,
	}

	resumeSink := &multiSink{}
	loader, plug := newCapturingLoader(map[string]func() string{
		"execute": func() string { return "success" },
		"review":  func() string { return "success" },
		"cleanup": func() string { return "success" },
	})

	eng := engine.New(g, loader, resumeSink,
		engine.WithResumedVars(vars),
		engine.WithResumedIter(crashCursor),
	)
	// Resume at "review" (the mid-iteration step for index 1 = "b").
	if err := eng.RunFrom(context.Background(), "review", 1); err != nil {
		t.Fatalf("RunFrom: %v", err)
	}

	if resumeSink.terminal != "done" {
		t.Errorf("terminal = %q, want 'done'", resumeSink.terminal)
	}

	// rebindEachOnResume must have bound each.value = "b" for the resumed
	// half-iteration (index 1). Verify review and cleanup both received "b".
	// Then the loop advances to index 2 ("c"), so they also run with "c".
	for _, stepName := range []string{"review", "cleanup"} {
		got := plug.captured[stepName]
		if len(got) < 1 {
			t.Errorf("%s: no captured values; each.* may not have been re-bound on resume", stepName)
			continue
		}
		// First captured call must be "b" (the resumed iteration).
		if got[0] != "b" {
			t.Errorf("%s first call after resume: each.value = %q, want %q (rebindEachOnResume broken)", stepName, got[0], "b")
		}
	}
}

// --- Test 13: nested for_each compile check ---

// TestForEachMultiStep_NestedForEach_OverlapRejected verifies that two for_each
// nodes accidentally sharing a step in their iteration bodies produce a compile
// error. Explicit nesting (an inner for_each within the outer body) compiles.
func TestForEachMultiStep_NestedForEach_OverlapRejected(t *testing.T) {
	// Two for_each nodes whose subgraphs overlap (both try to claim "cleanup").
	// This should fail to compile (tested in compile tests too, but verifying
	// from the engine test package as well for defence in depth).
	src := `
workflow "t" {
  version       = "0.1"
  initial_state = "loop_a"
  target_state  = "done"

  for_each "loop_a" {
    items = ["a"]
    do    = "execute_a"
    outcome "all_succeeded" { transition_to = "loop_b" }
    outcome "any_failed"    { transition_to = "done" }
  }

  for_each "loop_b" {
    items = ["b"]
    do    = "execute_b"
    outcome "all_succeeded" { transition_to = "done" }
    outcome "any_failed"    { transition_to = "done" }
  }

  step "execute_a" {
    adapter = "noop"
    outcome "success" { transition_to = "cleanup" }
  }

  step "execute_b" {
    adapter = "noop"
    outcome "success" { transition_to = "cleanup" }
  }

  step "cleanup" {
    adapter = "noop"
    outcome "done" { transition_to = "_continue" }
  }

  state "done" {
    terminal = true
    success  = true
  }
}
`
	spec, diags := workflow.Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse error: %s", diags.Error())
	}
	_, diags = workflow.Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for overlapping subgraphs, got none")
	}
	if !strings.Contains(diags.Error(), "steps cannot be shared between distinct for_each subgraphs") {
		t.Errorf("expected overlap diagnostic; got: %s", diags.Error())
	}
}

// --- Test 14: graph invariants that enable CLI subgraph membership check ---

// TestForEachMultiStep_ResumeSubgraphMembershipCheck verifies the graph
// invariants (IterationOwner + IterationSteps) that checkIterationSubgraphMembership
// (in internal/cli) relies on. Confirms that after compilation a step in an
// iteration body has IterationOwner set, and that removing it from IterationSteps
// creates the exact inconsistency the CLI check is designed to detect.
//
// CLI-level enforcement tests (calling checkIterationSubgraphMembership directly)
// live in internal/cli/reattach_test.go.
func TestForEachMultiStep_ResumeSubgraphMembershipCheck(t *testing.T) {
	src := `
workflow "t" {
  version       = "0.1"
  initial_state = "loop"
  target_state  = "done"

  for_each "loop" {
    items = ["a"]
    do    = "execute"
    outcome "all_succeeded" { transition_to = "done" }
    outcome "any_failed"    { transition_to = "done" }
  }

  step "execute" {
    adapter = "noop"
    outcome "success" { transition_to = "review" }
  }

  step "review" {
    adapter = "noop"
    outcome "done" { transition_to = "_continue" }
  }

  state "done" {
    terminal = true
    success  = true
  }
}
`
	spec, diags := workflow.Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := workflow.Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}

	// After compilation, "review" must be in the loop subgraph with IterationOwner set.
	fe := g.ForEachs["loop"]
	if fe == nil {
		t.Fatal("for_each 'loop' not found")
	}
	if _, ok := fe.IterationSteps["review"]; !ok {
		t.Fatalf("review not in loop's IterationSteps; got %v", fe.IterationSteps)
	}
	reviewStep := g.Steps["review"]
	if reviewStep.IterationOwner != "loop" {
		t.Fatalf("review.IterationOwner = %q, want 'loop'", reviewStep.IterationOwner)
	}

	// Simulate an incompatible workflow edit by removing "review" from the subgraph.
	// After this, IterationOwner is set but the for_each no longer claims the step —
	// the exact mismatch checkIterationSubgraphMembership is designed to catch.
	delete(fe.IterationSteps, "review")
	if _, stillMember := fe.IterationSteps["review"]; stillMember {
		t.Fatal("expected review absent from IterationSteps after simulated edit")
	}
	if reviewStep.IterationOwner == "" {
		t.Fatal("expected review.IterationOwner still set (simulating stale checkpoint)")
	}
}
