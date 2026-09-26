package adapterhost

// sessions_t07_test.go — T-07: crash classification from wire facts. A peer
// handle whose supervision journal delivered a CrashClassified.reason wire
// fact (ADR-0007, T-05) is consumed VERBATIM by classifySessionCrash, ahead
// of the ProcessExited branch and the legacy string heuristics. The CRI-287
// teardown-window carve-out applies to peer-supervised handles: a
// supervision-delivered ProcessExited inside the open window is a teardown
// consequence (the child dies with the engine's canceled turn), so the
// failed Execute routes as timeout, while the same fact outside the window
// classifies the crash verbatim. Legacy-runner handles keep the pinned
// conservative rule (see TestCRI287_ProcessExitedDuringTeardownWindowStillCrashClassified).

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/brokenbots/criteria/workflow"
)

// supervisedCrashHandle is a fake peer-supervised handle: the cri287Handle
// transport behavior plus the SupervisedHandle capability exposing the
// reason the (simulated) supervision journal delivered.
type supervisedCrashHandle struct {
	*cri287Handle

	mu     sync.Mutex
	reason string // "" = no classification delivered yet
}

func (h *supervisedCrashHandle) SupervisionCrashReason() (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.reason == "" {
		return "", false
	}
	return h.reason, true
}

func (h *supervisedCrashHandle) deliver(reason string) {
	h.mu.Lock()
	h.reason = reason
	h.mu.Unlock()
}

// newSupervisedSession registers a session carrying the supervised fake.
func newSupervisedSession() (*SessionManager, *Session, *supervisedCrashHandle) {
	h := &supervisedCrashHandle{cri287Handle: &cri287Handle{name: "fake", deadAfter: 0}}
	sm := &SessionManager{loader: nil, sessions: map[string]*Session{}}
	sess := &Session{Name: "shell.develop", Adapter: "shell", handle: h}
	sm.mu.Lock()
	sm.sessions["shell.develop"] = sess
	sm.mu.Unlock()
	sess.noteActivity()
	return sm, sess, h
}

func TestClassifySessionCrash_SupervisionWireFactPriority(t *testing.T) {
	t.Run("delivered reason is used verbatim", func(t *testing.T) {
		_, _, h := newSupervisedSession()
		h.deliver(CrashReasonProcessTerminated)
		if got := classifySessionCrash(&Session{handle: h}, cri287TransportErr); got != CrashReasonProcessTerminated {
			t.Errorf("crash reason = %q, want the wire fact verbatim %q", got, CrashReasonProcessTerminated)
		}
	})
	t.Run("no classification falls through to ProcessExited", func(t *testing.T) {
		h := &supervisedCrashHandle{cri287Handle: &cri287Handle{name: "fake", deadAfter: 0}}
		h.exited.Store(true)
		if got := classifySessionCrash(&Session{handle: h}, cri287TransportErr); got != CrashReasonProcessExitedEarly {
			t.Errorf("crash reason = %q, want %q", got, CrashReasonProcessExitedEarly)
		}
	})
	t.Run("no classification and no exit falls back to heuristics", func(t *testing.T) {
		_, _, h := newSupervisedSession()
		if got := classifySessionCrash(&Session{handle: h}, cri287TransportErr); got != CrashReasonTransportClosed {
			t.Errorf("crash reason = %q, want the heuristic %q", got, CrashReasonTransportClosed)
		}
	})
	t.Run("legacy handle without the seam is unchanged", func(t *testing.T) {
		h := &cri287Handle{name: "fake", deadAfter: 0}
		h.exited.Store(true)
		if got := classifySessionCrash(&Session{handle: h}, nil); got != CrashReasonProcessExitedEarly {
			t.Errorf("crash reason = %q, want %q", got, CrashReasonProcessExitedEarly)
		}
	})
	t.Run("nil session and nil error stay unknown", func(t *testing.T) {
		if got := classifySessionCrash(nil, nil); got != CrashReasonUnknown {
			t.Errorf("crash reason = %q, want %q", got, CrashReasonUnknown)
		}
	})
}

