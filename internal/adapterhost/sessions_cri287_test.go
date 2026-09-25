package adapterhost

// sessions_cri287_test.go — CRI-287: on an engine-initiated step-timeout
// cancellation (the CRI-275 step ceiling), the canceled Execute stream tears
// down sibling phone-home transports in the same second. A transport close
// observed on a later Execute (fresh context) is therefore the consequence of
// that cancellation, not an adapter death, and must not be classified as a
// session crash — the step's declared failure/default outcome routing (the
// checkpoint loop) has to win the race against the transport-close classifier.
// A genuine adapter death outside the teardown window keeps the
// hard-failure classification (CRI-271).

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	criteriav2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/workflow"
)

// cri287TransportErr is the verbatim teardown signature from run 74cf49b7
// (CRI-287): the go-plugin shim's gRPC transport closing underneath a live
// adapter session while the step-timeout teardown cascades through sibling
// sessions on the same phone-home connection.
var cri287TransportErr = errors.New("rpc error: code = Canceled desc = grpc: the client connection is closing")

// cri287Handle mimics a session whose transport died during a step-timeout
// teardown cascade: the first `deadAfter` Executes succeed, every later one
// replays the transport error. The `exited` flag implements the
// ProcessExitReporter seam so tests can exercise the process-exit crash
// classification (the "adapter is verifiably dead" case).
type cri287Handle struct {
	name      string
	deadAfter int

	exited atomic.Bool

	mu       sync.Mutex
	executes int
}

// ProcessExited implements ProcessExitReporter for the test fake.
func (h *cri287Handle) ProcessExited() bool { return h.exited.Load() }

func (h *cri287Handle) Info(context.Context) (Info, error) { return Info{Name: h.name}, nil }
func (h *cri287Handle) OpenSession(context.Context, string, map[string]string, map[string]string) error {
	return nil
}
func (h *cri287Handle) Execute(context.Context, string, *workflow.StepNode, adapter.EventSink) (adapter.Result, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.executes++
	if h.executes > h.deadAfter {
		return adapter.Result{}, cri287TransportErr
	}
	return adapter.Result{Outcome: "success"}, nil
}
func (h *cri287Handle) CloseSession(context.Context, string) error { return nil }
func (h *cri287Handle) Kill()                                      {}
func (h *cri287Handle) Pause(context.Context, string) error        { return nil }
func (h *cri287Handle) Resume(context.Context, string) error       { return nil }
func (h *cri287Handle) Inspect(context.Context, string) (*criteriav2.InspectResponse, error) {
	return &criteriav2.InspectResponse{}, nil
}
func (h *cri287Handle) Snapshot(context.Context, string) (*criteriav2.SnapshotResponse, error) {
	return &criteriav2.SnapshotResponse{}, nil
}
func (h *cri287Handle) Restore(context.Context, string, []byte, uint32) error { return nil }

// newCri287Session registers a session whose handle fails every Execute with
// the transport-closed signature, standing in for a session torn down by the
// step-timeout cascade.
func newCri287Session() (*SessionManager, *Session) {
	h := &cri287Handle{name: "fake", deadAfter: 0}
	sm := &SessionManager{loader: nil, sessions: map[string]*Session{}}
	sess := &Session{Name: "shell.develop", Adapter: "shell", handle: h}
	sm.mu.Lock()
	sm.sessions["shell.develop"] = sess
	sm.mu.Unlock()
	sess.noteActivity() // the adapter showed life at open time
	return sm, sess
}

// TestCRI287_TimeoutTeardownTransportCloseNotCrashClassified pins the
// classification contract: with the engine-initiated step-timeout teardown
// marked, a transport close on a later Execute (fresh context) is returned
// as-is — no SessionCrashError, no session.crash sink event, no crashed
// flag — so the step's declared outcome routing can proceed.
func TestCRI287_TimeoutTeardownTransportCloseNotCrashClassified(t *testing.T) {
	sm, sess := newCri287Session()
	sm.MarkEngineStepTimeoutTeardown()

	coll := &adapterEventCollector{}
	result, err := sm.Execute(context.Background(), "shell.develop", &workflow.StepNode{Name: "comment_handler_failed"}, coll)

	if err == nil {
		t.Fatal("expected the raw transport error, got nil")
	}
	var crashErr *SessionCrashError
	if errors.As(err, &crashErr) {
		t.Fatalf("Execute err = %v, want the raw transport error (no crash classification)", err)
	}
	if !errors.Is(err, cri287TransportErr) {
		t.Errorf("err = %v, want the verbatim transport error", err)
	}
	if result.Outcome != "" {
		t.Errorf("result = %+v with a raw error; adapter outcome must not be synthesized", result)
	}
	if sess.crashed.Load() {
		t.Error("session must not be marked crashed during a step-timeout teardown")
	}
	if _, ok := coll.first("session.crash"); ok {
		t.Error("session.crash event must not be emitted during a step-timeout teardown")
	}
}

