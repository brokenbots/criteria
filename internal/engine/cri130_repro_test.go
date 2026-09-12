package engine

// cri130_repro_test.go — regression tests for CRI-130: a run where every
// functional step succeeded must complete with success=true even when a tail
// comment_* step crashes (adapter session crash) or fails.
//
// The reported incident: the intake workflow's final comment_handler_done
// shell step crashed with `rpc error: code = Canceled desc = grpc: the client
// connection is closing` after the PR was already merged and the ticket state
// set, flipping the run's terminal state to failed and triggering a scratch
// re-run. The engine now treats comment_* steps as best-effort: the failure is
// logged and the run continues along the step's success transition.
//
// cri130Adapter reproduces the incident at the adapterhost boundary: the
// failing step returns the exact observed gRPC error, which
// adapterhost.isLikelySessionCrash classifies as a session crash, so the
// request flows through handleCrash with the default on_crash=fail policy and
// reaches the engine as Result{failure} plus the "session crashed" error —
// the same shape the production run produced.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow"
)

// cri130CrashErr is the exact crash signature from the CRI-130 report
// (CRI-128/CRI-129/CRI-130 runs, comment_handler_done, session shell.intake).
const cri130CrashErr = "rpc error: code = Canceled desc = grpc: the client connection is closing"

// cri130Adapter returns success for every step except failStep, which fails in
// the configured mode: "crash" (session crash error, exercising the full
// adapterhost crash path) or "failure" (clean failure outcome, no error).
type cri130Adapter struct {
	*fakeAdapter
	failStep string
	failMode string
}

func (p *cri130Adapter) Execute(ctx context.Context, name string, step *workflow.StepNode, sink adapter.EventSink) (adapter.Result, error) {
	if step != nil && step.Name == p.failStep {
		switch p.failMode {
		case "crash":
			return adapter.Result{Outcome: "failure"}, errors.New(cri130CrashErr)
		default:
			return adapter.Result{Outcome: "failure"}, nil
		}
	}
	return p.fakeAdapter.Execute(ctx, name, step, sink)
}

// cri130NewLoader registers the adapter under both the bare type name and the
// dotted reference the compiled graph uses.
func cri130NewLoader(p *cri130Adapter) *fakeLoader {
	return &fakeLoader{adapters: map[string]adapterhost.Handle{
		"fake":         p,
		"fake.default": p,
	}}
}

// cri130IntakeWorkflow mirrors the tail of the reported intake run: a
// functional merge step, the ticket-state transition, then the failing tail
// comment step whose declared failure outcome routes to a failed terminal —
// the transition that poisoned the run before the fix.
func cri130IntakeWorkflow(commentStepOutcomes string) string {
	return `
workflow {
  name = "linear_intake_cri130"
  version = "0.1"
  initial_state = "merge_pr"
  target_state  = "done"
}
step "merge_pr" {
  target = adapter.fake
  outcome "success" { next = step.set_done_state }
}
step "set_done_state" {
  target = adapter.fake
  outcome "success" { next = step.comment_handler_done }
}
step "comment_handler_done" {
  target = adapter.fake
` + commentStepOutcomes + `
}
state "done" { terminal = true }
state "failed" {
  terminal = true
  success  = false
}`
}

