package engine

// cri130_repro_test.go — regression tests for CRI-130: the engine must route
// step outcomes exclusively through the routing the workflow declares. The
// old CRI-130 "best-effort" mechanism keyed on the step-name prefix comment_
// rewrote failed tail comment steps into successes (observed in run
// 6905052a: stepOutcome comment_handler_failed outcome=failure immediately
// followed by stepTransition to=set_review_state viaOutcome=success), making
// the declared failure arm dead code. That mechanism is deleted: the engine
// is step-name-agnostic, and a workflow that wants a soft tail step declares
// that routing itself (success-flagged terminal, recovery branch).
//
// The CRI-271 session-crash repair (RunState.CrashedSessions) is now
// step-name-agnostic too: a crashed session is recorded wherever it happens
// and follow-on steps re-open it and execute for real, routing through their
// own declared outcomes — no outcome is ever rewired by the engine.

import (
	"context"
	"errors"
	"fmt"
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
//
// failMode "failure" keeps the session alive (clean failure outcome, nil
// error) for a functional failure; failMode "crash" and any crashStep hit arm
// the sticky dead session.
type cri130Adapter struct {
	*fakeAdapter
	failStep  string
	failMode  string
	crashStep string
	mu        sync.Mutex
	crashed   bool
}

func (p *cri130Adapter) Execute(ctx context.Context, name string, step *workflow.StepNode, sink adapter.EventSink) (adapter.Result, error) {
	p.mu.Lock()
	crashed := p.crashed
	stepName := ""
	if step != nil {
		stepName = step.Name
	}
	if !crashed && ((stepName == p.failStep && p.failMode == "crash") || (p.crashStep != "" && stepName == p.crashStep)) {
		p.crashed = true
		crashed = true
	}
	p.mu.Unlock()
	if crashed {
		return adapter.Result{Outcome: "failure"}, errors.New(cri130CrashErr)
	}
	if stepName == p.failStep {
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

// cri130ViaSink records transitions with their via outcome, so tests can pin
// the exact observed failure signature: for the same step execution the
// stepOutcome event said "failure" while the transition's viaOutcome said
// "success".
type cri130ViaSink struct {
	*fakeSink

	mu   sync.Mutex
	vias []string
}

func (s *cri130ViaSink) OnStepTransition(from, to, via string) {
	s.fakeSink.OnStepTransition(from, to, via)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.vias = append(s.vias, from+"->"+to+" via "+via)
}

// cri130CommentTailWorkflow mirrors the incident tail: merge_pr, the
// pre-comment ticket-state update, then the comment step whose declared
// failure outcome routes to the failed terminal and whose success arm feeds
// the post-comment state update.
func cri130CommentTailWorkflow() string {
	return `
workflow {
  name = "linear_intake_cri130"
  version = "0.1"
  initial_state = "merge_pr"
  target_state  = "handler_complete"
}
step "merge_pr" {
  target = adapter.fake
  outcome "success" { next = step.set_review_state }
}
step "set_review_state" {
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

// TestCRI130_CommentDeclaredFailureRoutesToFailedTerminal is the primary
// KB-44/CRI-130 regression: the comment step declares
// outcome "failure" { next = state.failed } (CRI-275 semantics) and fails —
// cleanly (adapter failure outcome) or by crashing its adapter session. The
// declared failure arm must be taken: the run ends at the failed terminal
// with success=false, and the success arm's post-comment bookkeeping never
// executes. Under the deleted best-effort mechanism the failure outcome was
// rewritten to success and the run continued to set_review_state via
// viaOutcome=success — laundering the failure exactly as the incident
// reported.
func TestCRI130_CommentDeclaredFailureRoutesToFailedTerminal(t *testing.T) {
	for _, tc := range []struct {
		name     string
		failMode string
	}{
		{"clean_failure_outcome", "failure"},
		{"session_crash", "crash"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := compile(t, cri130CommentTailWorkflow())
			p := &cri130Adapter{
				fakeAdapter: &fakeAdapter{name: "fake", outcome: "success"},
				failStep:    "comment_handler_done",
				failMode:    tc.failMode,
			}
			sink := &cri130ViaSink{fakeSink: &fakeSink{}}
			if err := NewTestEngine(g, cri130NewLoader(p), sink).Run(context.Background()); err != nil {
				t.Fatalf("run: %v (a declared failure arm routes; it does not fail the run at the engine level)", err)
			}
			if sink.terminal != "failed" || sink.terminalOK {
				t.Errorf("terminal state %q success=%v; want failed/false (declared failure arm)", sink.terminal, sink.terminalOK)
			}
			if sink.failure != "" {
				t.Errorf("unexpected run failure: %s", sink.failure)
			}
			joined := strings.Join(sink.vias, ",")
			if !strings.Contains(joined, "comment_handler_done->failed via failure") {
				t.Errorf("transitions %q; want the declared failure arm taken with via=failure", joined)
			}
			if strings.Contains(joined, "comment_handler_done->set_review_state via success") {
				t.Errorf("transitions %q; the success arm must not be taken for a failed step (the old viaOutcome=success laundering)", joined)
			}
			ran := strings.Join(sink.stepsRun, ",")
			if strings.Contains(ran, "set_done_state") {
				t.Errorf("steps run %q; the success-arm bookkeeping must not execute after the failure", ran)
			}
		})
	}
}

// cri130RecoveryWorkflow models the fixed contract on a soft tail: a
// comment step whose declared failure outcome routes to a recovery step
// (restate_ticket) on the same adapter session, followed by the recovered
// and failed terminals.
func cri130RecoveryWorkflow() string {
	return `
workflow {
  name = "linear_intake_cri130"
  version = "0.1"
  initial_state = "merge_pr"
  target_state  = "done"
}
step "merge_pr" {
  target = adapter.fake
  outcome "success" { next = step.comment_handler_done }
}
step "comment_handler_done" {
  target = adapter.fake
  outcome "success" { next = state.done }
  outcome "failure" { next = step.restate_ticket }
}
step "restate_ticket" {
  target = adapter.fake
  outcome "success" { next = state.recovered }
  outcome "failure" { next = state.failed }
}
state "done" { terminal = true }
state "recovered" { terminal = true }
state "failed" {
  terminal = true
  success  = false
}`
}

// TestCRI130_CommentDeclaredFailureTakesRecoveryBranch: a comment step may
// declare its failure outcome to a recovery branch. The recovery branch is
// taken, not the success arm — the graph is the only policy surface.
func TestCRI130_CommentDeclaredFailureTakesRecoveryBranch(t *testing.T) {
	g := compile(t, cri130RecoveryWorkflow())
	p := &cri130Adapter{
		fakeAdapter: &fakeAdapter{name: "fake", outcome: "success"},
		failStep:    "comment_handler_done",
		failMode:    "failure",
	}
	sink := &fakeSink{}
	if err := NewTestEngine(g, cri130NewLoader(p), sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if sink.terminal != "recovered" || !sink.terminalOK {
		t.Errorf("terminal state %q success=%v; want recovered/true (declared recovery branch)", sink.terminal, sink.terminalOK)
	}
	joined := strings.Join(sink.transitions, ",")
	if !strings.Contains(joined, "comment_handler_done->restate_ticket") {
		t.Errorf("transitions %q; want the declared failure arm to the recovery branch", joined)
	}
	if strings.Contains(joined, "comment_handler_done->done") {
		t.Errorf("transitions %q; the success arm must not be taken for a failed comment step", joined)
	}
	ran := strings.Join(sink.stepsRun, ",")
	if !strings.Contains(ran, "restate_ticket") {
		t.Errorf("steps run %q; the recovery branch must execute", ran)
	}
}

// TestCRI130_RoutingIndependentOfStepName pins the layering fix: the engine
// must route by the declared graph, never by the step name. Two otherwise
// identical workflows — one with the comment_-prefixed name, one with any
// other name — must produce the same terminal, the same transitions (modulo
// the step name), and the same success flag.
func TestCRI130_RoutingIndependentOfStepName(t *testing.T) {
	const tpl = `
workflow {
  name = "linear_intake_cri130"
  version = "0.1"
  initial_state = "merge_pr"
  target_state  = "handler_complete"
}
step "merge_pr" {
  target = adapter.fake
  outcome "success" { next = step.%[1]s }
}
step "%[1]s" {
  target = adapter.fake
  outcome "success" { next = step.set_review_state }
  outcome "failure" { next = state.failed }
}
step "set_review_state" {
  target = adapter.fake
  outcome "success" { next = state.handler_complete }
  outcome "failure" { next = state.failed }
}
state "handler_complete" { terminal = true }
state "failed" {
  terminal = true
  success  = false
}`
	byName := map[string]string{}
	for _, stepName := range []string{"comment_handler_done", "finalize_bookkeeping"} {
		t.Run(stepName, func(t *testing.T) {
			g := compile(t, fmt.Sprintf(tpl, stepName))
			p := &cri130Adapter{
				fakeAdapter: &fakeAdapter{name: "fake", outcome: "success"},
				failStep:    stepName,
				failMode:    "failure",
			}
			sink := &fakeSink{}
			if err := NewTestEngine(g, cri130NewLoader(p), sink).Run(context.Background()); err != nil {
				t.Fatalf("run: %v", err)
			}
			if sink.terminal != "failed" || sink.terminalOK {
				t.Errorf("terminal state %q success=%v; want failed/false (declared failure arm, name-independent)", sink.terminal, sink.terminalOK)
			}
			got := make([]string, 0, len(sink.transitions))
			for _, tr := range sink.transitions {
				got = append(got, strings.Replace(tr, stepName, "FAILSTEP", 1))
			}
			byName[stepName] = strings.Join(got, ",")
		})
	}
	if byName["comment_handler_done"] != byName["finalize_bookkeeping"] {
		t.Errorf("routing depends on the step name: comment-prefixed %q vs plain %q", byName["comment_handler_done"], byName["finalize_bookkeeping"])
	}
}

// TestCRI130_CommentCrashRecoveryRunsOnReopenedSession proves the CRI-271
// session repair now serves comment steps: the comment step crashes its
// session mid-run and the workflow declares the comment step's failure
// outcome to a recovery step (restate_ticket) on the SAME adapter reference.
// The recovery step is re-opened onto a fresh session (a second OpenSession)
// and executes FOR REAL, reaching the recovered terminal. Under the deleted
// mechanism the comment crash was laundered to its success arm (state.done)
// and the recovery branch could never run.
func TestCRI130_CommentCrashRecoveryRunsOnReopenedSession(t *testing.T) {
	g := compile(t, cri130RecoveryWorkflow())
	p := newCri271Adapter("comment_handler_done")
	sink := &fakeSink{}
	if err := NewTestEngine(g, cri271NewLoader(p), sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if sink.terminal != "recovered" || !sink.terminalOK {
		t.Errorf("terminal state %q success=%v; want recovered/true (declared recovery branch on the re-opened session)", sink.terminal, sink.terminalOK)
	}
	if sink.failure != "" {
		t.Errorf("unexpected run failure: %s", sink.failure)
	}
	opens, executes := p.callLog()
	// The crashed comment session is re-opened before the recovery step runs.
	wantOpens := []string{"fake.default", "fake.default"}
	if len(opens) != len(wantOpens) {
		t.Fatalf("OpenSession calls: %v; want %v (the re-open is name-agnostic)", opens, wantOpens)
	}
	wantExecutes := []string{"merge_pr", "comment_handler_done", "restate_ticket"}
	if len(executes) != len(wantExecutes) {
		t.Fatalf("Execute calls: %v; want %v", executes, wantExecutes)
	}
	for i, want := range wantExecutes {
		if executes[i] != want {
			t.Errorf("Execute calls[%d] = %q; want %q", i, executes[i], want)
		}
	}
	joined := strings.Join(sink.transitions, ",")
	if !strings.Contains(joined, "comment_handler_done->restate_ticket") {
		t.Errorf("transitions %q; want the declared failure arm taken to the recovery branch", joined)
	}
	if strings.Contains(joined, "comment_handler_done->done") {
		t.Errorf("transitions %q; the success arm must not be taken (no laundered continuation)", joined)
	}
	if !strings.Contains(joined, "restate_ticket->recovered") {
		t.Errorf("transitions %q; want the recovery step to complete on the re-opened session", joined)
	}
}

// TestCRI130_CommentCrashRecoveryWithReopenDisabledFailsRun pins the reopen
// kill switch against the recovery shape: with CRITERIA_SESSION_CRASH_REOPEN
// disabled the crashed session stays dead, so the recovery step replays the
// crash error and its OWN declared failure outcome routes to the failed
// terminal. No laundering: under the deleted mechanism the comment crash was
// rewritten to success and the run ended at done with success=true.
func TestCRI130_CommentCrashRecoveryWithReopenDisabledFailsRun(t *testing.T) {
	t.Setenv("CRITERIA_SESSION_CRASH_REOPEN", "0")
	g := compile(t, cri130RecoveryWorkflow())
	p := newCri271Adapter("comment_handler_done")
	sink := &fakeSink{}
	if err := NewTestEngine(g, cri271NewLoader(p), sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v (the run ends at the failed terminal, not with an engine error)", err)
	}
	if sink.terminal != "failed" || sink.terminalOK {
		t.Errorf("terminal state %q success=%v; want failed/false (restate_ticket's declared failure arm on the dead session)", sink.terminal, sink.terminalOK)
	}
	opens, executes := p.callLog()
	if len(opens) != 1 {
		t.Errorf("OpenSession calls: %v; want exactly the initial open (re-open disabled)", opens)
	}
	wantExecutes := []string{"merge_pr", "comment_handler_done", "restate_ticket"}
	if len(executes) != len(wantExecutes) {
		t.Fatalf("Execute calls: %v; want %v", executes, wantExecutes)
	}
	joined := strings.Join(sink.transitions, ",")
	if !strings.Contains(joined, "comment_handler_done->restate_ticket") {
		t.Errorf("transitions %q; want the comment step's declared failure arm taken", joined)
	}
	if !strings.Contains(joined, "restate_ticket->failed") {
		t.Errorf("transitions %q; want the recovery step routed via its declared failure outcome", joined)
	}
	if strings.Contains(joined, "restate_ticket->recovered") || strings.Contains(joined, "comment_handler_done->done") {
		t.Errorf("transitions %q; no step may reach a success arm after the crash replay", joined)
	}
}

// TestCRI130_CrashExhaustionFailsRunWhenNoFailureArm: a step that declares no
// "failure" outcome and crashes has nowhere to route; after the attempts are
// exhausted the run fails with the wrapped error. Under the deleted mechanism
// the exhaustion was rewritten into a success continuation.
func TestCRI130_CrashExhaustionFailsRunWhenNoFailureArm(t *testing.T) {
	g := compile(t, `
workflow {
  name = "linear_intake_cri130"
  version = "0.1"
  initial_state = "merge_pr"
  target_state  = "done"
}
step "merge_pr" {
  target = adapter.fake
  outcome "success" { next = step.comment_handler_done }
}
step "comment_handler_done" {
  target = adapter.fake
  outcome "success" { next = state.done }
}
state "done" { terminal = true }`)
	p := &cri130Adapter{
		fakeAdapter: &fakeAdapter{name: "fake", outcome: "success"},
		failStep:    "comment_handler_done",
		failMode:    "crash",
	}
	sink := &fakeSink{}
	err := NewTestEngine(g, cri130NewLoader(p), sink).Run(context.Background())
	if err == nil {
		t.Fatal("expected the run to fail when the crashed step has no declared failure outcome")
	}
	if !strings.Contains(err.Error(), "step \"comment_handler_done\" failed after 1 attempts") {
		t.Errorf("unexpected error: %v", err)
	}
	if !strings.Contains(err.Error(), cri130CrashErr) {
		t.Errorf("error must carry the observed crash signature; got: %v", err)
	}
	if sink.failure == "" {
		t.Error("expected OnRunFailed for the crashed step with no declared failure outcome")
	}
	if sink.terminal != "" {
		t.Errorf("terminal %q; the failed run must not report a terminal", sink.terminal)
	}
}

// TestCRI130_UnroutedFailureOutcomeFailsRun: a clean failure outcome from a
// step that declares neither a "failure" outcome nor a default has no place
// in the graph to route; the engine must refuse (the unmapped-outcome guard)
// instead of continuing as if the step succeeded. Under the deleted mechanism
// the clean failure was rewritten to success.
func TestCRI130_UnroutedFailureOutcomeFailsRun(t *testing.T) {
	g := compile(t, `
workflow {
  name = "linear_intake_cri130"
  version = "0.1"
  initial_state = "merge_pr"
  target_state  = "done"
}
step "merge_pr" {
  target = adapter.fake
  outcome "success" { next = step.comment_handler_done }
}
step "comment_handler_done" {
  target = adapter.fake
  outcome "success" { next = state.done }
}
state "done" { terminal = true }`)
	p := &cri130Adapter{
		fakeAdapter: &fakeAdapter{name: "fake", outcome: "success"},
		failStep:    "comment_handler_done",
		failMode:    "failure",
	}
	sink := &fakeSink{}
	err := NewTestEngine(g, cri130NewLoader(p), sink).Run(context.Background())
	if err == nil {
		t.Fatal("expected the run to fail on the unmapped failure outcome")
	}
	if !strings.Contains(err.Error(), `produced unmapped outcome "failure"`) {
		t.Errorf("unexpected error: %v", err)
	}
	if sink.failure == "" {
		t.Error("expected OnRunFailed for the unmapped failure outcome")
	}
}

// A comment step may declare only its failure outcome (e.g. a comment_invalid
// tail that reports the invalid condition and stops). The declared routing
// applies — the comment step's name buys no continuation.
func TestCRI130_CommentStepWithoutSuccessTransitionKeepsRouting(t *testing.T) {
	g := compile(t, `
workflow {
  name = "linear_intake_cri130"
  version = "0.1"
  initial_state = "merge_pr"
  target_state  = "done"
}
step "merge_pr" {
  target = adapter.fake
  outcome "success" { next = step.comment_invalid }
}
step "comment_invalid" {
  target = adapter.fake
  outcome "failure" { next = state.failed }
}
state "done" { terminal = true }
state "failed" {
  terminal = true
  success  = false
}`)
	p := &cri130Adapter{
		fakeAdapter: &fakeAdapter{name: "fake", outcome: "success"},
		failStep:    "comment_invalid",
		failMode:    "crash",
	}
	sink := &fakeSink{}
	if err := NewTestEngine(g, cri130NewLoader(p), sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if sink.terminal != "failed" || sink.terminalOK {
		t.Errorf("terminal state %q success=%v; want failed/false (declared failure routing preserved)", sink.terminal, sink.terminalOK)
	}
}

// An explicit on_crash=abort_run is author intent: the run must still fail.
func TestCRI130_CommentStepAbortRunStillFatal(t *testing.T) {
	g := compile(t, `
workflow {
  name = "linear_intake_cri130"
  version = "0.1"
  initial_state = "merge_pr"
  target_state  = "done"
}
step "merge_pr" {
  target = adapter.fake
  outcome "success" { next = step.comment_handler_done }
}
step "comment_handler_done" {
  target = adapter.fake
  on_crash = "abort_run"
  outcome "success" { next = state.done }
  outcome "failure" { next = state.failed }
}
state "done" { terminal = true }
state "failed" {
  terminal = true
  success  = false
}`)
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

// Non-comment steps are unaffected: a session crash at any step routes via
// its declared failure outcome (and the CRI-271 re-open repair works the same
// way).
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

// TestCRI130_FunctionalFailureBranchNotReportedSuccess: run_handler fails
// functionally (clean failure outcome, session alive), the declared failure
// arm routes to the notification comment step, and the comment step CRASHES.
// The comment step's own declared failure arm applies — the run ends at the
// failed terminal and the success arm (set_review_state → awaiting_human)
// never executes: a functionally failed run must never reach a success
// terminal.
func TestCRI130_FunctionalFailureBranchNotReportedSuccess(t *testing.T) {
	g := compile(t, `
workflow {
  name = "linear_intake_cri130"
  version = "0.1"
  initial_state = "run_handler"
  target_state  = "awaiting_human"
}
step "run_handler" {
  target = adapter.fake
  outcome "success" { next = state.handler_complete }
  outcome "failure" { next = step.comment_handler_failed }
}
step "comment_handler_failed" {
  target = adapter.fake
  outcome "success" { next = step.set_review_state }
  outcome "failure" { next = state.failed }
}
step "set_review_state" {
  target = adapter.fake
  outcome "success" { next = state.awaiting_human }
  outcome "failure" { next = state.failed }
}
state "handler_complete" { terminal = true }
state "awaiting_human" { terminal = true }
state "failed" {
  terminal = true
  success  = false
}`)
	p := &cri130Adapter{
		fakeAdapter: &fakeAdapter{name: "fake", outcome: "success"},
		failStep:    "run_handler",
		failMode:    "failure",
		crashStep:   "comment_handler_failed",
	}
	sink := &fakeSink{}
	if err := NewTestEngine(g, cri130NewLoader(p), sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if sink.terminal != "failed" || sink.terminalOK {
		t.Errorf("terminal state %q success=%v; want failed/false (a functionally failed run must not reach a success terminal)", sink.terminal, sink.terminalOK)
	}
	joined := strings.Join(sink.transitions, ",")
	if !strings.Contains(joined, "run_handler->comment_handler_failed") {
		t.Errorf("transitions %q; want run_handler's declared failure arm taken", joined)
	}
	if !strings.Contains(joined, "comment_handler_failed->failed") {
		t.Errorf("transitions %q; want the comment step's declared failure arm taken after its crash", joined)
	}
	if strings.Contains(joined, "comment_handler_failed->set_review_state") {
		t.Errorf("transitions %q; the crashed comment step's success arm must not be taken (no viaOutcome=success laundering)", joined)
	}
	ran := strings.Join(sink.stepsRun, ",")
	if strings.Contains(ran, "set_review_state") {
		t.Errorf("steps run %q; the success-arm bookkeeping must not execute after the comment crash", ran)
	}
}

// TestCRI130_NonTailFollowOnStillFailsRun: mid-workflow, a comment step that
// crashes with a declared failure arm routes through that arm to the failed
// terminal — the success-arm follow-ons (set_done_state, finalize_report)
// never execute and the run fails for real instead of silently skipping
// forward or laundering the crash into success.
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
		t.Errorf("terminal state %q success=%v; want failed/false (follow-on failure must keep genuine routing)", sink.terminal, sink.terminalOK)
	}
	if got := strings.Join(sink.transitions, ","); !strings.Contains(got, "comment_progress->failed") {
		t.Errorf("transitions %q; want comment_progress to route via its failure outcome (no silent downgrade)", got)
	}
	if ran := strings.Join(sink.stepsRun, ","); strings.Contains(ran, "set_done_state") {
		t.Errorf("steps run %q; the success-arm follow-ons must not execute after the declared failure routing", ran)
	}
}
