package engine

// cri287_repro_test.go — regression tests for CRI-287: an engine-initiated
// step-timeout cancellation (the CRI-275 step ceiling) tears down sibling
// phone-home transports in the same second as the canceled Execute stream.
// The transport closes observed on the follow-on steps (the failure-route
// bookkeeping and the checkpoint loop) are the consequence of that
// cancellation, not an adapter death, and must be routed as the step's
// declared failure/default outcomes (the checkpoint loop) instead of being
// classified as `session crashed` and terminating the run.
//
// The incident (run 74cf49b7): the develop step (copilot adapter) ran silent
// past its 60m step timeout; at +60m the engine canceled it and both shell
// sessions' transports (shell.sh, shell.develop) closed in the same second
// during teardown. The next step's Execute (fresh context) saw the dead
// transport and was crash-classified
// (`crash_reason="gRPC client transport closed (adapter or shim closed the
// connection)"`), so the run ended `failed` instead of re-entering the
// checkpoint loop (push_checkpoint -> develop resumes from the last pushed
// commit).

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	criteriav2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow"
)

// cri287Workflow models the linear_develop_v1 checkpoint loop: the develop
// step (copilot adapter) carries the CRI-275 step timeout; its declared
// failure outcome routes into the bookkeeping tail (comment_handler_failed)
// and the checkpoint loop (push_checkpoint -> develop), which resumes the
// developer from the last pushed commit.
const cri287Workflow = `
workflow {
  name = "cri287"
  version = "0.1"
  initial_state = "boot"
  target_state  = "done"
}
adapter "copilot" "default" {}
adapter "shell" "default" {}
step "boot" {
  target = adapter.shell.default
  outcome "success" { next = step.develop }
  outcome "failure" { next = step.develop }
}
step "develop" {
  target = adapter.copilot.default
  timeout = "150ms"
  outcome "success" { next = step.push_checkpoint }
  outcome "failure" { next = step.comment_handler_failed }
}
step "comment_handler_failed" {
  target = adapter.shell.default
  outcome "success" { next = step.push_checkpoint }
  outcome "failure" { next = step.push_checkpoint }
}
step "push_checkpoint" {
  target = adapter.shell.default
  outcome "success" { next = step.develop }
  outcome "failure" { next = step.develop }
  outcome "done"    { next = state.done }
}
state "done" {
  terminal = true
  success  = true
}`

// cri287Shared models the shared phone-home connection the copilot and shell
// shims ride on in production (same pod): the step-timeout teardown cascade
// closes it, and the copilot adapter's re-dial on the next turn
// re-establishes it (CRI-274 re-handshake). developTurns counts the copilot
// turns that completed successfully, bounding the modeled checkpoint loop.
type cri287Shared struct {
	mu           sync.Mutex
	up           bool
	developTurns int
}

func (s *cri287Shared) setUp(up bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.up = up
}

func (s *cri287Shared) isUp() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.up
}

func (s *cri287Shared) completedTurn() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.developTurns++
}

func (s *cri287Shared) turns() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.developTurns
}

// cri287Copilot models the develop step's adapter: the first turn is silent
// and is killed by the engine's step timeout (Execute unblocks on context
// cancellation and returns the context error, exactly as the production
// go-plugin transport does). The cancellation tears down the shared
// phone-home connection in the same second. The next turn re-dials
// (connection healed) and succeeds — the developer resumed from the last
// pushed commit.
type cri287Copilot struct {
	shared *cri287Shared

	mu    sync.Mutex
	opens []string
	turns int
}

func (a *cri287Copilot) Info(context.Context) (adapterhost.Info, error) {
	return adapterhost.Info{Name: "copilot", Version: "test"}, nil
}

func (a *cri287Copilot) OpenSession(_ context.Context, name string, _, _ map[string]string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.opens = append(a.opens, name)
	a.shared.setUp(true)
	return nil
}

