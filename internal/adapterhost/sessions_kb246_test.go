package adapterhost

// sessions_kb246_test.go — KB-246 defect-4: the fleet's drift-recreate window
// closes the old peer pod's phone-home conns mid-execute while the recreated
// pod dials back the same session key the same second ("Canceled (connection
// closing)"). The in-flight step died immediately (on_crash=fail short-circuits
// before any re-wait), so a sequential subworkflow body failed in one step.
// The fix re-keys the transport-closed crash on a shim-dispatched (remote)
// session into the respawn arm: WaitForFreshHandle re-enters the wait, the
// recreated pod's fresh dial resolves (the stale handle is excluded), and the
// step re-executes once on the fresh handle. Recovery failure stays
// fail-closed; local sessions keep CRI-130 crash semantics; on_crash=
// abort_run keeps its hard-fail arm.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	criteriav2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/workflow"
)

// kb246FleetErr is the verbatim fleet signature from the KB-246 canary sweep:
// the drift-recreate closed the old pod's conns mid-wait ("session shell.sh
// crashed: Canceled (connection closing)").
var kb246FleetErr = errors.New("rpc error: code = Canceled desc = connection closing")

// kb246Loader never resolves: every session in this file is remote-bound, so
// a resolution attempt means the re-entry took the wrong arm.
type kb246Loader struct{}

func (kb246Loader) Resolve(context.Context, string) (Handle, error) {
	panic("kb246Loader.Resolve must not be called: remote sessions resolve via the shim")
}
func (kb246Loader) Shutdown(context.Context) error { return nil }

// kb246Handle stands in for a phone-home bridge handle. A dead handle
// replays the transport error on every Execute; a live one returns `outcome`.
type kb246Handle struct {
	name    string
	dead    bool
	outcome string

	mu       sync.Mutex
	executes int
}

func newKB246Handle(name string, dead bool, outcome string) *kb246Handle {
	return &kb246Handle{name: name, dead: dead, outcome: outcome}
}

func (h *kb246Handle) executions() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.executes
}

func (h *kb246Handle) Info(context.Context) (Info, error) { return Info{Name: h.name}, nil }
func (h *kb246Handle) OpenSession(context.Context, string, map[string]string, map[string]string) error {
	return nil
}
func (h *kb246Handle) Execute(_ context.Context, _ string, _ *workflow.StepNode, _ adapter.EventSink, _ *criteriav2.ExecutionRejection) (adapter.Result, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.executes++
	if h.dead {
		return adapter.Result{}, kb246FleetErr
	}
	return adapter.Result{Outcome: h.outcome}, nil
}
func (h *kb246Handle) CloseSession(context.Context, string) error { return nil }
func (h *kb246Handle) Kill()                                      {}
func (h *kb246Handle) Pause(context.Context, string) error        { return nil }
func (h *kb246Handle) Resume(context.Context, string) error       { return nil }
func (h *kb246Handle) Inspect(context.Context, string) (*criteriav2.InspectResponse, error) {
	return &criteriav2.InspectResponse{}, nil
}
func (h *kb246Handle) Snapshot(context.Context, string) (*criteriav2.SnapshotResponse, error) {
	return &criteriav2.SnapshotResponse{}, nil
}
func (h *kb246Handle) Restore(context.Context, string, []byte, uint32) error { return nil }

// kb246Shim mimics the peer phone-home shim serving the recreated pod. The
// fresh dial exists unless freshErr is set; freshHandleFunc produces the
// handle to hand back (so each subtest controls whether the stale handle is
// ever returned — it must not be).
type kb246Shim struct {
	mu        sync.Mutex
	freshErr  error
	fresh     Handle
	excluded  []Handle
	freshWait []string
	waitCalls []string
}

func (s *kb246Shim) WaitForHandle(context.Context, string, string) (Handle, error) {
	s.mu.Lock()
	s.waitCalls = append(s.waitCalls, "plain")
	s.mu.Unlock()
	return nil, errors.New("kb246Shim.WaitForHandle must not be called: re-entry uses the fresh form")
}

