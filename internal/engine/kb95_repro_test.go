package engine

// kb95_repro_test.go — regression test for KB-95 / ADR-0008 D2: the parent
// engine treats a criteria-as-adapter (workflow.v1) session as one child run
// anchored behind the adapter session.
//
// The incident semantics being pinned:
//
//   - Re-Execute guard (fail closed): while the child run is in flight
//     (holding the session's execute turn), any overlapping Execute on the
//     same session returns the typed ErrChildRunInFlight — never queued,
//     never a double child run. The guard is enforced by the session
//     manager's execute-turn gate (see internal/adapterhost
//     execute_turn_kb95_test.go for the full gate matrix); here it is driven
//     from inside a live engine run so the engine-level integration is
//     documented.
//   - Crash adoption: when the phone-home transport to the child dies while
//     the run is in flight, the parent step fails with the child's own
//     terminal/crash evidence — the ADR-0007 CrashClassified wire fact
//     delivered through the SupervisedHandle seam — classified as
//     CrashReasonChildRunLost, NOT a bare transport-error string. A surviving
//     child run is adopted from the child's run record on the next session
//     open (internal/adapter/environment/remote peer_workflow_test.go covers
//     the adoption mechanics); at the engine boundary the observable is the
//     typed classification and the session.crash diagnosability.

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

// kb95ChildLossErr is the transport shape observed when the phone-home
// connection to the child dies mid-run: the wrapped loss carries the child
// evidence under it (internal/adapter/environment/remote isPeerTransportLoss).
const kb95ChildLossErr = "connection lost: criteria peer transport lost (child run evidence pending adoption)"

// kb95Workflow is the parent shape: the child-run step declares a failure
// arm, so the run routes through bookkeeping and completes — a lost child
// run is a step-level failure, not a run teardown.
const kb95Workflow = `
workflow {
  name = "kb95"
  version = "0.1"
  initial_state = "run_child"
  target_state  = "done"
}
step "run_child" {
  target = adapter.fake
  outcome "success" { next = state.done }
  outcome "failure" { next = step.collect_outcome }
}
step "collect_outcome" {
  target = adapter.fake
  outcome "success" { next = state.done }
}
state "done" {
  terminal = true
  success  = true
}`

// kb95Shared is the observation hub the re-Execute guard probe writes into:
// the step's Execute is already running (it holds the execute turn), so a
// second overlapping Execute standing for the re-Execute caller resolves
// immediately against the engine's live session manager.
type kb95Shared struct {
	engine *Engine

	mu            sync.Mutex
	guardErr      error
	guardResultOk bool
}

// liveManager mirrors the cri287 accessor: the live pointer is swapped under
// the engine mutex, so read it guarded.
func (s *kb95Shared) liveManager() *adapterhost.SessionManager {
	if s == nil {
		return nil
	}
	s.engine.mu.RLock()
	sessions := s.engine.liveSessions
	s.engine.mu.RUnlock()
	return sessions
}

// reExecuteProbe is the failing step's Execute reaching back into the
// engine's live session manager while it still holds the turn: the probe is
// the overlapping re-Execute (never queued, never a second child run, the
// typed guard reply instead).
func (s *kb95Shared) reExecuteProbe(ctx context.Context, sessionName string, step *workflow.StepNode) {
	mgr := s.liveManager()
	if mgr == nil {
		// No live manager yet: the probe lands as "not rejected" and the
		// test's guard assertion fails, documenting the guard expectation.
		return
	}
	// The probe never queues: on a busy workflow.v1 session the gate rejects
	// the call outright with the typed error.
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, err := mgr.Execute(probeCtx, sessionName, step, noopSink{}, nil)
	s.mu.Lock()
	s.guardErr = err
	s.guardResultOk = err != nil && errors.As(err, new(*adapterhost.ErrChildRunInFlight))
	s.mu.Unlock()
}

func (s *kb95Shared) guard() (error, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.guardErr, s.guardResultOk
}