func (a *cri287Copilot) Execute(ctx context.Context, _ string, _ *workflow.StepNode, _ adapter.EventSink) (adapter.Result, error) {
	a.mu.Lock()
	a.turns++
	turn := a.turns
	a.mu.Unlock()
	if turn == 1 {
		// The developer turn is silent: the engine's step timeout cancels the
		// Execute stream. The teardown cascade closes the shared phone-home
		// connection in the same second.
		<-ctx.Done()
		a.shared.setUp(false)
		return adapter.Result{}, ctx.Err()
	}
	// The resumed turn re-dials the phone-home transport (CRI-274) and
	// completes: the developer pushed a checkpoint and finished the turn.
	a.shared.setUp(true)
	a.shared.completedTurn()
	return adapter.Result{Outcome: "success"}, nil
}

func (a *cri287Copilot) CloseSession(context.Context, string) error { return nil }
func (a *cri287Copilot) Kill()                                      {}
func (a *cri287Copilot) Pause(context.Context, string) error        { return nil }
func (a *cri287Copilot) Resume(context.Context, string) error       { return nil }
func (a *cri287Copilot) Inspect(context.Context, string) (*criteriav2.InspectResponse, error) {
	return &criteriav2.InspectResponse{}, nil
}
func (a *cri287Copilot) Snapshot(context.Context, string) (*criteriav2.SnapshotResponse, error) {
	return &criteriav2.SnapshotResponse{}, nil
}
func (a *cri287Copilot) Restore(context.Context, string, []byte, uint32) error { return nil }

func (a *cri287Copilot) openLog() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.opens...)
}

// cri287Shell models the shell bookkeeping steps riding the shared
// phone-home connection: while the connection is down (step-timeout teardown
// cascade) every Execute replays the transport error — the exact observed
// signature from run 74cf49b7. Once the copilot adapter re-dials, the shell
// transport is healthy again. push_checkpoint returns the "done" outcome
// once the developer has completed two turns, bounding the checkpoint loop.
type cri287Shell struct {
	shared *cri287Shared

	mu    sync.Mutex
	opens []string
}

func (a *cri287Shell) Info(context.Context) (adapterhost.Info, error) {
	return adapterhost.Info{Name: "shell", Version: "test"}, nil
}

func (a *cri287Shell) OpenSession(_ context.Context, name string, _, _ map[string]string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.opens = append(a.opens, name)
	// A fresh shell shim re-establishes its phone-home transport.
	a.shared.setUp(true)
	return nil
}

func (a *cri287Shell) Execute(_ context.Context, _ string, step *workflow.StepNode, _ adapter.EventSink) (adapter.Result, error) {
	if !a.shared.isUp() {
		// The shared phone-home transport is down: the transport close is
		// replayed verbatim, exactly as the production shim did during the
		// teardown cascade.
		return adapter.Result{}, errors.New(cri271CrashErr)
	}
	if step != nil && step.Name == "push_checkpoint" && a.shared.turns() >= 2 {
		return adapter.Result{Outcome: "done"}, nil
	}
	return adapter.Result{Outcome: "success"}, nil
}

func (a *cri287Shell) CloseSession(context.Context, string) error { return nil }
func (a *cri287Shell) Kill()                                      {}
func (a *cri287Shell) Pause(context.Context, string) error        { return nil }
func (a *cri287Shell) Resume(context.Context, string) error       { return nil }
func (a *cri287Shell) Inspect(context.Context, string) (*criteriav2.InspectResponse, error) {
	return &criteriav2.InspectResponse{}, nil
}
func (a *cri287Shell) Snapshot(context.Context, string) (*criteriav2.SnapshotResponse, error) {
	return &criteriav2.SnapshotResponse{}, nil
}
func (a *cri287Shell) Restore(context.Context, string, []byte, uint32) error { return nil }

func (a *cri287Shell) openLog() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.opens...)
}

// cri287Sink extends fakeSink with OnStepOutcome recording (step, outcome,
// error text) and a session-level adapter event recorder so tests can assert
// both the CRI-275 timeout signature on the develop step and the absence of
// session.crash classification events. When `engine` is set, every
// observation also records whether the CRI-287 step-timeout teardown window
// was open at that moment (read via the engine's live session manager).
type cri287Sink struct {
	*fakeSink

	engine *Engine

	mu              sync.Mutex
	outcomes        []cri287Outcome
	events          []cri271Event
	windowAtRunFail bool
	sawRunFail      bool
}