func (s *kb246Shim) WaitForFreshHandle(_ context.Context, adapterName, scope string, stale Handle) (Handle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.freshWait = append(s.freshWait, adapterName+":"+scope)
	s.excluded = append(s.excluded, stale)
	if s.freshErr != nil {
		return nil, s.freshErr
	}
	return s.fresh, nil
}

func (s *kb246Shim) RegisterScope(string, string)                      {}
func (s *kb246Shim) UnregisterScope(string)                            {}
func (s *kb246Shim) CloseHandle(context.Context, string, string) error { return nil }
func (s *kb246Shim) ListenAddr() string                                { return "127.0.0.1:1" }
func (s *kb246Shim) Stop(context.Context) error                        { return nil }

func (s *kb246Shim) freshWaitCalls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.freshWait...)
}

func (s *kb246Shim) excludedOf(idx int) Handle {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.excluded[idx]
}

// kb246Graph declares a definition-identity adapter bound to a remote
// environment — the post-KB-242 fleet shape (children keyed by
// "<type>.<name>", dots legal).
func kb246Graph() *workflow.FSMGraph {
	return &workflow.FSMGraph{
		Adapters: map[string]*workflow.AdapterNode{
			"shell.develop": {Name: "shell.develop", Type: "shell", Environment: "remote.fleet"},
		},
		AdapterOrder: []string{"shell.develop"},
		Environments: map[string]*workflow.EnvironmentNode{
			"remote.fleet": {Type: "remote", Name: "fleet"},
		},
	}
}

// newKB246RemoteSession builds a session manager with a remote-bound
// definition-identity session ("shell.develop") served by the given shim, a
// stale handle that dies immediately, and a fresh handle registered on the
// shim for the recreate window.
func newKB246RemoteSession(t *testing.T, onCrash string) (*SessionManager, *kb246Shim, *kb246Handle) {
	t.Helper()
	stale := newKB246Handle("stale", true, "")
	fresh := newKB246Handle("fresh", false, "success")
	shim := &kb246Shim{fresh: fresh}

	sm := NewSessionManager(kb246Loader{})
	sm.SetGraph(kb246Graph())
	sm.SetRemoteShimForEnv("remote.fleet", shim)

	sess := &Session{Name: "shell.develop", Adapter: "shell", handle: stale, ScopeInstanceID: "run-1", OnCrash: onCrash}
	sm.mu.Lock()
	sm.sessions["shell.develop"] = sess
	sm.mu.Unlock()
	sess.noteActivity() // the adapter showed life at open time
	return sm, shim, stale
}

