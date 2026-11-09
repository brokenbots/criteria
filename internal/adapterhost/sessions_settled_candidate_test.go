package adapterhost

// sessions_settled_candidate_test.go — pins the settle arm of
// isSessionCrashCandidate: a transport death observed before the journal's
// terminal/CrashClassified record lands is only visible through the
// bounded real-time settle query, so that arm is load-bearing for the
// in-process supervisor's flip-window behaviour (every other evidence
// source is covered elsewhere). Also pins the peer shape whose journal
// delivered only the plain Exited placeholder: it carries no
// classification evidence, so its failure stays an ordinary step error.

import (
	"context"
	"errors"
	"testing"
	"time"

	criteriav2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/workflow"
)

// newSettledOnlySession builds the settled-only supervised handle and its
// crash session: SupervisionCrashReason answers ("", false), the settled
// query carries the fact once delivered, and ProcessExited stays false —
// the settled fact is the session's only crash evidence.
func newSettledOnlySession() (*settledOnlySupervisedHandle, *SessionManager, *Session) {
	h := &settledOnlySupervisedHandle{
		supervisedCrashHandle: &supervisedCrashHandle{
			cri287Handle: &cri287Handle{name: "fake", deadAfter: 0},
		},
	}
	sm, sess := newCrashSession(h)
	return h, sm, sess
}

// TestSettledCrashCandidate_PlainQueryEmptySettledFactMakesCandidate is the
// predicate isolation: the settle arm alone flips the candidate — no plain
// fact, no process exit, just the settled query delivering the fact. With
// the settle arm deleted, the candidate stays false after deliverSettled.
func TestSettledCrashCandidate_PlainQueryEmptySettledFactMakesCandidate(t *testing.T) {
	h, _, sess := newSettledOnlySession()

	if isSessionCrashCandidate(context.Background(), sess) {
		t.Fatal("crash candidate = true with no plain fact, no settled fact, and no process exit")
	}
	h.deliverSettled(CrashReasonTransportClosed)
	if !isSessionCrashCandidate(context.Background(), sess) {
		t.Fatal("crash candidate = false with only the settled query carrying the fact (the settle arm is load-bearing)")
	}
}

// TestSettledCrashCandidate_SettledFactRoutesCrashOutsideWindow is the
// end-to-end pin for the flip window: with the teardown window closed, a
// settled-only fact routes the failed Execute through handleCrash —
// SessionCrashError wrapping the raw transport error, crashed flag set, a
// session.crash event carrying the delivered reason. Without the settle
// arm, the candidate is false: no SessionCrashError, no event, no crashed
// flag, and the raw error surfaces directly.
func TestSettledCrashCandidate_SettledFactRoutesCrashOutsideWindow(t *testing.T) {
	h, sm, sess := newSettledOnlySession()
	h.deliverSettled(CrashReasonTransportClosed)

	coll := &adapterEventCollector{}
	_, err := sm.Execute(context.Background(), "shell.develop", &workflow.StepNode{Name: "comment_handler_failed"}, coll, nil)
	var crashErr *SessionCrashError
	if !errors.As(err, &crashErr) || crashErr.Session != "shell.develop" {
		t.Fatalf("Execute err = %v, want SessionCrashError (the settled fact is the only evidence)", err)
	}
	if !errors.Is(err, cri287TransportErr) {
		t.Errorf("crash err = %v, want it to wrap the raw transport error", err)
	}
	if !sess.crashed.Load() {
		t.Error("session should be marked crashed")
	}
	event, ok := coll.first("session.crash")
	if !ok {
		t.Fatal("expected the session.crash event")
	}
	if got := event["crash_reason"]; got != CrashReasonTransportClosed {
		t.Errorf("crash_reason = %q, want the delivered settled reason %q", got, CrashReasonTransportClosed)
	}
}

// TestSettledCrashCandidate_SettledFactInsideWindowRoutesTimeout preserves
// the carve-out semantics for the settled arm: inside an open teardown
// window the failure routes as a timeout — raw error, no SessionCrashError,
// no session.crash event. Every settler is peer-supervised (JournalCrashSettler
// embeds SupervisedHandle, which isPeerSupervised tests for), so the
// open-window carve-out never reaches handleCrash for a settler; the
// settled-fact crash route is the window-closed path above.
func TestSettledCrashCandidate_SettledFactInsideWindowRoutesTimeout(t *testing.T) {
	h, sm, sess := newSettledOnlySession()
	h.deliverSettled(CrashReasonTransportClosed)
	sm.StepTimeoutTeardownWindow = time.Minute
	sm.MarkEngineStepTimeoutTeardown()

	coll := &adapterEventCollector{}
	_, err := sm.Execute(context.Background(), "shell.develop", &workflow.StepNode{Name: "comment_handler_failed"}, coll, nil)
	if err == nil || !errors.Is(err, cri287TransportErr) {
		t.Fatalf("Execute err = %v, want the raw transport error (timeout routing inside the window)", err)
	}
	var crashErr *SessionCrashError
	if errors.As(err, &crashErr) {
		t.Fatalf("Execute err = %v, want no SessionCrashError inside the teardown window", err)
	}
	if sess.crashed.Load() {
		t.Error("session must not be marked crashed while the window is open")
	}
	if _, ok := coll.first("session.crash"); ok {
		t.Error("session.crash event must not be emitted while the window is open")
	}
	// The settle budget is bounded: one settle for the candidate plus one
	// for the carve-out's classifySessionCrash diagnostics.
	if got := h.settleCount(); got != 2 {
		t.Errorf("settle calls = %d, want exactly 2 (candidate evaluation + crash diagnostics)", got)
	}
}