type cri287Outcome struct {
	step       string
	outcome    string
	err        string
	windowOpen bool
}

// teardownWindowOpen reads the CRI-287 teardown-window state from the run's
// live session manager. All sink callbacks fire from the run goroutine, but
// the live pointer is swapped under the engine mutex, so read it guarded.
func (s *cri287Sink) teardownWindowOpen() bool {
	s.mu.Lock()
	engine := s.engine
	s.mu.Unlock()
	if engine == nil {
		return false
	}
	engine.mu.RLock()
	sessions := engine.liveSessions
	engine.mu.RUnlock()
	if sessions == nil {
		return false
	}
	return sessions.StepTimeoutTeardownWindowOpen()
}

func (s *cri287Sink) OnStepOutcome(step, outcome string, _ time.Duration, err error) {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	open := s.teardownWindowOpen()
	s.mu.Lock()
	s.outcomes = append(s.outcomes, cri287Outcome{step: step, outcome: outcome, err: msg, windowOpen: open})
	s.mu.Unlock()
}

func (s *cri287Sink) OnRunFailed(reason, step string) {
	open := s.teardownWindowOpen()
	s.mu.Lock()
	s.windowAtRunFail = open
	s.sawRunFail = true
	s.mu.Unlock()
	s.fakeSink.OnRunFailed(reason, step)
}

func (s *cri287Sink) StepEventSink(string) adapter.EventSink {
	return &cri287EventRecorder{parent: s}
}

type cri287EventRecorder struct {
	parent *cri287Sink
}

func (r *cri287EventRecorder) Log(string, []byte) {}
func (r *cri287EventRecorder) Adapter(kind string, data any) {
	payload, _ := data.(map[string]any)
	r.parent.mu.Lock()
	defer r.parent.mu.Unlock()
	r.parent.events = append(r.parent.events, cri271Event{kind: kind, data: payload})
}

func (s *cri287Sink) recorded(kind string) (map[string]any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, evt := range s.events {
		if evt.kind == kind {
			return evt.data, true
		}
	}
	return nil, false
}

func (s *cri287Sink) eventCount(kind string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, evt := range s.events {
		if evt.kind == kind {
			n++
		}
	}
	return n
}