// TestSessions_KB246_TransportClosedReentersRespawnWait pins the defect-4
// fix: a transport-closed crash on a remote session under the default
// on_crash policy re-enters the wait through WaitForFreshHandle (excluding
// the dead handle), swaps in the recreated pod's fresh handle, and re-executes
// the step once — instead of failing the step outright.
func TestSessions_KB246_TransportClosedReentersRespawnWait(t *testing.T) {
	sm, shim, stale := newKB246RemoteSession(t, "")
	coll := &adapterEventCollector{}

	result, err := sm.Execute(context.Background(), "shell.develop", &workflow.StepNode{Name: "get_branch_name"}, coll, nil)

	if err != nil {
		t.Fatalf("Execute err = %v, want the retried step to succeed on the fresh handle", err)
	}
	if result.Outcome != "success" {
		t.Errorf("result.Outcome = %q, want the fresh handle's verdict", result.Outcome)
	}
	if got := shim.freshWaitCalls(); len(got) != 1 || got[0] != "shell.develop:run-1" {
		t.Errorf("WaitForFreshHandle calls = %v, want exactly [shell.develop:run-1] (definition identity + scope)", got)
	}
	if excluded := shim.excludedOf(0); excluded != Handle(stale) {
		t.Errorf("stale handle not excluded from the fresh wait: got %v", excluded != nil)
	}
	sess, _ := sm.lookup("shell.develop")
	fh, ok := sess.handle.(*kb246Handle)
	if !ok {
		t.Fatalf("session handle = %T, want the swapped-in kb246Handle", sess.handle)
	}
	if fh == stale {
		t.Errorf("session still holds the stale handle")
	}
	if sess.crashed.Load() {
		t.Errorf("session marked crashed after a successful re-entry")
	}
	if stale.executions() != 1 {
		t.Errorf("stale handle executes = %d, want exactly 1 (the dropped attempt)", stale.executions())
	}
	if fh.executions() != 1 {
		t.Errorf("fresh handle runs = %d, want exactly 1 re-executed attempt", fh.executions())
	}
	if evt, ok := coll.first("session.respawned"); !ok {
		t.Errorf("session.respawned event not emitted; events = %+v", coll.events)
	} else if evt["session"] != "shell.develop" {
		t.Errorf("session.respawned payload = %+v, want session key", evt)
	}
	if _, ok := coll.first("session.crash"); ok {
		t.Errorf("session.crash event must not be emitted on a recovered re-entry")
	}
}

// TestSessions_KB246_TransportClosedReentryFailureStaysFailClosed pins the
// fail-closed arm: without a fresh dial within the shim's budget the step
// fails with the session-crash classification — same contract as before,
// with the recovery attempt's cause preserved in the wrap.
func TestSessions_KB246_TransportClosedReentryFailureStaysFailClosed(t *testing.T) {
	sm, shim, _ := newKB246RemoteSession(t, "")
	shim.mu.Lock()
	shim.freshErr = errors.New("budget exhausted waiting for a fresh dial")
	shim.mu.Unlock()
	coll := &adapterEventCollector{}

	result, err := sm.Execute(context.Background(), "shell.develop", &workflow.StepNode{Name: "get_branch_name"}, coll, nil)

	var crashErr *SessionCrashError
	if !errors.As(err, &crashErr) {
		t.Fatalf("Execute err = %v, want *SessionCrashError (fail-closed contract)", err)
	}
	// Discriminate: the re-entry DID run, and the recovery attempt failed —
	// the wrap carries the respawn failure and the original crash.
	if !strings.Contains(err.Error(), "respawn after crash failed") {
		t.Errorf("err = %v, want the respawn-attempt wrap (proves the re-entry ran)", err)
	}
	if result.Outcome != "failure" {
		t.Errorf("result.Outcome = %q, want failure", result.Outcome)
	}
	if got := shim.freshWaitCalls(); len(got) != 1 {
		t.Errorf("WaitForFreshHandle calls = %v, want exactly one re-entry", got)
	}
	if _, ok := coll.first("session.crash"); !ok {
		t.Errorf("session.crash event not emitted for the failed re-entry")
	}
	if sess, _ := sm.lookup("shell.develop"); !sess.crashed.Load() {
		t.Errorf("session must stay latched crashed after a failed re-entry")
	}
}

