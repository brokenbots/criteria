package adapterhost

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	criteriav2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/workflow"
)

// cri271CrashErr is the verbatim failure signature from CRI-271: the go-plugin
// shim's gRPC transport closing underneath a live adapter session.
var cri271CrashErr = errors.New("rpc error: code = Canceled desc = grpc: the client connection is closing")

// cri271Handle is a minimal Handle stub that records OpenSession calls and
// either fails every Execute with the CRI-271 signature (gen 1) or succeeds
// (gen 2), emulating a dead process being replaced by a fresh respawn.
type cri271Handle struct {
	name  string
	gen   int
	fails bool

	mu    sync.Mutex
	opens []string
}

func (h *cri271Handle) Info(context.Context) (Info, error) { return Info{Name: h.name}, nil }
func (h *cri271Handle) OpenSession(_ context.Context, name string, _, _ map[string]string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.opens = append(h.opens, name)
	return nil
}
func (h *cri271Handle) Execute(context.Context, string, *workflow.StepNode, adapter.EventSink) (adapter.Result, error) {
	if h.fails {
		return adapter.Result{Outcome: "failure"}, cri271CrashErr
	}
	return adapter.Result{Outcome: "success"}, nil
}
func (h *cri271Handle) CloseSession(context.Context, string) error { return nil }
func (h *cri271Handle) Kill()                                      {}
func (h *cri271Handle) Pause(context.Context, string) error        { return nil }
func (h *cri271Handle) Resume(context.Context, string) error       { return nil }
func (h *cri271Handle) Inspect(context.Context, string) (*criteriav2.InspectResponse, error) {
	return &criteriav2.InspectResponse{}, nil
}
func (h *cri271Handle) Snapshot(context.Context, string) (*criteriav2.SnapshotResponse, error) {
	return &criteriav2.SnapshotResponse{}, nil
}
func (h *cri271Handle) Restore(context.Context, string, []byte, uint32) error { return nil }

// cri271Loader hands out pre-built handle generations in order; Resolve is
// counted so tests can assert exactly one replacement process was spawned.
type cri271Loader struct {
	mu       sync.Mutex
	gens     []*cri271Handle
	idx      int
	resolves []string
}

func (l *cri271Loader) Resolve(_ context.Context, name string) (Handle, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.resolves = append(l.resolves, name)
	if l.idx < len(l.gens) {
		h := l.gens[l.idx]
		l.idx++
		return h, nil
	}
	return l.gens[len(l.gens)-1], nil
}

func (l *cri271Loader) Shutdown(context.Context) error { return nil }

func (l *cri271Loader) resolveCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.resolves)
}

// TestCRI271_ClassifySessionCrash pins the crash-reason vocabulary engine
// logs rely on (CRI-271 acceptance 1). The process-exited branch delegates to
// go-plugin's rpc client and needs a real subprocess, so it is covered
// indirectly by the message heuristics; every shape produced by the
// go-plugin/gRPC stack is pinned here.
func TestCRI271_ClassifySessionCrash(t *testing.T) {
	cases := []struct {
		errText string
		want    string
	}{
		{"rpc error: code = Canceled desc = grpc: the client connection is closing",
			CrashReasonTransportClosed},
		{"transport is closing", CrashReasonTransportClosed},
		{"heartbeat stall detected in log stream", CrashReasonHeartbeatStall},
		{"rpc error: code = Unavailable desc = connection error", CrashReasonEndpointUnavailable},
		{"write |1: broken pipe", CrashReasonStdioPipeBroken},
		{"EOF", CrashReasonStdioEOF},
		{"process terminated unexpectedly", CrashReasonProcessTerminated},
		{"some other weird failure", CrashReasonUnknownAdapterError},
	}
	for _, tc := range cases {
		got := classifySessionCrash(nil, errors.New(tc.errText))
		if got != tc.want {
			t.Errorf("classifySessionCrash(%q) = %q, want %q", tc.errText, got, tc.want)
		}
	}
	if got := classifySessionCrash(nil, nil); got != CrashReasonUnknown {
		t.Errorf("classifySessionCrash(nil, nil) = %q, want %q", got, CrashReasonUnknown)
	}
}

