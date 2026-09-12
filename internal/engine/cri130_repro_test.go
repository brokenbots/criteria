package engine

// cri130_repro_test.go — regression tests for CRI-130: a run where every
// functional step succeeded must complete with success=true even when a tail
// comment_* step crashes (adapter session crash) or fails.
//
// The reported incident: the intake workflow's comment_handler_done shell step
// crashed with `rpc error: code = Canceled desc = grpc: the client connection
// is closing` right before the post-comment ticket-state update
// (set_done_state runs on the same shell.intake session and comes AFTER the
// comment step in linear_intake_v1). Under the default on_crash=fail policy
// the crashed session stays registered but dead, so set_done_state re-observed
// the crash too, routed to the failed terminal, and flipped the run to
// failed — triggering a scratch re-run of the whole workflow. The engine now
// treats comment_* steps and their tail follow-on steps as best-effort: the
// failures are logged and the run continues along the declared success
// transitions.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow"
)

// cri130CrashErr is the exact crash signature from the CRI-130 report
// (CRI-128/CRI-129/CRI-130 runs, comment_handler_done, session shell.intake).
const cri130CrashErr = "rpc error: code = Canceled desc = grpc: the client connection is closing"

// cri130Adapter reproduces the incident at the adapterhost boundary with the
// documented session-crash semantics (internal/adapter/conformance:
// session_crash_detection): the failing step returns the exact observed gRPC
// error — which adapterhost.isLikelySessionCrash classifies as a session
// crash, so the request flows through handleCrash with the default
// on_crash=fail policy — and once crashed, the session stays dead: every
// subsequent Execute on it returns the same crash error, exactly as the
// production shell.intake session did after its teardown race.
type cri130Adapter struct {
	*fakeAdapter
	failStep string
	failMode string
	mu       sync.Mutex
	crashed  bool
}