// TestCRI287_StepTimeoutTeardownReentersCheckpointLoop is the CRI-287
// regression test: the develop step's command is silent past its step
// timeout; the engine cancels the turn (CRI-275 ceiling) and the teardown
// cascade closes the sibling shell transports. The follow-on steps'
// transport closes must be routed as the step's declared failures — the
// checkpoint loop re-enters develop (resumed from the last pushed commit)
// and the run completes — with no `session crashed` classification, no
// session.crash event, and no crash-driven respawn of the shell session.
func TestCRI287_StepTimeoutTeardownReentersCheckpointLoop(t *testing.T) {
	g := compile(t, cri287Workflow)
	shared := &cri287Shared{up: true}
	copilot := &cri287Copilot{shared: shared}
	shell := &cri287Shell{shared: shared}
	sink := &cri287Sink{fakeSink: &fakeSink{}}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"copilot":         copilot,
		"copilot.default": copilot,
		"shell":           shell,
		"shell.default":   shell,
	}}
	e := NewTestEngine(g, loader, sink)
	sink.engine = e
	if err := e.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if sink.terminal != "done" || !sink.terminalOK {
		t.Fatalf("terminal state %q success=%v; want done/true", sink.terminal, sink.terminalOK)
	}
	if sink.failure != "" {
		t.Errorf("OnRunFailed: %q; want none (the checkpoint loop, not a run failure)", sink.failure)
	}

	// The timed-out develop step carries the CRI-275 outcome: failure with
	// the context deadline as the error, routed into the declared failure
	// outcome (the checkpoint loop), not into crash machinery.
	var developFailure *cri287Outcome
	for i := range sink.outcomes {
		o := &sink.outcomes[i]
		if o.step == "develop" && o.outcome == "failure" {
			developFailure = o
			break
		}
	}
	if developFailure == nil {
		t.Fatalf("develop outcomes: %+v; want a declared failure outcome", sink.outcomes)
	}
	if !strings.Contains(developFailure.err, "context deadline exceeded") {
		t.Errorf("develop failure error = %q; want the CRI-275 context deadline signature", developFailure.err)
	}

	// The engine marked the teardown between the develop step's outcome and
	// the follow-on Execute: the follow-on bookkeeping outcome observed the
	// open teardown window (CRI-287 mark-before-next-Execute ordering).
	var commentOutcome *cri287Outcome
	for i := range sink.outcomes {
		o := &sink.outcomes[i]
		if o.step == "comment_handler_failed" && o.outcome == "failure" {
			commentOutcome = o
			break
		}
	}
	if commentOutcome == nil {
		t.Fatalf("comment_handler_failed outcomes: %+v; want a declared failure outcome", sink.outcomes)
	}
	if !commentOutcome.windowOpen {
		t.Errorf("comment_handler_failed outcome observed windowOpen=false; want the step-timeout teardown window open")
	}

	// No session.crash classification: the transport closes during the
	// teardown window are routed as timeouts, not crashes.
	if n := sink.eventCount("session.crash"); n != 0 {
		t.Errorf("session.crash events: %d; want 0 for a step-timeout teardown cascade", n)
	}

	// No crash machinery side effects: the shell session was never marked
	// crashed, so no re-open/respawn spawned a second shell process.
	if opens := shell.openLog(); len(opens) != 1 {
		t.Errorf("shell OpenSession calls: %v; want exactly the initial open (no respawn)", opens)
	}
	if opens := copilot.openLog(); len(opens) != 1 {
		t.Errorf("copilot OpenSession calls: %v; want exactly the initial open", opens)
	}

	// The checkpoint loop re-entered develop: comment_handler_failed and
	// push_checkpoint ran between the timed-out develop turn and the resumed
	// one — the resumed turn starts from the last pushed commit. The boot
	// step ran first so the shell session was already live when the develop
	// turn was torn down (as in the incident).
	steps := sink.stepsRun
	if len(steps) < 5 {
		t.Fatalf("steps run: %v; want the checkpoint loop between develop turns", steps)
	}
	if steps[0] != "boot" {
		t.Errorf("steps[0] = %q; want the bootstrap step that opens the shell session", steps[0])
	}
	if steps[1] != "develop" {
		t.Errorf("steps[1] = %q; want the timed-out develop turn", steps[1])
	}
	if steps[2] != "comment_handler_failed" {
		t.Errorf("steps[2] = %q; want the failure-route bookkeeping step", steps[2])
	}
	if steps[3] != "push_checkpoint" {
		t.Errorf("steps[3] = %q; want the checkpoint step before develop resumes", steps[3])
	}
	if steps[4] != "develop" {
		t.Errorf("steps[4] = %q; want develop re-entered from the checkpoint loop", steps[4])
	}
}

// TestCRI287_GenuineAdapterDeathStillCrashClassified pins the contrast: the
// same transport error on a step WITHOUT an engine-initiated step timeout (a
// genuine adapter death outside any cancellation) keeps the CRI-271
// hard-failure classification — the session.crash event is emitted with the
// classified reason.
func TestCRI287_GenuineAdapterDeathStillCrashClassified(t *testing.T) {
	g := compile(t, strings.Replace(cri287Workflow, "  timeout = \"150ms\"\n", "", 1))
	shared := &cri287Shared{up: true}
	copilot := &cri287CopilotDying{shared: shared}
	shell := &cri287Shell{shared: shared}
	sink := &cri287Sink{fakeSink: &fakeSink{}}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"copilot":         copilot,
		"copilot.default": copilot,
		"shell":           shell,
		"shell.default":   shell,
	}}
	if err := NewTestEngine(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if sink.terminal != "done" || !sink.terminalOK {
		t.Fatalf("terminal state %q success=%v; want done/true", sink.terminal, sink.terminalOK)
	}

	// A genuine adapter death (no engine-initiated cancellation) is still
	// classified as a session crash, with the classified reason.
	data, ok := sink.recorded("session.crash")
	if !ok {
		t.Fatal("expected a session.crash event for a genuine adapter death")
	}
	if data["session"] != "copilot.default" {
		t.Errorf("session.crash session=%v; want copilot.default", data["session"])
	}
	if reason, _ := data["crash_reason"].(string); reason != "gRPC client transport closed (adapter or shim closed the connection)" {
		t.Errorf("crash_reason=%q; want the classified transport-close cause", reason)
	}
}