// TestCRI287_NoMarkStillCrashClassified is the contrast case: without the
// engine-initiated teardown mark the same transport error keeps the CRI-271
// hard-failure classification.
func TestCRI287_NoMarkStillCrashClassified(t *testing.T) {
	sm, _ := newCri287Session()

	coll := &adapterEventCollector{}
	_, err := sm.Execute(context.Background(), "shell.develop", &workflow.StepNode{Name: "comment_handler_failed"}, coll)

	var crashErr *SessionCrashError
	if !errors.As(err, &crashErr) || crashErr.Session != "shell.develop" {
		t.Fatalf("Execute err = %v, want SessionCrashError for shell.develop", err)
	}
	if _, ok := coll.first("session.crash"); !ok {
		t.Error("expected the session.crash event for a genuine transport death")
	}
}

// TestCRI287_SuccessfulSiblingDoesNotCloseTeardownWindow pins the window
// lifecycle (CRI-287 review): a successful Execute on a healthy sibling
// session does NOT end the teardown window — a healthy sibling proving its
// own transport alive is not evidence that a torn-down sibling has been
// observed — so the torn-down sibling's next Execute is still routed as a
// timeout teardown rather than misclassified as a crash.
func TestCRI287_SuccessfulSiblingDoesNotCloseTeardownWindow(t *testing.T) {
	sm := &SessionManager{loader: nil, sessions: map[string]*Session{}}
	healthy := &Session{Name: "shell.comment_handler", Adapter: "shell", handle: &cri287Handle{name: "fake", deadAfter: 1}}
	tornDown := &Session{Name: "shell.develop", Adapter: "shell", handle: &cri287Handle{name: "fake", deadAfter: 0}}
	sm.mu.Lock()
	sm.sessions["shell.comment_handler"] = healthy
	sm.sessions["shell.develop"] = tornDown
	sm.mu.Unlock()
	healthy.noteActivity()
	tornDown.noteActivity()
	sm.MarkEngineStepTimeoutTeardown()

	// The healthy sibling's Execute succeeds.
	if _, err := sm.Execute(context.Background(), "shell.comment_handler", &workflow.StepNode{Name: "comment_handler_failed"}, &adapterEventCollector{}); err != nil {
		t.Fatalf("healthy sibling Execute: %v", err)
	}

	// The torn-down sibling's Execute still observes the open window: the
	// transport close is routed as a timeout teardown, not a crash.
	coll := &adapterEventCollector{}
	result, err := sm.Execute(context.Background(), "shell.develop", &workflow.StepNode{Name: "comment_handler_failed"}, coll)
	var crashErr *SessionCrashError
	if errors.As(err, &crashErr) {
		t.Fatalf("torn-down sibling Execute err = %v, want the raw transport error (window must not close on a sibling success)", err)
	}
	if !errors.Is(err, cri287TransportErr) {
		t.Errorf("err = %v, want the verbatim transport error", err)
	}
	if result.Outcome != "" {
		t.Errorf("result = %+v with a raw error; adapter outcome must not be synthesized", result)
	}
	if tornDown.crashed.Load() {
		t.Error("torn-down sibling must not be marked crashed while the window is open")
	}
	if _, ok := coll.first("session.crash"); ok {
		t.Error("session.crash event must not be emitted while the window is open")
	}
}

