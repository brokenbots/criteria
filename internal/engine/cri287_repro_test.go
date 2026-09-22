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
// session.crash classification events.
type cri287Sink struct {
	*fakeSink

	mu       sync.Mutex
	outcomes []cri287Outcome
	events   []cri271Event
}

type cri287Outcome struct {
	step    string
	outcome string
	err     string
}

func (s *cri287Sink) OnStepOutcome(step, outcome string, _ time.Duration, err error) {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	s.mu.Lock()
	s.outcomes = append(s.outcomes, cri287Outcome{step: step, outcome: outcome, err: msg})
	s.mu.Unlock()
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
	if err := NewTestEngine(g, loader, sink).Run(context.Background()); err != nil {
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