// cri287CopilotDying models a genuine mid-turn adapter death with no step
// timeout involved: the first turn loses the transport immediately (the step
// context is still live), so the death is outside any engine-initiated
// cancellation.
type cri287CopilotDying struct {
	shared *cri287Shared

	mu    sync.Mutex
	opens []string
	turns int
}

func (a *cri287CopilotDying) Info(context.Context) (adapterhost.Info, error) {
	return adapterhost.Info{Name: "copilot", Version: "test"}, nil
}

func (a *cri287CopilotDying) OpenSession(_ context.Context, name string, _, _ map[string]string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.opens = append(a.opens, name)
	a.shared.setUp(true)
	return nil
}

func (a *cri287CopilotDying) Execute(_ context.Context, _ string, _ *workflow.StepNode, _ adapter.EventSink) (adapter.Result, error) {
	a.mu.Lock()
	a.turns++
	turn := a.turns
	a.mu.Unlock()
	if turn == 1 {
		// The shim connection drops mid-turn while the step context is still
		// live: a genuine adapter death, not a teardown cancellation.
		a.shared.setUp(false)
		return adapter.Result{}, errors.New(cri271CrashErr)
	}
	a.shared.setUp(true)
	a.shared.completedTurn()
	return adapter.Result{Outcome: "success"}, nil
}

func (a *cri287CopilotDying) CloseSession(context.Context, string) error { return nil }
func (a *cri287CopilotDying) Kill()                                      {}
func (a *cri287CopilotDying) Pause(context.Context, string) error        { return nil }
func (a *cri287CopilotDying) Resume(context.Context, string) error       { return nil }
func (a *cri287CopilotDying) Inspect(context.Context, string) (*criteriav2.InspectResponse, error) {
	return &criteriav2.InspectResponse{}, nil
}
func (a *cri287CopilotDying) Snapshot(context.Context, string) (*criteriav2.SnapshotResponse, error) {
	return &criteriav2.SnapshotResponse{}, nil
}
func (a *cri287CopilotDying) Restore(context.Context, string, []byte, uint32) error { return nil }

// TestCRI287_ParentDeadlineExpiryDoesNotOpenTeardownWindow pins the mark
// precondition (CRI-287 review): the engine only opens the step-timeout
// teardown window when it installed a CRI-275 step ceiling. When the develop
// step has no timeout (step.Timeout == 0) and the PARENT context's deadline
// expires mid-turn, the canceled turn must not mark the session manager — the
// follow-on bookkeeping step's Execute must observe a closed window, so a
// transport close there keeps the hard-failure crash classification.
func TestCRI287_ParentDeadlineExpiryDoesNotOpenTeardownWindow(t *testing.T) {
	g := compile(t, strings.Replace(cri287Workflow, "  timeout = \"150ms\"\n", "", 1))
	shared := &cri287Shared{up: true}
	copilot := &cri287Copilot{shared: shared}
	shell := &cri287Shell{shared: shared}
	sink := &cri287Sink{fakeSink: &fakeSink{}}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"copilot":         copilot,
		"copilot.default": copilot,
		"shell":           shell,
		"shell.default":   shell,
	}}
	e := NewTestEngine(g, loader, sink)
	sink.engine = e

	// The run context's deadline expires during the silent develop turn 1:
	// the adapter unblocks on context cancellation and the turn fails with
	// the parent deadline — no engine-installed step ceiling was involved.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := e.Run(ctx); err == nil {
		t.Fatal("run: expected the parent-deadline cancellation to fail the run")
	}

	// The run failed before the follow-on steps could Execute (their attempt
	// loop exits on the canceled context), so the discriminating observation
	// is the teardown-window state at the run failure: the engine must never
	// have marked.
	if !sink.sawRunFail {
		t.Fatal("expected OnRunFailed for the parent-deadline cancellation")
	}
	if sink.windowAtRunFail {
		t.Error("teardown window open at run failure; a parent-context deadline must not open the CRI-287 window")
	}
	for _, o := range sink.outcomes {
		if o.windowOpen {
			t.Errorf("outcome %s/%s observed windowOpen=true; no step timeout was installed, so the window must never open", o.step, o.outcome)
		}
	}
	if n := sink.eventCount("session.crash"); n != 0 {
		t.Errorf("session.crash events: %d; want 0 — a run-context deadline expiry is not an adapter crash", n)
	}
}