// kb95ChildAdapter models a workflow.v1 criteria-as-adapter child: its single
// in-flight Execute probes the re-Execute guard, then loses the phone-home
// transport to its child mid-run. The supervision journal's classification
// (ADR-0007, delivered through the SupervisedHandle seam) names the loss.
type kb95ChildAdapter struct {
	shared *kb95Shared

	mu       sync.Mutex
	executes []string
	opens    []string
}

func (a *kb95ChildAdapter) Info(context.Context) (adapterhost.Info, error) {
	return adapterhost.Info{
		Name:         "fake",
		Version:      "test",
		Capabilities: []string{"execute", "workflow.v1"},
	}, nil
}

func (a *kb95ChildAdapter) OpenSession(_ context.Context, name string, _, _ map[string]string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.opens = append(a.opens, name)
	return nil
}

func (a *kb95ChildAdapter) Execute(ctx context.Context, name string, step *workflow.StepNode, _ adapter.EventSink, rejection *criteriav2.ExecutionRejection) (adapter.Result, error) {
	a.mu.Lock()
	stepName := ""
	if step != nil {
		stepName = step.Name
	}
	a.executes = append(a.executes, stepName)
	a.mu.Unlock()
	if stepName == "run_child" {
		a.shared.reExecuteProbe(ctx, name, step)
		// The child died mid-run: the transport loss surfaces with the
		// wrapped child-run evidence, and the peer journal delivered the
		// terminal classification for the parent to consume.
		return adapter.Result{}, errors.New(kb95ChildLossErr)
	}
	return adapter.Result{Outcome: "success"}, nil
}

// SupervisionCrashReason implements SupervisedHandle: the peer supervision
// journal delivered the child-run-lost classification verbatim (ADR-0007
// wire fact; the remote peer wrap routes it here).
func (a *kb95ChildAdapter) SupervisionCrashReason() (string, bool) {
	return adapterhost.CrashReasonChildRunLost, true
}

func (a *kb95ChildAdapter) Permit(context.Context, string, string, bool, string) error { return nil }
func (a *kb95ChildAdapter) CloseSession(context.Context, string) error                 { return nil }
func (a *kb95ChildAdapter) Kill()                                                      {}
func (a *kb95ChildAdapter) Pause(context.Context, string) error                        { return nil }
func (a *kb95ChildAdapter) Resume(context.Context, string) error                       { return nil }
func (a *kb95ChildAdapter) Inspect(context.Context, string) (*criteriav2.InspectResponse, error) {
	return &criteriav2.InspectResponse{}, nil
}
func (a *kb95ChildAdapter) Snapshot(context.Context, string) (*criteriav2.SnapshotResponse, error) {
	return &criteriav2.SnapshotResponse{}, nil
}
func (a *kb95ChildAdapter) Restore(context.Context, string, []byte, uint32) error { return nil }

func (a *kb95ChildAdapter) callLog() (opens, executes []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.opens...), append([]string(nil), a.executes...)
}

func (a *kb95ChildAdapter) executeCount(stepName string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, s := range a.executes {
		if s == stepName {
			n++
		}
	}
	return n
}

// kb95ReproSink extends fakeSink with per-step adapter event capture and
// outcome error capture (the CRI-271 shape).
type kb95ReproSink struct {
	*fakeSink

	mu       sync.Mutex
	events   []cri271Event
	outcomes []kb95Outcome
}

type kb95Outcome struct {
	step    string
	outcome string
	err     string
}

func (s *kb95ReproSink) OnStepOutcome(step, outcome string, _ time.Duration, err error, comment string) {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	s.mu.Lock()
	s.outcomes = append(s.outcomes, kb95Outcome{step: step, outcome: outcome, err: msg})
	s.mu.Unlock()
	s.fakeSink.OnStepOutcome(step, outcome, 0, nil, "")
}

func (s *kb95ReproSink) StepEventSink(string) adapter.EventSink {
	return &kb95EventRecorder{parent: s}
}

type kb95EventRecorder struct {
	parent *kb95ReproSink
}

func (r *kb95EventRecorder) Log(string, []byte) {}
func (r *kb95EventRecorder) Adapter(kind string, data any) {
	payload, _ := data.(map[string]any)
	r.parent.mu.Lock()
	defer r.parent.mu.Unlock()
	r.parent.events = append(r.parent.events, cri271Event{kind: kind, data: payload})
}

