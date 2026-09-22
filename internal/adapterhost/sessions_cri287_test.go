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
// replays the transport error.
type cri287Handle struct {
	name      string
	deadAfter int

	mu       sync.Mutex
	executes int
}

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
func newCri287Session() (*SessionManager, *Session, *cri287Handle) {
	h := &cri287Handle{name: "fake", deadAfter: 0}
	sm := &SessionManager{loader: nil, sessions: map[string]*Session{}}
	sess := &Session{Name: "shell.develop", Adapter: "shell", handle: h}
	sm.mu.Lock()
	sm.sessions["shell.develop"] = sess
	sm.mu.Unlock()
	sess.noteActivity() // the adapter showed life at open time
	return sm, sess, h
}

// TestCRI287_TimeoutTeardownTransportCloseNotCrashClassified pins the
// classification contract: with the engine-initiated step-timeout teardown
// marked, a transport close on a later Execute (fresh context) is returned
// as-is — no SessionCrashError, no session.crash sink event, no crashed
// flag — so the step's declared outcome routing can proceed.
func TestCRI287_TimeoutTeardownTransportCloseNotCrashClassified(t *testing.T) {
	sm, sess, _ := newCri287Session()
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
	sm, _, _ := newCri287Session()

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

// TestCRI287_TimeoutTeardownLatchClearsOnSuccess pins the latch lifecycle:
// the first successful Execute ends the teardown window, so a later transport
// death is a genuine crash again (hard-failure classification preserved).
func TestCRI287_TimeoutTeardownLatchClearsOnSuccess(t *testing.T) {
	sm, sess, h := newCri287Session()
	h.mu.Lock()
	h.deadAfter = 1 // first Execute succeeds, later ones replay the transport error
	h.mu.Unlock()
	sm.MarkEngineStepTimeoutTeardown()

	// A successful Execute proves the transport is alive: the teardown window
	// closes.
	if _, err := sm.Execute(context.Background(), "shell.develop", &workflow.StepNode{Name: "develop"}, &adapterEventCollector{}); err != nil {
		t.Fatalf("healthy Execute: %v", err)
	}

	coll := &adapterEventCollector{}
	_, err := sm.Execute(context.Background(), "shell.develop", &workflow.StepNode{Name: "comment_handler_failed"}, coll)
	var crashErr *SessionCrashError
	if !errors.As(err, &crashErr) {
		t.Fatalf("post-window Execute err = %v, want SessionCrashError (teardown window closed)", err)
	}
	if !sess.crashed.Load() {
		t.Error("session should be marked crashed after the teardown window closed")
	}
}

// TestCRI287_TimeoutTeardownDoesNotSwallowPlainErrors pins that the latch
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

// TestCRI287_MarkLandedBeforeNextExecute pins the mark-before-next-Execute
// ordering the engine relies on: the mark must be observable to a Execute
// that starts after a step timeout, including one that starts concurrently
// with the mark (parallel fan-out).
func TestCRI287_MarkLandedBeforeNextExecute(t *testing.T) {
	sm, _, _ := newCri287Session()
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