// malformedSupervisedHandle simulates a peer whose journal delivered a
// CrashClassified record whose reason is not a trustworthy classification:
// empty, or outside the CrashReason* taxonomy (wire garbage / version skew).
type malformedSupervisedHandle struct {
	*cri287Handle

	reason string
}

func (h *malformedSupervisedHandle) SupervisionCrashReason() (string, bool) {
	return h.reason, true
}

// TestClassifySessionCrash_MalformedSupervisedReason pins the seam's
// invariant guard (T-07): a delivered classification the host cannot trust
// — empty or outside the CrashReason* taxonomy — is not consumed verbatim;
// classification falls through to the local evidence path, so
// classifySessionCrash only ever returns documented taxonomy constants.
func TestClassifySessionCrash_MalformedSupervisedReason(t *testing.T) {
	t.Run("empty delivered reason falls through to ProcessExited", func(t *testing.T) {
		h := &malformedSupervisedHandle{cri287Handle: &cri287Handle{name: "fake", deadAfter: 0}}
		h.exited.Store(true)
		if got := classifySessionCrash(&Session{handle: h}, cri287TransportErr); got != CrashReasonProcessExitedEarly {
			t.Errorf("crash reason = %q, want %q (an empty wire fact is not a classification)", got, CrashReasonProcessExitedEarly)
		}
	})
	t.Run("non-taxonomy delivered reason falls through to ProcessExited", func(t *testing.T) {
		h := &malformedSupervisedHandle{cri287Handle: &cri287Handle{name: "fake", deadAfter: 0}, reason: "universe restarted"}
		h.exited.Store(true)
		if got := classifySessionCrash(&Session{handle: h}, cri287TransportErr); got != CrashReasonProcessExitedEarly {
			t.Errorf("crash reason = %q, want %q (a non-taxonomy wire fact must not leak into the classification vocabulary)", got, CrashReasonProcessExitedEarly)
		}
	})
}

// TestCRI287_PeerProcessExitedDuringTeardownWindowRoutedAsTimeout pins the
// peer-path rule (T-07 requirement 2): with the engine-initiated step-timeout
// teardown window open, a supervision-delivered ProcessExited on a
// peer-supervised handle is a teardown consequence — the child dies with the
// canceled turn — so the failed Execute is returned as-is (timeout routing),
// not crash-classified. Outside the window the same wire fact classifies the
// crash with the journal reason verbatim.
func TestCRI287_PeerProcessExitedDuringTeardownWindowRoutedAsTimeout(t *testing.T) {
	t.Run("inside the window: routed as timeout, not a crash", func(t *testing.T) {
		sm, sess, h := newSupervisedSession()
		h.exited.Store(true)
		sm.StepTimeoutTeardownWindow = time.Minute
		sm.MarkEngineStepTimeoutTeardown()

		coll := &adapterEventCollector{}
		_, err := sm.Execute(context.Background(), "shell.develop", &workflow.StepNode{Name: "comment_handler_failed"}, coll)
		if err == nil || !errors.Is(err, cri287TransportErr) {
			t.Fatalf("Execute err = %v, want the raw transport error (timeout routing)", err)
		}
		var crashErr *SessionCrashError
		if errors.As(err, &crashErr) {
			t.Fatalf("Execute err = %v, want no SessionCrashError inside the teardown window", err)
		}
		if sess.crashed.Load() {
			t.Error("peer child death with the engine's canceled turn must not mark the session crashed")
		}
		if _, ok := coll.first("session.crash"); ok {
			t.Error("session.crash event must not be emitted inside the teardown window")
		}
	})

	t.Run("outside the window: wire fact classifies verbatim", func(t *testing.T) {
		sm, sess, h := newSupervisedSession()
		h.exited.Store(true)
		h.deliver(CrashReasonProcessTerminated)
		sm.StepTimeoutTeardownWindow = 25 * time.Millisecond
		sm.MarkEngineStepTimeoutTeardown()
		// Let the window expire without touching the classification path.
		deadline := time.Now().Add(5 * time.Second)
		for time.Since(time.Unix(0, sm.engineStepTimeoutTeardownAt.Load())) < sm.StepTimeoutTeardownWindow {
			if time.Now().After(deadline) {
				t.Fatal("teardown window did not expire")
			}
			time.Sleep(5 * time.Millisecond)
		}

		coll := &adapterEventCollector{}
		_, err := sm.Execute(context.Background(), "shell.develop", &workflow.StepNode{Name: "comment_handler_failed"}, coll)
		var crashErr *SessionCrashError
		if !errors.As(err, &crashErr) || crashErr.Session != "shell.develop" {
			t.Fatalf("Execute err = %v, want SessionCrashError after the window expired", err)
		}
		if !sess.crashed.Load() {
			t.Error("session should be marked crashed after the teardown window expired")
		}
		event, ok := coll.first("session.crash")
		if !ok {
			t.Fatal("expected the session.crash event")
		}
		if got := event["crash_reason"]; got != CrashReasonProcessTerminated {
			t.Errorf("crash_reason = %q, want the journal wire fact verbatim %q", got, CrashReasonProcessTerminated)
		}
	})
}