func TestCRI130_CommentStepCrashAfterMergeSucceeds(t *testing.T) {
	g := compile(t, cri130IntakeWorkflow(`
  outcome "success" { next = state.done }
  outcome "failure" { next = state.failed }`))
	p := &cri130Adapter{
		fakeAdapter: &fakeAdapter{name: "fake", outcome: "success"},
		failStep:    "comment_handler_done",
		failMode:    "crash",
	}
	sink := &fakeSink{}
	if err := NewTestEngine(g, cri130NewLoader(p), sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if sink.terminal != "done" || !sink.terminalOK {
		t.Errorf("terminal state %q success=%v; want done/true (comment crash must not flip the run)", sink.terminal, sink.terminalOK)
	}
	if sink.failure != "" {
		t.Errorf("unexpected run failure: %s", sink.failure)
	}
	if got := strings.Join(sink.transitions, ","); !strings.Contains(got, "comment_handler_done->done") || strings.Contains(got, "comment_handler_done->failed") {
		t.Errorf("transitions %q; want continuation to done, not the failure outcome", got)
	}
}

func TestCRI130_CommentStepCleanFailureOutcomeContinues(t *testing.T) {
	g := compile(t, cri130IntakeWorkflow(`
  outcome "success" { next = state.done }
  outcome "failure" { next = state.failed }`))
	p := &cri130Adapter{
		fakeAdapter: &fakeAdapter{name: "fake", outcome: "success"},
		failStep:    "comment_handler_done",
		failMode:    "failure",
	}
	sink := &fakeSink{}
	if err := NewTestEngine(g, cri130NewLoader(p), sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if sink.terminal != "done" || !sink.terminalOK {
		t.Errorf("terminal state %q success=%v; want done/true", sink.terminal, sink.terminalOK)
	}
	if sink.failure != "" {
		t.Errorf("unexpected run failure: %s", sink.failure)
	}
}

// A comment step on a failure branch may declare only its failure outcome
// (e.g. comment_invalid -> state.failed). Without a success/default
// transition there is nowhere to continue to, so the step's declared routing
// still applies.
func TestCRI130_CommentStepWithoutSuccessTransitionKeepsRouting(t *testing.T) {
	g := compile(t, cri130IntakeWorkflow(`
  outcome "failure" { next = state.failed }`))
	p := &cri130Adapter{
		fakeAdapter: &fakeAdapter{name: "fake", outcome: "success"},
		failStep:    "comment_handler_done",
		failMode:    "crash",
	}
	sink := &fakeSink{}
	if err := NewTestEngine(g, cri130NewLoader(p), sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if sink.terminal != "failed" || sink.terminalOK {
		t.Errorf("terminal state %q success=%v; want failed/false (declared failure routing preserved)", sink.terminal, sink.terminalOK)
	}
	if sink.failure != "" {
		t.Errorf("unexpected run failure: %s", sink.failure)
	}
}

// An explicit on_crash=abort_run on a comment step is author intent: the run
// must still fail.
func TestCRI130_CommentStepAbortRunStillFatal(t *testing.T) {
	g := compile(t, cri130IntakeWorkflow(`
  on_crash = "abort_run"
  outcome "success" { next = state.done }
  outcome "failure" { next = state.failed }`))
	p := &cri130Adapter{
		fakeAdapter: &fakeAdapter{name: "fake", outcome: "success"},
		failStep:    "comment_handler_done",
		failMode:    "crash",
	}
	sink := &fakeSink{}
	if err := NewTestEngine(g, cri130NewLoader(p), sink).Run(context.Background()); err == nil {
		t.Fatal("expected run failure for on_crash=abort_run")
	}
	if sink.failure == "" {
		t.Error("expected OnRunFailed for on_crash=abort_run")
	}
}

// Non-comment steps are unaffected: a session crash at a functional step
// still routes via its declared failure outcome.
func TestCRI130_NonCommentStepCrashStillFailsRun(t *testing.T) {
	g := compile(t, `
workflow {
  name = "t"
  version = "0.1"
  initial_state = "merge_pr"
  target_state  = "done"
}
step "merge_pr" {
  target = adapter.fake
  outcome "success" { next = step.set_done_state }
  outcome "failure" { next = state.failed }
}
step "set_done_state" {
  target = adapter.fake
  outcome "success" { next = state.done }
}
state "done" { terminal = true }
state "failed" {
  terminal = true
  success  = false
}`)
	p := &cri130Adapter{
		fakeAdapter: &fakeAdapter{name: "fake", outcome: "success"},
		failStep:    "merge_pr",
		failMode:    "crash",
	}
	sink := &fakeSink{}
	if err := NewTestEngine(g, cri130NewLoader(p), sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if sink.terminal != "failed" || sink.terminalOK {
		t.Errorf("terminal state %q success=%v; want failed/false", sink.terminal, sink.terminalOK)
	}
}