func (s *kb95ReproSink) recorded(kind string) (map[string]any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, evt := range s.events {
		if evt.kind == kind {
			return evt.data, true
		}
	}
	return nil, false
}

func (s *kb95ReproSink) lastOutcome(step string) (kb95Outcome, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.outcomes) - 1; i >= 0; i-- {
		if s.outcomes[i].step == step {
			return s.outcomes[i], true
		}
	}
	return kb95Outcome{}, false
}

// TestKB95_WorkflowV1ReExecuteGuardFailClosedAndCrashAdoption is the KB-95
// CRI-style repro: during the child run's Execute a second (overlapping)
// Execute on the same session is rejected fail-closed with the typed
// ErrChildRunInFlight — the adapter never double-runs — and when the
// phone-home transport to the child dies mid-run the step fails with the
// child's classified evidence (CrashReasonChildRunLost via the supervision
// wire fact), the run routes through the step's failure arm to completion,
// and no additional child run is spawned for the failed attempt.
func TestKB95_WorkflowV1ReExecuteGuardFailClosedAndCrashAdoption(t *testing.T) {
	g := compile(t, kb95Workflow)
	shared := &kb95Shared{}
	p := &kb95ChildAdapter{shared: shared}
	sink := &kb95ReproSink{fakeSink: &fakeSink{}}
	eng := NewTestEngine(g, cri271NewLoader(p), sink)
	shared.engine = eng
	err := eng.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if sink.terminal != "done" || !sink.terminalOK {
		t.Errorf("terminal state %q success=%v; want done/true", sink.terminal, sink.terminalOK)
	}

	// 1) Fail-closed re-Execute guard: the probe got the typed error.
	guardErr, guardTyped := shared.guard()
	if guardErr == nil {
		t.Fatal("re-Execute probe was not rejected; want the typed ErrChildRunInFlight")
	}
	if !guardTyped {
		t.Errorf("guard err = %v; want *adapterhost.ErrChildRunInFlight", guardErr)
	}
	var guard *adapterhost.ErrChildRunInFlight
	if errors.As(guardErr, &guard) && guard.Session != "fake.default" {
		t.Errorf("guard.Session = %q; want fake.default", guard.Session)
	}

	// 2) No double-run: exactly one child-run Execute (the in-flight one);
	// the guard probe never reached the adapter, and the failed attempt was
	// not retried as a fresh child run (the step's failure arm routes it).
	if got := p.executeCount("run_child"); got != 1 {
		t.Errorf("run_child Execute count = %d; want exactly 1 (no queued/no double run)", got)
	}

	// 3) Crash adoption: the step failure carries the child evidence, and the
	// classification consumed the supervision wire fact, not the transport
	// string matching.
	out, ok := sink.lastOutcome("run_child")
	if !ok || out.outcome != "failure" {
		t.Fatalf("run_child outcome = %+v ok=%v; want a failure outcome", out, ok)
	}
	if !strings.Contains(out.err, "connection lost") || !strings.Contains(out.err, kb95ChildLossErr) {
		t.Errorf("step failure error = %q; want the child-run-loss evidence", out.err)
	}
	data, ok := sink.recorded("session.crash")
	if !ok {
		t.Fatal("expected a session.crash event for the lost child run")
	}
	if reason, _ := data["crash_reason"].(string); reason != adapterhost.CrashReasonChildRunLost {
		t.Errorf("crash_reason = %q; want CrashReasonChildRunLost", reason)
	}
	if data["session"] != "fake.default" {
		t.Errorf("session.crash session = %v; want fake.default", data["session"])
	}

	// 4) Bookkeeping completed on the (re-opened) session: CRI-271 reopen
	// applies to the adopted crash too.
	_, executes := p.callLog()
	foundBookkeeping := false
	for _, s := range executes {
		if s == "collect_outcome" {
			foundBookkeeping = true
		}
	}
	if !foundBookkeeping {
		t.Errorf("collect_outcome did not execute after the adopted crash; executes = %v", executes)
	}
}