// TestCRI287_PeerTransportCloseDuringTeardownWindowRoutedAsTimeout covers
// the plain transport-close case on a peer-supervised handle: the peer conn
// is the only transport, so its loss inside the window is the cascade's
// consequence — same routing, no classification.
func TestCRI287_PeerTransportCloseDuringTeardownWindowRoutedAsTimeout(t *testing.T) {
	sm, sess, _ := newSupervisedSession()
	sm.StepTimeoutTeardownWindow = time.Minute
	sm.MarkEngineStepTimeoutTeardown()

	coll := &adapterEventCollector{}
	_, err := sm.Execute(context.Background(), "shell.develop", &workflow.StepNode{Name: "comment_handler_failed"}, coll)
	if err == nil || !errors.Is(err, cri287TransportErr) {
		t.Fatalf("Execute err = %v, want the raw transport error", err)
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

// TestCRI287_PeerWindowExpiredReenablesWireFactClassification is the
// contrast for the transport-close peer path: once the window expires the
// same failure keeps the CRI-271 hard-failure classification.
func TestCRI287_PeerWindowExpiredReenablesWireFactClassification(t *testing.T) {
	sm, sess, _ := newSupervisedSession()
	sm.StepTimeoutTeardownWindow = 25 * time.Millisecond
	sm.MarkEngineStepTimeoutTeardown()

	_, err := sm.Execute(context.Background(), "shell.develop", &workflow.StepNode{Name: "comment_handler_failed"}, &adapterEventCollector{})
	if err == nil || !errors.Is(err, cri287TransportErr) {
		t.Fatalf("in-window Execute err = %v, want the raw transport error", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Since(time.Unix(0, sm.engineStepTimeoutTeardownAt.Load())) < sm.StepTimeoutTeardownWindow {
		if time.Now().After(deadline) {
			t.Fatal("teardown window did not expire")
		}
		time.Sleep(5 * time.Millisecond)
	}

	coll := &adapterEventCollector{}
	_, err = sm.Execute(context.Background(), "shell.develop", &workflow.StepNode{Name: "comment_handler_failed"}, coll)
	var crashErr *SessionCrashError
	if !errors.As(err, &crashErr) || crashErr.Session != "shell.develop" {
		t.Fatalf("post-window Execute err = %v, want SessionCrashError (window expired)", err)
	}
	if !sess.crashed.Load() {
		t.Error("session should be marked crashed after the teardown window expired")
	}
	if _, ok := coll.first("session.crash"); !ok {
		t.Error("expected the session.crash event after the teardown window expired")
	}
}