// TestCRI271_SessionIdleTracking verifies the idle-since-last-event baseline
// (CRI-271): a session with no recorded activity reports no idle claim, and
// noteActivity starts the observable-activity clock.
func TestCRI271_SessionIdleTracking(t *testing.T) {
	sess := &Session{Name: "s1", Adapter: "fake"}
	if _, ok := sess.idleSinceLastEvent(); ok {
		t.Fatal("fresh session should report no idle claim")
	}
	if got := idleStringOrEmpty(sess); got != "" {
		t.Errorf("idleStringOrEmpty(fresh) = %q, want empty", got)
	}
	args := sess.crashDiagnostics("x")
	if len(args) != 2 || args[0] != "crash_reason" {
		t.Errorf("crashDiagnostics(fresh) = %v, want only crash_reason", args)
	}

	sess.noteActivity()
	idle, ok := sess.idleSinceLastEvent()
	if !ok || idle < 0 {
		t.Fatalf("after noteActivity: idle=%v ok=%v", idle, ok)
	}
	if got := idleStringOrEmpty(sess); got == "" {
		t.Error("idleStringOrEmpty should be non-empty after activity")
	}
	args = sess.crashDiagnostics("y")
	if len(args) != 4 || args[2] != "idle_since_last_event" {
		t.Errorf("crashDiagnostics(after activity) = %v, want idle_since_last_event", args)
	}
}

// TestCRI271_HeartbeatStallThresholdEnv pins the operator-configurable stall
// threshold (CRI-271 acceptance 3): a valid duration is honored, empty,
// malformed, and non-positive values fall back to the built-in default (0).
func TestCRI271_HeartbeatStallThresholdEnv(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		t.Setenv("CRITERIA_SESSION_HEARTBEAT_STALL", "5m")
		if got := heartbeatStallThresholdFromEnv(); got != 5*time.Minute {
			t.Errorf("got %v, want 5m", got)
		}
		if sm := NewSessionManager(nil); sm.HeartbeatStallThreshold != 5*time.Minute {
			t.Errorf("NewSessionManager threshold = %v, want 5m", sm.HeartbeatStallThreshold)
		}
	})
	t.Run("trimmed", func(t *testing.T) {
		t.Setenv("CRITERIA_SESSION_HEARTBEAT_STALL", " 10m ")
		if got := heartbeatStallThresholdFromEnv(); got != 10*time.Minute {
			t.Errorf("got %v, want 10m", got)
		}
	})
	for _, v := range []string{"", "garbage", "-1s", "0s"} {
		t.Run("fallback_"+v, func(t *testing.T) {
			t.Setenv("CRITERIA_SESSION_HEARTBEAT_STALL", v)
			if got := heartbeatStallThresholdFromEnv(); got != 0 {
				t.Errorf("heartbeatStallThresholdFromEnv(%q) = %v, want 0", v, got)
			}
		})
	}
}

// TestCRI271_ExecuteCrashEmitsDiagnosableEvent replays the CRI-271 signature
// end to end through SessionManager.Execute: the crash WARN names the reason
// and the idle window, and the session.crash sink event carries the same
// fields so engine logs answer "which session died and why".
func TestCRI271_ExecuteCrashEmitsDiagnosableEvent(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(old) })

	sm := &SessionManager{loader: nil, sessions: map[string]*Session{}}
	h := &cri271Handle{name: "fake", gen: 1, fails: true}
	sess := &Session{Name: "fake.default", Adapter: "fake", handle: h}
	sm.mu.Lock()
	sm.sessions["fake.default"] = sess
	sm.mu.Unlock()
	sess.noteActivity() // adapter showed life at open time

	coll := &adapterEventCollector{}
	_, err := sm.Execute(context.Background(), "fake.default", &workflow.StepNode{Name: "develop"}, coll)

	var crashErr *SessionCrashError
	if !errors.As(err, &crashErr) || crashErr.Session != "fake.default" {
		t.Fatalf("Execute err = %v, want SessionCrashError for fake.default", err)
	}
	if !sess.crashed.Load() {
		t.Error("session should be marked crashed after the transport failure")
	}

	evt, ok := coll.first("session.crash")
	if !ok {
		t.Fatal("expected a session.crash sink event")
	}
	if evt["crash_reason"] != CrashReasonTransportClosed {
		t.Errorf("event crash_reason = %v", evt["crash_reason"])
	}
	if idle, _ := evt["idle_since_last_event"].(string); idle == "" {
		t.Errorf("event idle_since_last_event = %v, want non-empty", evt["idle_since_last_event"])
	}

	out := buf.String()
	if !strings.Contains(out, "adapter session crashed") ||
		!strings.Contains(out, "crash_reason") ||
		!strings.Contains(out, "idle_since_last_event") {
		t.Errorf("crash WARN log missing diagnostics, got:\n%s", out)
	}
}

