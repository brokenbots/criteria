package adapterhost

// sessions_supervised_crash_test.go — regression pinning the supervision
// wire fact as crash-candidate evidence in executeError: a failed Execute on
// a handle carrying a journal-delivered classification (SupervisedHandle,
// ADR-0007 T-07) routes to the crash machinery regardless of the transport
// error text, because a real peer conn produces a bare codes.Canceled
// "context canceled" when the child's death cancels the server-side Execute
// handler first — an error shape matching none of the legacy string
// heuristics. Scoping: handles without the seam, and supervised handles
// whose journal delivered only the plain Exited placeholder (ok=false),
// keep the existing routing.

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	criteriav2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/workflow"
)

// canceledTurnError is the transport error the peer path produces when the
// child's death cancels the server-side Execute handler before the client
// transport notices: codes.Canceled "context canceled", matching none of the
// legacy heuristic signatures ("connection", "transport is closing",
// "unavailable", "broken pipe", "eof", "terminated").
var canceledTurnError = status.Error(codes.Canceled, "context canceled")

// canceledSupervisedHandle is the supervised fake with the transport failure
// shape swapped for the bare canceled-turn error: the supervisedCrashHandle
// classification seam plus an Execute replay the legacy heuristics never match.
type canceledSupervisedHandle struct {
	*supervisedCrashHandle
}

func (h *canceledSupervisedHandle) Execute(context.Context, string, *workflow.StepNode, adapter.EventSink, *criteriav2.ExecutionRejection) (adapter.Result, error) {
	return adapter.Result{}, canceledTurnError
}

// canceledHandle is the legacy cri287Handle with the same swapped failure
// shape: no SupervisedHandle seam, so the classification seam cannot widen
// its gate.
type canceledHandle struct {
	*cri287Handle
}

func (h *canceledHandle) Execute(context.Context, string, *workflow.StepNode, adapter.EventSink, *criteriav2.ExecutionRejection) (adapter.Result, error) {
	return adapter.Result{}, canceledTurnError
}

// newSupervisedCrashSession registers a session whose supervised fails every
// Execute with the canceled-turn error, standing in for a peer conn dead
// from the child's crash.
func newSupervisedCrashSession() (*SessionManager, *Session, *canceledSupervisedHandle) {
	h := &canceledSupervisedHandle{supervisedCrashHandle: &supervisedCrashHandle{cri287Handle: &cri287Handle{name: "fake", deadAfter: 0}}}
	sm := &SessionManager{loader: nil, sessions: map[string]*Session{}}
	sess := &Session{Name: "shell.develop", Adapter: "shell", handle: h}
	sm.mu.Lock()
	sm.sessions["shell.develop"] = sess
	sm.mu.Unlock()
	sess.noteActivity()
	return sm, sess, h
}

// TestSupervisedCrashCandidate_ClassificationRoutesCrashOutsideWindow pins
// the fix: with no teardown window open, a classification-backed Execute
// failure reaches handleCrash even though the transport error text matches
// no legacy heuristic, producing SessionCrashError for the session and a
// session.crash event whose crash_reason is the journal wire fact verbatim.
func TestSupervisedCrashCandidate_ClassificationRoutesCrashOutsideWindow(t *testing.T) {
	sm, sess, h := newSupervisedCrashSession()
	h.deliver(CrashReasonProcessTerminated)

	coll := &adapterEventCollector{}
	_, err := sm.Execute(context.Background(), "shell.develop", &workflow.StepNode{Name: "comment_handler_failed"}, coll, nil)
	var crashErr *SessionCrashError
	if !errors.As(err, &crashErr) || crashErr.Session != "shell.develop" {
		t.Fatalf("Execute err = %v, want SessionCrashError (wire fact outranks the unmatched transport text)", err)
	}
	if !errors.Is(err, canceledTurnError) {
		t.Errorf("crash err = %v, want it to wrap the raw transport error", err)
	}
	if !sess.crashed.Load() {
		t.Error("session should be marked crashed")
	}
	event, ok := coll.first("session.crash")
	if !ok {
		t.Fatal("expected the session.crash event")
	}
	if got := event["crash_reason"]; got != CrashReasonProcessTerminated {
		t.Errorf("crash_reason = %q, want the journal wire fact verbatim %q", got, CrashReasonProcessTerminated)
	}
}