// TestCRI287_ProcessExitDuringTeardownWindowStillCrashClassified pins the
// process-evidence carve-out end to end (CRI-287 review): with the teardown
// window open (the develop step's CRI-275 ceiling expired and was marked), a
// copilot turn whose adapter process verifiably exited still keeps the
// hard-failure crash classification — the session.crash event carries the
// ProcessExited reason, not a silent timeout routing.
func TestCRI287_ProcessExitDuringTeardownWindowStillCrashClassified(t *testing.T) {
	g := compile(t, cri287ProcessExitWorkflow)
	shared := &cri287Shared{up: true}
	copilot := &cri287ProcessExitCopilot{shared: shared}
	shell := &cri287Shell{shared: shared}
	sink := &cri287Sink{fakeSink: &fakeSink{}}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"copilot":         copilot,
		"copilot.default": copilot,
		"shell":           shell,
		"shell.default":   shell,
	}}
	e := NewTestEngine(g, loader, sink)
	sink.engine = e
	if err := e.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if sink.terminal != "done" || !sink.terminalOK {
		t.Fatalf("terminal state %q success=%v; want done/true (the checkpoint loop resumes)", sink.terminal, sink.terminalOK)
	}

	// The follow-on verify_death turn observed the open teardown window AND
	// still got the crash classification: the process-exit evidence wins
	// over the window.
	var verifyOutcome *cri287Outcome
	for i := range sink.outcomes {
		o := &sink.outcomes[i]
		if o.step == "verify_death" {
			verifyOutcome = o
			break
		}
	}
	if verifyOutcome == nil {
		t.Fatalf("verify_death outcomes: %+v; want the death-verification turn recorded", sink.outcomes)
	}
	if !verifyOutcome.windowOpen {
		t.Errorf("verify_death outcome observed windowOpen=false; want the step-timeout teardown window open")
	}
	if verifyOutcome.outcome != "failure" {
		t.Errorf("verify_death outcome = %q; want the declared failure outcome", verifyOutcome.outcome)
	}

	// The verifiably exited adapter is crash-classified despite the open
	// window: the process-exit reason from classifySessionCrash is carried on
	// the event.
	data, ok := sink.recorded("session.crash")
	if !ok {
		t.Fatal("expected a session.crash event for the verifiably exited adapter")
	}
	if data["session"] != "copilot.default" {
		t.Errorf("session.crash session=%v; want copilot.default", data["session"])
	}
	if reason, _ := data["crash_reason"].(string); reason != "adapter process exited before the call completed" {
		t.Errorf("crash_reason=%q; want the ProcessExited classification", reason)
	}
	if n := sink.eventCount("session.crash"); n != 1 {
		t.Errorf("session.crash events: %d; want exactly 1 (the copilot death; shell transport closes stay timeout-routed)", n)
	}
}

