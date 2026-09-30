package engine

// kb57_repro_test.go — regression tests for KB-57: step outcomes dropped
// engine-side in post-develop bookkeeping chains.
//
// Incident shape (castle run 4246be92 in run kb-55-1790727148): develop
// resolved ready_for_review, the write_work_log/commit_work_log shell steps
// resolved success, and the THIRD shell step push_wip_checkpoint produced NO
// outcome — step.entered (seq 772) was the last event before the runner tore
// sessions down; the run completed via comment_handler_failed without the
// next step ever running. The loader dropped the shell step's delivered
// ExecuteResult under the teardown race, and failed attempts for steps that
// declare no failure outcome were emitted with an empty workflow-level
// outcome — invisible to event consumers as a "step.outcome".
//
// Two halves are pinned here:
//  1. a step whose Execute context (session teardown / step ceiling) is
//     already cancelled when the result is delivered resolves to the
//     adapter's verdict and the chain continues;
//  2. a failed attempt for a step with no declared failure outcome emits a
//     visible "failure" workflow-level outcome instead of an empty one.

import (
	"context"
	"errors"
	"testing"
	"time"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/brokenbots/criteria/internal/adapterhost"
)

// kb57Workflow mirrors the castle post-develop tail: the develop turn
// resolves, the bookkeeping chain runs (write_work_log, commit_work_log),
// the checkpoint push runs under a short step ceiling whose teardown window
// races the result delivery, and the chain must reach the terminal state.
// push_wip_checkpoint declares only a success outcome, matching the
// incident's shell wrapper.
const kb57Workflow = `
workflow {
  name = "kb57"
  version = "0.1"
  initial_state = "develop"
  target_state  = "done"
}
step "develop" {
  target = adapter.copilot
  outcome "ready_for_review" { next = step.write_work_log }
  outcome "failure"          { next = step.comment_handler_failed }
}
step "write_work_log" {
  target = adapter.pipeline
  outcome "success" { next = step.commit_work_log }
}
step "commit_work_log" {
  target = adapter.pipeline
  outcome "success" { next = step.push_wip_checkpoint }
}
step "push_wip_checkpoint" {
  target = adapter.shell
  timeout = "40ms"
  outcome "success" { next = state.done }
}
step "comment_handler_failed" {
  target = adapter.pipeline
  outcome "success" { next = state.done }
}
state "done" {
  terminal = true
  success  = true
}
`

// kb57EmptyOutcomeWorkflow: a step that declares only a success outcome. A
// failed attempt on it previously emitted an EMPTY workflow-level outcome;
// it must emit "failure" so event consumers can see the attempt resolve.
const kb57EmptyOutcomeWorkflow = `
workflow {
  name = "kb57-empty"
  version = "0.1"
  initial_state = "push_wip_checkpoint"
  target_state  = "done"
}
step "push_wip_checkpoint" {
  target = adapter.shell
  outcome "success" { next = state.done }
}
state "done" {
  terminal = true
  success  = true
}
`

// kb57TeardownShellClient is the shell-adapter transport for the incident:
// the shell command needs longer than the step ceiling, so the engine's
// cancellation fires first; the command then COMPLETES and its full
// ExecuteResult is delivered into a context the engine has already torn
// down. The stream ends on the already-fired cancellation, exactly the
// window in which the delivered verdict used to be dropped.
type kb57TeardownShellClient struct {
	kb53FinalizingClient
}

func (c *kb57TeardownShellClient) Execute(ctx context.Context, _ *v2.ExecuteRequest, sink adapterhost.ExecuteEventSink) error {
	// Work past the step ceiling: the deadline fires while the shell command
	// is still running (select prefers the already-ready cancellation).
	select {
	case <-ctx.Done():
	case <-time.After(200 * time.Millisecond):
	}
	// The command completed and delivers its result; the outcome must land
	// even though the execution context is gone.
	if err := sink.Emit(&v2.ExecuteEvent{
		Event: &v2.ExecuteEvent_Result{
			Result: &v2.ExecuteResult{Outcome: "success"},
		},
	}); err != nil {
		return err
	}
	return ctx.Err()
}