// TestCRI271_ReopenCrashedSession covers the engine's recovery hook: one
// respawn replaces the dead process (resolveCount 1), the crashed flag is
// cleared, follow-on Executes succeed, a healthy session is a no-op, and an
// unknown session is an error wrapping ErrUnknownSession.
func TestCRI271_ReopenCrashedSession(t *testing.T) {
	gen1 := &cri271Handle{name: "fake", gen: 1, fails: true}
	gen2 := &cri271Handle{name: "fake", gen: 2}
	// idx starts at 1: gens[0] is the dead original already bound to the
	// session; the loader's first Resolve (the respawn) must hand out gen2.
	loader := &cri271Loader{gens: []*cri271Handle{gen1, gen2}, idx: 1}
	sm := &SessionManager{loader: loader, sessions: map[string]*Session{}}
	sess := &Session{Name: "fake.default", Adapter: "fake", handle: gen1}
	sm.mu.Lock()
	sm.sessions["fake.default"] = sess
	sm.mu.Unlock()

	coll := &adapterEventCollector{}
	_, err := sm.Execute(context.Background(), "fake.default", &workflow.StepNode{Name: "develop"}, coll)
	if err == nil {
		t.Fatal("expected the first Execute to fail with the crash signature")
	}

	ctx := context.Background()
	if err := sm.ReopenCrashedSession(ctx, "fake.default"); err != nil {
		t.Fatalf("ReopenCrashedSession: %v", err)
	}
	if got := loader.resolveCount(); got != 1 {
		t.Errorf("resolveCount = %d, want 1", got)
	}
	if gen2.opens == nil || len(gen2.opens) != 1 {
		t.Errorf("gen2 opens = %v, want one OpenSession", gen2.opens)
	}
	if sess.crashed.Load() {
		t.Error("crashed flag should be cleared after re-open")
	}
	if gen2H, ok := sess.handle.(*cri271Handle); !ok || gen2H != gen2 {
		t.Error("session handle should point at the replacement process")
	}

	// The re-opened session serves follow-on work.
	res, err := sm.Execute(ctx, "fake.default", &workflow.StepNode{Name: "comment_handler_failed"}, coll)
	if err != nil || res.Outcome != "success" {
		t.Fatalf("Execute after re-open: res=%v err=%v", res, err)
	}

	// Re-open of a healthy session is a no-op (no extra respawn).
	if err := sm.ReopenCrashedSession(ctx, "fake.default"); err != nil {
		t.Fatalf("ReopenCrashedSession(healthy): %v", err)
	}
	if got := loader.resolveCount(); got != 1 {
		t.Errorf("resolveCount after healthy re-open = %d, want 1", got)
	}

	// Unknown sessions are reported, not silently ignored.
	err = sm.ReopenCrashedSession(ctx, "missing")
	if !errors.Is(err, ErrUnknownSession) {
		t.Errorf("ReopenCrashedSession(unknown) = %v, want ErrUnknownSession wrap", err)
	}
}

// TestCRI271_ReopenSingleFlight pins the per-session single-flight guard:
// concurrent callers sharing a crashed reference must spawn exactly one
// replacement process.
func TestCRI271_ReopenSingleFlight(t *testing.T) {
	gen1 := &cri271Handle{name: "fake", gen: 1, fails: true}
	gen2 := &cri271Handle{name: "fake", gen: 2}
	loader := &cri271Loader{gens: []*cri271Handle{gen1, gen2}}
	sm := &SessionManager{loader: loader, sessions: map[string]*Session{}}
	sess := &Session{Name: "fanout", Adapter: "fake", handle: gen1}
	sm.mu.Lock()
	sm.sessions["fanout"] = sess
	sm.mu.Unlock()
	sess.crashed.Store(true)

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := sm.ReopenCrashedSession(context.Background(), "fanout"); err != nil {
				t.Errorf("ReopenCrashedSession: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := loader.resolveCount(); got != 1 {
		t.Errorf("resolveCount = %d, want 1 (single-flight)", got)
	}
	if sess.crashed.Load() {
		t.Error("crashed flag should be cleared after re-open")
	}
}

// TestCRI271_ReopenClosingSessionRejected pins the closing-session guard:
// a session being torn down cannot be re-opened.
func TestCRI271_ReopenClosingSessionRejected(t *testing.T) {
	sm := &SessionManager{loader: &cri271Loader{}, sessions: map[string]*Session{}}
	sess := &Session{Name: "going", Adapter: "fake"}
	sess.closing.Store(true)
	sess.crashed.Store(true)
	sm.mu.Lock()
	sm.sessions["going"] = sess
	sm.mu.Unlock()

	err := sm.ReopenCrashedSession(context.Background(), "going")
	if err == nil || !strings.Contains(err.Error(), "closing") {
		t.Errorf("ReopenCrashedSession(closing) = %v, want closing error", err)
	}
}