// TestCRI287_TeardownWindowExpiresReenablesCrashClassification pins the
// window auto-expiry: a transport close inside the window is routed as a
// timeout teardown, but once the window expires a transport close is a
// genuine crash again — even when every Execute in between kept failing, so
// the failure-only cascade never leaves crash classification suppressed.
func TestCRI287_TeardownWindowExpiresReenablesCrashClassification(t *testing.T) {
	sm, sess := newCri287Session()
	sm.StepTimeoutTeardownWindow = 25 * time.Millisecond
	sm.MarkEngineStepTimeoutTeardown()

	// Inside the window: routed as a timeout teardown.
	_, err := sm.Execute(context.Background(), "shell.develop", &workflow.StepNode{Name: "comment_handler_failed"}, &adapterEventCollector{})
	if err == nil || !errors.Is(err, cri287TransportErr) {
		t.Fatalf("in-window Execute err = %v, want the raw transport error", err)
	}
	if sess.crashed.Load() {
		t.Fatal("in-window transport close must not mark the session crashed")
	}

	// After the window expires the same transport error is a genuine crash.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if time.Since(time.Unix(0, sm.engineStepTimeoutTeardownAt.Load())) >= sm.StepTimeoutTeardownWindow {
			break
		}
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

// TestCRI287_ProcessExitedDuringTeardownWindowStillCrashClassified pins the
// process-evidence carve-out: even with the teardown window open, an adapter
// whose process has verifiably exited keeps the hard-failure crash
// classification (CRI-271) — the ProcessExited signal is the precise
// "adapter is dead" case the transport-close reclassification must not
// swallow — and the crashed session stays reachable through
// ReopenCrashedSession.
func TestCRI287_ProcessExitedDuringTeardownWindowStillCrashClassified(t *testing.T) {
	sm := &SessionManager{
		loader:                    &cri271Loader{gens: []*cri271Handle{{name: "fake", gen: 1, fails: true}, {name: "fake", gen: 2}}, idx: 1},
		sessions:                  map[string]*Session{},
		StepTimeoutTeardownWindow: time.Minute,
	}
	gen1 := &cri287Handle{name: "fake", deadAfter: 0}
	gen1.exited.Store(true)
	sess := &Session{Name: "shell.develop", Adapter: "shell", handle: gen1}
	sm.mu.Lock()
	sm.sessions["shell.develop"] = sess
	sm.mu.Unlock()
	sess.noteActivity()
	sm.MarkEngineStepTimeoutTeardown()

	coll := &adapterEventCollector{}
	_, err := sm.Execute(context.Background(), "shell.develop", &workflow.StepNode{Name: "comment_handler_failed"}, coll)
	var crashErr *SessionCrashError
	if !errors.As(err, &crashErr) || crashErr.Session != "shell.develop" {
		t.Fatalf("Execute err = %v, want SessionCrashError despite the open teardown window (ProcessExited evidence)", err)
	}
	if !sess.crashed.Load() {
		t.Error("session must be marked crashed when the process verifiably exited")
	}
	event, ok := coll.first("session.crash")
	if !ok {
		t.Fatal("expected the session.crash event")
	}
	if got := event["crash_reason"]; got != CrashReasonProcessExitedEarly {
		t.Errorf("crash_reason = %q, want the ProcessExited classification", got)
	}

	// The crash machinery stays reachable: re-opening the crashed session
	// respawns a fresh handle (gen 2) and clears the crashed flag, so
	// follow-on work can proceed.
	if err := sm.ReopenCrashedSession(context.Background(), "shell.develop"); err != nil {
		t.Fatalf("ReopenCrashedSession: %v", err)
	}
	if l := sm.loader.(*cri271Loader); l.resolveCount() != 1 {
		t.Errorf("resolve count = %d, want exactly one respawn Resolve", l.resolveCount())
	}
	if sess.crashed.Load() {
		t.Error("session must not stay crashed after a successful re-open")
	}
	if _, err := sm.Execute(context.Background(), "shell.develop", &workflow.StepNode{Name: "comment_handler_failed"}, &adapterEventCollector{}); err != nil {
		t.Errorf("follow-on Execute on the re-opened session: %v", err)
	}
}