// TestSessions_KB246_TransportClosedLocalSessionStillFails pins the CRI-130
// boundary: a transport close on a LOCAL (in-host) session keeps the
// immediate crash-fail semantics — no re-entry, no respawn event, no shim.
func TestSessions_KB246_TransportClosedLocalSessionStillFails(t *testing.T) {
	sm := NewSessionManager(kb246Loader{})
	shim := &kb246Shim{}
	sm.SetRemoteShimForEnv("remote.fleet", shim)

	stale := newKB246Handle("stale", true, "")
	sess := &Session{Name: "shell.local", Adapter: "shell", handle: stale}
	sm.mu.Lock()
	sm.sessions["shell.local"] = sess
	sm.mu.Unlock()
	sess.noteActivity()
	coll := &adapterEventCollector{}

	result, err := sm.Execute(context.Background(), "shell.local", &workflow.StepNode{Name: "get_branch_name"}, coll, nil)

	var crashErr *SessionCrashError
	if !errors.As(err, &crashErr) {
		t.Fatalf("Execute err = %v, want *SessionCrashError (local sessions keep crash semantics)", err)
	}
	if result.Outcome != "failure" {
		t.Errorf("result.Outcome = %q, want failure", result.Outcome)
	}
	if calls := shim.freshWaitCalls(); len(calls) != 0 {
		t.Errorf("shim fresh waits = %v, want none for a local session", calls)
	}
	if _, ok := coll.first("session.respawned"); ok {
		t.Errorf("session.respawned event must not be emitted for a local session")
	}
	if _, ok := coll.first("session.crash"); !ok {
		t.Errorf("session.crash event not emitted for the local crash")
	}
}

// TestSessions_KB246_TransportClosedAbortRunStillAborts pins the policy
// ordering: on_crash=abort_run keeps its hard-fail arm ahead of the
// transport-closed re-entry.
func TestSessions_KB246_TransportClosedAbortRunStillAborts(t *testing.T) {
	sm, shim, _ := newKB246RemoteSession(t, OnCrashAbortRun)
	coll := &adapterEventCollector{}

	result, err := sm.Execute(context.Background(), "shell.develop", &workflow.StepNode{Name: "get_branch_name"}, coll, nil)

	var fatal *FatalRunError
	if !errors.As(err, &fatal) {
		t.Fatalf("Execute err = %v, want *FatalRunError (abort_run before re-entry)", err)
	}
	if result.Outcome != "failure" {
		t.Errorf("result.Outcome = %q, want failure", result.Outcome)
	}
	if calls := shim.freshWaitCalls(); len(calls) != 0 {
		t.Errorf("shim fresh waits = %v, want none under abort_run", calls)
	}
	if _, ok := coll.first("session.respawned"); ok {
		t.Errorf("session.respawned event must not be emitted under abort_run")
	}
}

// TestSessions_KB246_ClassifyConnectionClosing pins the classification fix:
// the fleet's recreate-window signature ("Canceled (connection closing)") is
// a transport close, not an unclassified adapter error.
func TestSessions_KB246_ClassifyConnectionClosing(t *testing.T) {
	h := &kb246Handle{name: "fake"}
	sess := &Session{Name: "shell.develop", Adapter: "shell", handle: h}

	if got := classifySessionCrash(sess, kb246FleetErr); got != CrashReasonTransportClosed {
		t.Errorf("classifySessionCrash(%v) = %q, want %q", kb246FleetErr, got, CrashReasonTransportClosed)
	}
}

// TestSessions_KB246_CanceledContextDoesNotReenter pins the cancellation
// guard: a cancelled step context returns the raw transport error through the
// cancellation early-exit — the step's own cancellation must not re-enter the
// wait or respawn a session.
func TestSessions_KB246_CanceledContextDoesNotReenter(t *testing.T) {
	sm, shim, _ := newKB246RemoteSession(t, "")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	coll := &adapterEventCollector{}

	result, err := sm.Execute(ctx, "shell.develop", &workflow.StepNode{Name: "get_branch_name"}, coll, nil)

	if !errors.Is(err, context.Canceled) && !errors.Is(err, kb246FleetErr) {
		t.Fatalf("Execute err = %v, want the raw transport/cancellation error without re-entry", err)
	}
	if result.Outcome != "" {
		t.Errorf("result = %+v with a raw error; adapter outcome must not be synthesized", result)
	}
	if calls := shim.freshWaitCalls(); len(calls) != 0 {
		t.Errorf("shim fresh waits = %v, want none under caller cancellation", calls)
	}
	if _, ok := coll.first("session.respawned"); ok {
		t.Errorf("session.respawned event must not be emitted under caller cancellation")
	}
}