func kb57LoaderWithShell(client adapterhost.Client) *fakeLoader {
	return &fakeLoader{adapters: map[string]adapterhost.Handle{
		"copilot":          &fakeAdapter{name: "copilot", outcome: "ready_for_review"},
		"copilot.default":  &fakeAdapter{name: "copilot", outcome: "ready_for_review"},
		"pipeline":         &fakeAdapter{name: "pipeline", outcome: "success"},
		"pipeline.default": &fakeAdapter{name: "pipeline", outcome: "success"},
		"shell":            adapterhost.NewRPCHandle("shell.default", nil, client),
		"shell.default":    adapterhost.NewRPCHandle("shell.default", nil, client),
	}}
}

// TestKB57_TeardownRacingStepCompletionPreservesOutcome: the bookkeeping
// chain runs to the checkpoint push, the step's deadline fires while the
// shell command runs, the command completes anyway and delivers its result,
// and the step must resolve to the delivered verdict with the chain
// continuing to the terminal state. Pre-KB-57 the delivered result was
// discarded for a synthetic failure whose empty outcome made the step
// vanish from the event stream (step.entered was the last event).
func TestKB57_TeardownRacingStepCompletionPreservesOutcome(t *testing.T) {
	g := compile(t, kb57Workflow)
	sink := &kb53CopilotSink{fakeSink: &fakeSink{}}

	if err := NewTestEngine(g, kb57LoaderWithShell(&kb57TeardownShellClient{}), sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v (the delivered ExecuteResult must survive the teardown race)", err)
	}
	if sink.terminal != "done" || !sink.terminalOK {
		t.Errorf("terminal state %q success=%v; want done/true", sink.terminal, sink.terminalOK)
	}

	outcomes, transitions, _ := sink.snapshot()

	// The push step resolved to the adapter's verdict — not empty, not
	// failure.
	found := false
	for _, o := range outcomes {
		if o == "push_wip_checkpoint=success" {
			found = true
		}
		if o == "push_wip_checkpoint=" || o == "push_wip_checkpoint=failure" {
			t.Errorf("step outcomes = %v; the teardown race must not erase or invert the delivered outcome", outcomes)
		}
	}
	if !found {
		t.Fatalf("step outcomes = %v; want push_wip_checkpoint=success", outcomes)
	}

	// The chain must have continued: the bookkeeping steps and the push all
	// resolved, reaching the terminal state (the run was not truncated).
	joined := ""
	for _, tr := range transitions {
		joined += tr + " "
	}
	for _, wantEdge := range []string{
		"develop->write_work_log",
		"write_work_log->commit_work_log",
		"commit_work_log->push_wip_checkpoint",
		"push_wip_checkpoint->done",
	} {
		if !contains(joined, wantEdge) {
			t.Errorf("transitions %v; want edge %q", transitions, wantEdge)
		}
	}

	if sink.failure != "" {
		t.Errorf("OnRunFailed(%q); the run must complete successfully once the outcome lands", sink.failure)
	}
}

// TestKB57_FailureWithoutDeclaredOutcomeIsVisible: a failed attempt for a
// step that declares no failure outcome must emit a WORKFLOW-LEVEL "failure"
// outcome, never an empty one. In the incident, failed attempts recorded an
// empty outcome string (no step.outcome at all in castle events), which is
// how the third shell step vanished between step.entered and the teardown.
func TestKB57_FailureWithoutDeclaredOutcomeIsVisible(t *testing.T) {
	g := compile(t, kb57EmptyOutcomeWorkflow)
	sink := &kb53CopilotSink{fakeSink: &fakeSink{}}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"shell":         &fakeAdapter{name: "shell", outcome: "success", err: errors.New("shell exit 1")},
		"shell.default": &fakeAdapter{name: "shell", outcome: "success", err: errors.New("shell exit 1")},
	}}

	// MaxStepRetries defaults to 0: exactly one attempt, then the exhaustive
	// error fails the run.
	if err := NewTestEngine(g, loader, sink).Run(context.Background()); err == nil {
		t.Fatalf("run: err = nil, want the attempt exhaustion error")
	}

	outcomes, _, _ := sink.snapshot()
	if len(outcomes) != 1 {
		t.Fatalf("step outcomes = %v; want exactly the push attempt outcome", outcomes)
	}
	if outcomes[0] != "push_wip_checkpoint=failure" {
		t.Errorf("step outcomes = %v; want push_wip_checkpoint=failure (an empty outcome is invisible to event consumers)", outcomes)
	}
}
