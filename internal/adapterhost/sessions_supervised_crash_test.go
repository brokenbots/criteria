package adapterhost

// sessions_supervised_crash_test.go — regression pinning the supervision
// wire fact as crash-candidate evidence in executeError: a failed Execute on
// a handle carrying a journal-delivered classification (SupervisedHandle,
// ADR-0007 T-07) routes to the crash machinery regardless of the transport
// error text, because a real peer conn produces a bare codes.Canceled
// "context canceled" when the child's death cancels the server-side Execute
// handler first — an error shape the host never inspects textually (the
// journal, not the message, is the classification source). Scoping: handles
// without any classification evidence keep the existing routing.

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
// transport notices: codes.Canceled "context canceled". The host never
// inspects this text — the journal-delivered fact is what classifies.
var canceledTurnError = status.Error(codes.Canceled, "context canceled")

// canceledSupervisedHandle is the supervised fake with the transport failure
// shape swapped for the bare canceled-turn error: the supervisedCrashHandle
// classification seam plus an Execute replay whose text carries no evidence.
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

// newSupervisedCrashSession registers a session whose supervised handle
// fails every Execute with the canceled-turn error, standing in for a peer
// conn dead from the child's crash.
func newSupervisedCrashSession() (*SessionManager, *Session, *canceledSupervisedHandle) {
	h := &canceledSupervisedHandle{supervisedCrashHandle: &supervisedCrashHandle{cri287Handle: &cri287Handle{name: "fake", deadAfter: 0}}}
	sm, sess := newCrashSession(h)
	return sm, sess, h
}

// newCrashSession registers a pre-built failing handle as the live session
// "shell.develop".
func newCrashSession(h Handle) (*SessionManager, *Session) {
	sm := &SessionManager{loader: nil, sessions: map[string]*Session{}}
	sess := &Session{Name: "shell.develop", Adapter: "shell", handle: h}
	sm.mu.Lock()
	sm.sessions["shell.develop"] = sess
	sm.mu.Unlock()
	sess.noteActivity()
	return sm, sess
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
// SessionCrashError, no session.crash event. The classification content is
// irrelevant to the routing, so a different journal fact (the ADR-0008
// child-run-lost taxonomy reason) exercises the seam too.
func TestSupervisedCrashCandidate_ClassificationInsideWindowRoutesTimeout(t *testing.T) {
	sm, sess, h := newSupervisedCrashSession()
	h.deliver(CrashReasonChildRunLost)
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

// TestSupervisedCrashCandidate_PlainExitedPlaceholderRoutesCrashStructurally
// pins the M5 scoping: a supervised handle whose journal delivered only the
// plain Exited placeholder (SupervisionCrashReason ok=false) carries no
// classification, but the underlying process exit is structural crash
// evidence, so the failed Execute still routes to the crash machinery — with
// crash_reason from the structural arm, not from the journal.
func TestSupervisedCrashCandidate_PlainExitedPlaceholderRoutesCrashStructurally(t *testing.T) {
	sm, sess, h := newSupervisedCrashSession()
	h.exited.Store(true)

	coll := &adapterEventCollector{}
	_, err := sm.Execute(context.Background(), "shell.develop", &workflow.StepNode{Name: "comment_handler_failed"}, coll, nil)
	var crashErr *SessionCrashError
	if !errors.As(err, &crashErr) || crashErr.Session != "shell.develop" {
		t.Fatalf("Execute err = %v, want SessionCrashError (structural exit is crash evidence)", err)
	}
	if !errors.Is(err, canceledTurnError) {
		t.Errorf("crash err = %v, want it to wrap the raw transport error", err)
	}
	if !sess.crashed.Load() {
		t.Error("session should be marked crashed for the structural exit")
	}
	event, ok := coll.first("session.crash")
	if !ok {
		t.Fatal("expected the session.crash event")
	}
	if got := event["crash_reason"]; got != CrashReasonProcessExitedEarly {
		t.Errorf("crash_reason = %q, want %q (structural exit arm, not a journal classification)", got, CrashReasonProcessExitedEarly)
	}
}

// TestSupervisedCrashCandidate_UnmatchedErrorOnHandleWithoutEvidenceStaysRaw
// pins the boundary: a handle carrying no classification evidence (no
// SupervisedHandle fact, no process exit) and an Execute error the host
// never inspects textually is returned as-is — no crash classification, no
// behavior change.
func TestSupervisedCrashCandidate_UnmatchedErrorOnHandleWithoutEvidenceStaysRaw(t *testing.T) {
	sm, sess := newCrashSession(&canceledHandle{cri287Handle: &cri287Handle{name: "fake", deadAfter: 0}})

	coll := &adapterEventCollector{}
	_, err := sm.Execute(context.Background(), "shell.develop", &workflow.StepNode{Name: "comment_handler_failed"}, coll, nil)
	if err == nil || !errors.Is(err, canceledTurnError) {
		t.Fatalf("Execute err = %v, want the raw transport error (no evidence on the handle)", err)
	}
	var crashErr *SessionCrashError
	if errors.As(err, &crashErr) {
		t.Fatalf("Execute err = %v, want no SessionCrashError for the evidence-free handle", err)
	}
	if sess.crashed.Load() {
		t.Error("session must not be marked crashed without evidence")
	}
	if _, ok := coll.first("session.crash"); ok {
		t.Error("session.crash event must not be emitted without evidence")
	}
	if isSessionCrashCandidate(context.Background(), sess) {
		t.Error("evidence-free handle must not be a crash candidate")
	}
}