// TestSettledCrashCandidate_WindowOpenFactFreeCountsOneSettle pins the
// single candidate evaluation per failed Execute: without the hoist, the
// two candidate call sites in executeError each settle (two budgets) and
// can observe different journal states. On the window-open path with no
// evidence, exactly one settle is spent; the diagnostics settle only runs
// when the carve-out routes, which this fact-free shape never reaches.
func TestSettledCrashCandidate_WindowOpenFactFreeCountsOneSettle(t *testing.T) {
	h, sm, _ := newSettledOnlySession()
	sm.StepTimeoutTeardownWindow = time.Minute
	sm.MarkEngineStepTimeoutTeardown()

	coll := &adapterEventCollector{}
	_, err := sm.Execute(context.Background(), "shell.develop", &workflow.StepNode{Name: "comment_handler_failed"}, coll, nil)
	if err == nil || !errors.Is(err, cri287TransportErr) {
		t.Fatalf("Execute err = %v, want the raw transport error (no evidence, no classification)", err)
	}
	if got := h.settleCount(); got != 1 {
		t.Errorf("settle calls = %d, want exactly 1 (single candidate evaluation per failed Execute)", got)
	}
}

// exitedPlaceholderPeerHandle models the peer session whose supervision
// journal delivered only the plain Exited placeholder: the supervised
// query reports no terminal classification and the peer carries no
// ProcessExitReporter capability. Journal-only classification leaves that
// shape with zero crash evidence, so the transport failure must stay an
// ordinary step error.
type exitedPlaceholderPeerHandle struct {
	name string
}

func (h *exitedPlaceholderPeerHandle) SupervisionCrashReason() (string, bool) {
	return "", false
}

func (h *exitedPlaceholderPeerHandle) Info(context.Context) (Info, error) {
	return Info{Name: h.name}, nil
}

func (h *exitedPlaceholderPeerHandle) OpenSession(context.Context, string, map[string]string, map[string]string) error {
	return nil
}

func (h *exitedPlaceholderPeerHandle) Execute(_ context.Context, _ string, _ *workflow.StepNode, _ adapter.EventSink, _ *criteriav2.ExecutionRejection) (adapter.Result, error) {
	return adapter.Result{}, cri287TransportErr
}

func (h *exitedPlaceholderPeerHandle) CloseSession(context.Context, string) error { return nil }
func (h *exitedPlaceholderPeerHandle) Kill()                                      {}
func (h *exitedPlaceholderPeerHandle) Pause(context.Context, string) error        { return nil }
func (h *exitedPlaceholderPeerHandle) Resume(context.Context, string) error       { return nil }
func (h *exitedPlaceholderPeerHandle) Inspect(context.Context, string) (*criteriav2.InspectResponse, error) {
	return &criteriav2.InspectResponse{}, nil
}
func (h *exitedPlaceholderPeerHandle) Snapshot(context.Context, string) (*criteriav2.SnapshotResponse, error) {
	return &criteriav2.SnapshotResponse{}, nil
}
func (h *exitedPlaceholderPeerHandle) Restore(context.Context, string, []byte, uint32) error {
	return nil
}

// TestPlainExitedPeerJournalStaysOrdinaryStepError pins the peer shape
// whose journal carries only the plain Exited placeholder: no crash
// evidence exists ( SupervisionCrashReason answers ("", false), no
// ProcessExitReporter), so the failed Execute returns the raw transport
// error directly — no SessionCrashError, no crashed flag, no session.crash
// event.
func TestPlainExitedPeerJournalStaysOrdinaryStepError(t *testing.T) {
	sm, sess := newCrashSession(&exitedPlaceholderPeerHandle{name: "fake"})

	coll := &adapterEventCollector{}
	result, err := sm.Execute(context.Background(), "shell.develop", &workflow.StepNode{Name: "comment_handler_failed"}, coll, nil)
	if err == nil {
		t.Fatal("expected the raw transport error, got nil")
	}
	var crashErr *SessionCrashError
	if errors.As(err, &crashErr) {
		t.Fatalf("Execute err = %v, want the raw transport error (plain Exited carries no classification evidence)", err)
	}
	if !errors.Is(err, cri287TransportErr) {
		t.Errorf("err = %v, want the verbatim transport error", err)
	}
	if result.Outcome != "" {
		t.Errorf("result = %+v with a raw error; adapter outcome must not be synthesized", result)
	}
	if sess.crashed.Load() {
		t.Error("session must not be marked crashed without any crash evidence")
	}
	if _, ok := coll.first("session.crash"); ok {
		t.Error("session.crash event must not be emitted without any crash evidence")
	}
}