// cri287ProcessExitWorkflow models a develop turn killed by the CRI-275 step
// ceiling whose adapter then verifiably dies (process exit) before the next
// copilot turn: the death must keep the crash classification even though the
// CRI-287 teardown window is open.
const cri287ProcessExitWorkflow = `
workflow {
  name = "cri287_process_exit"
  version = "0.1"
  initial_state = "boot"
  target_state  = "done"
}
adapter "copilot" "default" {}
adapter "shell" "default" {}
step "boot" {
  target = adapter.shell.default
  outcome "success" { next = step.develop }
  outcome "failure" { next = step.develop }
}
step "develop" {
  target = adapter.copilot.default
  timeout = "150ms"
  outcome "success" { next = step.verify_death }
  outcome "failure" { next = step.verify_death }
}
step "verify_death" {
  target = adapter.copilot.default
  outcome "success" { next = step.push_checkpoint }
  outcome "failure" { next = step.push_checkpoint }
}
step "push_checkpoint" {
  target = adapter.shell.default
  outcome "success" { next = step.develop }
  outcome "failure" { next = step.develop }
  outcome "done"    { next = state.done }
}
state "done" {
  terminal = true
  success  = true
}`

// cri287ProcessExitCopilot models the develop adapter across a CRI-275
// ceiling kill followed by a verifiable process death: turn 1 is silent and
// is killed by the step timeout (marking the CRI-287 teardown window) and
// the cancellation closes the shared phone-home connection; turn 2 loses the
// transport with the process already exited — the "adapter is verifiably
// dead" case that must stay crash-classified; the resumed turn re-dials and
// completes.
type cri287ProcessExitCopilot struct {
	shared *cri287Shared

	exited atomic.Bool

	mu    sync.Mutex
	opens []string
	turns int
}

// ProcessExited implements the adapterhost.ProcessExitReporter seam: the
// go-plugin client observed the adapter subprocess exit.
func (a *cri287ProcessExitCopilot) ProcessExited() bool { return a.exited.Load() }

func (a *cri287ProcessExitCopilot) Info(context.Context) (adapterhost.Info, error) {
	return adapterhost.Info{Name: "copilot", Version: "test"}, nil
}

func (a *cri287ProcessExitCopilot) OpenSession(_ context.Context, name string, _, _ map[string]string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.opens = append(a.opens, name)
	a.shared.setUp(true)
	return nil
}

func (a *cri287ProcessExitCopilot) Execute(ctx context.Context, _ string, _ *workflow.StepNode, _ adapter.EventSink) (adapter.Result, error) {
	a.mu.Lock()
	a.turns++
	turn := a.turns
	a.mu.Unlock()
	switch turn {
	case 1:
		// The developer turn is silent past the CRI-275 step ceiling: the
		// engine cancels it and the teardown cascade closes the shared
		// phone-home connection in the same second. The process dies with the
		// turn.
		<-ctx.Done()
		a.exited.Store(true)
		a.shared.setUp(false)
		return adapter.Result{}, ctx.Err()
	case 2:
		// The next turn observes the dead transport AND the exited process:
		// verifiable adapter death inside the open teardown window.
		return adapter.Result{}, errors.New(cri271CrashErr)
	}
	// The resumed turn re-dials (CRI-274) and completes.
	a.shared.setUp(true)
	a.shared.completedTurn()
	return adapter.Result{Outcome: "success"}, nil
}

func (a *cri287ProcessExitCopilot) CloseSession(context.Context, string) error { return nil }
func (a *cri287ProcessExitCopilot) Kill()                                      {}
func (a *cri287ProcessExitCopilot) Pause(context.Context, string) error        { return nil }
func (a *cri287ProcessExitCopilot) Resume(context.Context, string) error       { return nil }
func (a *cri287ProcessExitCopilot) Inspect(context.Context, string) (*criteriav2.InspectResponse, error) {
	return &criteriav2.InspectResponse{}, nil
}
func (a *cri287ProcessExitCopilot) Snapshot(context.Context, string) (*criteriav2.SnapshotResponse, error) {
	return &criteriav2.SnapshotResponse{}, nil
}
func (a *cri287ProcessExitCopilot) Restore(context.Context, string, []byte, uint32) error {
	return nil
}