// TestSupervisedCrashCandidate_ClassificationInsideWindowRoutesTimeout is
// the other half of the contract: with the teardown window open, the same
// classification-backed failure is a teardown consequence — raw error, no
// SessionCrashError, no session.crash event.
func TestSupervisedCrashCandidate_ClassificationInsideWindowRoutesTimeout(t *testing.T) {
	sm, sess, h := newSupervisedCrashSession()
	h.deliver(CrashReasonProcessTerminated)
	sm.StepTimeoutTeardownWindow = time.Minute
	sm.MarkEngineStepTimeoutTeardown()

	coll := &adapterEventCollector{}
	_, err := sm.Execute(context.Background(), "shell.develop", &workflow.StepNode{Name: "comment_handler_failed"}, coll, nil)
	if err == nil || !errors.Is(err, canceledTurnError) {
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
}

// TestSupervisedCrashCandidate_PlainExitedPlaceholderNotWidened pins the
// scoping rule: a supervised handle whose journal delivered only the plain
// Exited placeholder (SupervisionCrashReason ok=false, e.g. a clean child
// exit) does not turn a later heuristic-unmatched Execute error into a
// crash — the gate is widened on the delivered classification, not on
// ProcessExited.
func TestSupervisedCrashCandidate_PlainExitedPlaceholderNotWidened(t *testing.T) {
	sm, sess, h := newSupervisedCrashSession()
	h.exited.Store(true)

	coll := &adapterEventCollector{}
	_, err := sm.Execute(context.Background(), "shell.develop", &workflow.StepNode{Name: "comment_handler_failed"}, coll, nil)
	if err == nil || !errors.Is(err, canceledTurnError) {
		t.Fatalf("Execute err = %v, want the raw transport error (no classification delivered)", err)
	}
	var crashErr *SessionCrashError
	if errors.As(err, &crashErr) {
		t.Fatalf("Execute err = %v, want no SessionCrashError for the plain Exited placeholder", err)
	}
	if sess.crashed.Load() {
		t.Error("session must not be marked crashed without a delivered classification")
	}
	if _, ok := coll.first("session.crash"); ok {
		t.Error("session.crash event must not be emitted without a delivered classification")
	}
}

// TestSupervisedCrashCandidate_LegacyHandleKeepsStringHeuristics pins the
// legacy path: a handle without the SupervisedHandle seam and a transport
// error matching no heuristic signature is returned as-is — no crash
// classification, no behavior change.
func TestSupervisedCrashCandidate_LegacyHandleKeepsStringHeuristics(t *testing.T) {
	h := &canceledHandle{cri287Handle: &cri287Handle{name: "fake", deadAfter: 0}}
	sm := &SessionManager{loader: nil, sessions: map[string]*Session{}}
	sess := &Session{Name: "shell.develop", Adapter: "shell", handle: h}
	sm.mu.Lock()
	sm.sessions["shell.develop"] = sess
	sm.mu.Unlock()
	sess.noteActivity()

	coll := &adapterEventCollector{}
	_, err := sm.Execute(context.Background(), "shell.develop", &workflow.StepNode{Name: "comment_handler_failed"}, coll, nil)
	if err == nil || !errors.Is(err, canceledTurnError) {
		t.Fatalf("Execute err = %v, want the raw transport error (legacy handle, no heuristic match)", err)
	}
	var crashErr *SessionCrashError
	if errors.As(err, &crashErr) {
		t.Fatalf("Execute err = %v, want no SessionCrashError for the legacy handle", err)
	}
	if sess.crashed.Load() {
		t.Error("session must not be marked crashed for the legacy handle")
	}
	if _, ok := coll.first("session.crash"); ok {
		t.Error("session.crash event must not be emitted for the legacy handle")
	}
}