// TestCRI287_TeardownWindowFromEnv pins the operator-configurable teardown
// window: a valid duration is honored, empty, malformed, and non-positive
// values fall back to the built-in default (0).
func TestCRI287_TeardownWindowFromEnv(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		t.Setenv("CRITERIA_STEP_TIMEOUT_TEARDOWN_WINDOW", "30s")
		if got := stepTimeoutTeardownWindowFromEnv(); got != 30*time.Second {
			t.Errorf("got %v, want 30s", got)
		}
		if sm := NewSessionManager(nil); sm.StepTimeoutTeardownWindow != 30*time.Second {
			t.Errorf("NewSessionManager window = %v, want 30s", sm.StepTimeoutTeardownWindow)
		}
	})
	t.Run("trimmed", func(t *testing.T) {
		t.Setenv("CRITERIA_STEP_TIMEOUT_TEARDOWN_WINDOW", " 2m ")
		if got := stepTimeoutTeardownWindowFromEnv(); got != 2*time.Minute {
			t.Errorf("got %v, want 2m", got)
		}
	})
	for _, v := range []string{"", "garbage", "-1s", "0s"} {
		t.Run("fallback_"+v, func(t *testing.T) {
			t.Setenv("CRITERIA_STEP_TIMEOUT_TEARDOWN_WINDOW", v)
			if got := stepTimeoutTeardownWindowFromEnv(); got != 0 {
				t.Errorf("stepTimeoutTeardownWindowFromEnv(%q) = %v, want 0", v, got)
			}
		})
	}
}

// TestCRI287_TimeoutTeardownDoesNotSwallowPlainErrors pins that the window
// only reclassifies transport-close-shaped errors: an adapter-reported plain
// failure keeps its error text verbatim (declared outcome routing unchanged).
func TestCRI287_TimeoutTeardownDoesNotSwallowPlainErrors(t *testing.T) {
	sm := &SessionManager{loader: nil, sessions: map[string]*Session{}}
	plainErr := errors.New("script exited with code 3")
	h := &cri287PlainErrHandle{err: plainErr}
	sess := &Session{Name: "shell.develop", Adapter: "shell", handle: h}
	sm.mu.Lock()
	sm.sessions["shell.develop"] = sess
	sm.mu.Unlock()
	sm.MarkEngineStepTimeoutTeardown()

	coll := &adapterEventCollector{}
	_, err := sm.Execute(context.Background(), "shell.develop", &workflow.StepNode{Name: "develop"}, coll)
	if !errors.Is(err, plainErr) {
		t.Fatalf("err = %v, want the verbatim plain error", err)
	}
	if sess.crashed.Load() {
		t.Error("a plain adapter failure must not mark the session crashed")
	}
}

// cri287PlainErrHandle always fails with a non-transport error.
type cri287PlainErrHandle struct{ err error }

func (h *cri287PlainErrHandle) Info(context.Context) (Info, error) { return Info{Name: "fake"}, nil }
func (h *cri287PlainErrHandle) OpenSession(context.Context, string, map[string]string, map[string]string) error {
	return nil
}
func (h *cri287PlainErrHandle) Execute(context.Context, string, *workflow.StepNode, adapter.EventSink) (adapter.Result, error) {
	return adapter.Result{Outcome: "failure"}, h.err
}
func (h *cri287PlainErrHandle) CloseSession(context.Context, string) error { return nil }
func (h *cri287PlainErrHandle) Kill()                                      {}
func (h *cri287PlainErrHandle) Pause(context.Context, string) error        { return nil }
func (h *cri287PlainErrHandle) Resume(context.Context, string) error       { return nil }
func (h *cri287PlainErrHandle) Inspect(context.Context, string) (*criteriav2.InspectResponse, error) {
	return &criteriav2.InspectResponse{}, nil
}
func (h *cri287PlainErrHandle) Snapshot(context.Context, string) (*criteriav2.SnapshotResponse, error) {
	return &criteriav2.SnapshotResponse{}, nil
}
func (h *cri287PlainErrHandle) Restore(context.Context, string, []byte, uint32) error { return nil }

// TestCRI287_MarkLandedBeforeNextExecute pins that an Execute starting after
// the teardown mark observes the open window. (Engine-side mark/Execute
// ordering across steps is covered by
// TestCRI287_StepTimeoutTeardownReentersCheckpointLoop.)
func TestCRI287_MarkLandedBeforeNextExecute(t *testing.T) {
	sm, _ := newCri287Session()
	// The engine marks between the timed-out step and the next step's
	// Execute; simulate the same ordering from the test side.
	sm.MarkEngineStepTimeoutTeardown()

	done := make(chan error, 1)
	go func() {
		_, err := sm.Execute(context.Background(), "shell.develop", &workflow.StepNode{Name: "comment_handler_failed"}, &adapterEventCollector{})
		done <- err
	}()
	select {
	case err := <-done:
		var crashErr *SessionCrashError
		if errors.As(err, &crashErr) {
			t.Fatalf("Execute err = %v, want the raw transport error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Execute did not return")
	}
}