func (p *cri130Adapter) Execute(ctx context.Context, name string, step *workflow.StepNode, sink adapter.EventSink) (adapter.Result, error) {
	p.mu.Lock()
	crashed := p.crashed
	if !crashed && step != nil && step.Name == p.failStep && p.failMode == "crash" {
		p.crashed = true
		crashed = true
	}
	p.mu.Unlock()
	if crashed {
		return adapter.Result{Outcome: "failure"}, errors.New(cri130CrashErr)
	}
	if step != nil && step.Name == p.failStep {
		return adapter.Result{Outcome: "failure"}, nil
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

// cri130ProductionTailWorkflow mirrors the shipped linear_intake_v1 success
// tail exactly: the comment step runs BEFORE the post-comment ticket-state
// update, and both target the same adapter session (the production
// shell.intake). This is the shape the incident reproduced in — the inverse of
// cri130IntakeWorkflow.
func cri130ProductionTailWorkflow() string {
	return `
workflow {
  name = "linear_intake_cri130"
  version = "0.1"
  initial_state = "merge_pr"
  target_state  = "handler_complete"
}
step "merge_pr" {
  target = adapter.fake
  outcome "success" { next = step.comment_handler_done }
}
step "comment_handler_done" {
  target = adapter.fake
  outcome "success" { next = step.set_done_state }
  outcome "failure" { next = state.failed }
}
step "set_done_state" {
  target = adapter.fake
  outcome "success" { next = state.handler_complete }
  outcome "failure" { next = state.failed }
}
state "handler_complete" { terminal = true }
state "failed" {
  terminal = true
  success  = false
}`
}

// TestCRI130_ProductionTailCommentCrashContinues is the primary CRI-130
// regression: comment_handler_done crashes after merge_pr, leaving the shared
// adapter session dead; the follow-on set_done_state re-observes the same
// crash. Both are best-effort tail steps, so the run must finish at the
// success terminal with no failure routing anywhere.
func TestCRI130_ProductionTailCommentCrashContinues(t *testing.T) {
	g := compile(t, cri130ProductionTailWorkflow())
	p := &cri130Adapter{
		fakeAdapter: &fakeAdapter{name: "fake", outcome: "success"},
		failStep:    "comment_handler_done",
		failMode:    "crash",
	}
	sink := &fakeSink{}
	if err := NewTestEngine(g, cri130NewLoader(p), sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if sink.terminal != "handler_complete" || !sink.terminalOK {
		t.Errorf("terminal state %q success=%v; want handler_complete/true (dead tail session must not flip the run)", sink.terminal, sink.terminalOK)
	}
	if sink.failure != "" {
		t.Errorf("unexpected run failure: %s", sink.failure)
	}
	joined := strings.Join(sink.transitions, ",")
	for _, want := range []string{"merge_pr->comment_handler_done", "comment_handler_done->set_done_state", "set_done_state->handler_complete"} {
		if !strings.Contains(joined, want) {
			t.Errorf("transitions %q; missing %q (suppressed steps must continue along their success routing)", joined, want)
		}
	}
	if strings.Contains(joined, "->failed") {
		t.Errorf("transitions %q; no step may route to the failed terminal (CRI-130)", joined)
	}
	ran := strings.Join(sink.stepsRun, ",")
	if !strings.Contains(ran, "set_done_state") {
		t.Errorf("steps run %q; the follow-on set_done_state must still execute on the crashed session", ran)
	}
}

// A functional follow-on that is not tail bookkeeping — its continuation path
// crosses another adapter step — must keep its genuine failure routing even on
// a crashed session, so mid-workflow failures still fail the run and trigger
// the (correct) scratch re-run instead of silently skipping real work.
func TestCRI130_NonTailFollowOnStillFailsRun(t *testing.T) {
	g := compile(t, `
workflow {
  name = "linear_intake_cri130"
  version = "0.1"
  initial_state = "merge_pr"
  target_state  = "handler_complete"
}
step "merge_pr" {
  target = adapter.fake
  outcome "success" { next = step.comment_progress }
}
step "comment_progress" {
  target = adapter.fake
  outcome "success" { next = step.set_done_state }
  outcome "failure" { next = state.failed }
}
step "set_done_state" {
  target = adapter.fake
  outcome "success" { next = step.finalize_report }
  outcome "failure" { next = state.failed }
}
step "finalize_report" {
  target = adapter.fake
  outcome "success" { next = state.handler_complete }
  outcome "failure" { next = state.failed }
}
state "handler_complete" { terminal = true }
state "failed" {
  terminal = true
  success  = false
}`)
	p := &cri130Adapter{
		fakeAdapter: &fakeAdapter{name: "fake", outcome: "success"},
		failStep:    "comment_progress",
		failMode:    "crash",
	}
	sink := &fakeSink{}
	if err := NewTestEngine(g, cri130NewLoader(p), sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if sink.terminal != "failed" || sink.terminalOK {
		t.Errorf("terminal state %q success=%v; want failed/false (non-tail follow-on failure must keep genuine routing)", sink.terminal, sink.terminalOK)
	}
	if got := strings.Join(sink.transitions, ","); !strings.Contains(got, "set_done_state->failed") {
		t.Errorf("transitions %q; want set_done_state to route via its failure outcome (no silent downgrade)", got)
	}
}

// TestCRI130_CommentStepRetryExhaustionContinues covers the retry-exhausted
// hook: a comment step with only a success outcome crashes, retries are
// exhausted, and the exhaustion is still treated as best-effort instead of
// failing the run with a wrapped error.
func TestCRI130_CommentStepRetryExhaustionContinues(t *testing.T) {
	g := compile(t, cri130IntakeWorkflow(`
  outcome "success" { next = state.done }`))
	p := &cri130Adapter{
		fakeAdapter: &fakeAdapter{name: "fake", outcome: "success"},
		failStep:    "comment_handler_done",
		failMode:    "crash",
	}
	sink := &fakeSink{}
	if err := NewTestEngine(g, cri130NewLoader(p), sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v (retry exhaustion of a comment step must stay best-effort)", err)
	}
	if sink.terminal != "done" || !sink.terminalOK {
		t.Errorf("terminal state %q success=%v; want done/true", sink.terminal, sink.terminalOK)
	}
	if sink.failure != "" {
		t.Errorf("unexpected run failure: %s", sink.failure)
	}
}

// TestCRI130_CommentStepCrashAfterMergeSucceeds covers the shape where the
// comment step is the literal last step before the success terminal.
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
