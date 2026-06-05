package adapterhost

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapter/secrets"
	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/brokenbots/criteria/workflow"
)

// loggingMockHandle is a mock Handle that also implements LogStreamStarter.
type loggingMockHandle struct {
	executeFunc func(ctx context.Context, sessionID string, step *workflow.StepNode, sink adapter.EventSink) (adapter.Result, error)
	logEvents   []*v2.LogEvent
	mu          sync.Mutex
	started     bool
	cancelled   bool
	emitTrigger chan struct{} // when closed, the log goroutine starts emitting
}

func (m *loggingMockHandle) Info(ctx context.Context) (Info, error) { return Info{}, nil }
func (m *loggingMockHandle) OpenSession(ctx context.Context, id string, config, secrets map[string]string) error {
	return nil
}
func (m *loggingMockHandle) Execute(ctx context.Context, sessionID string, step *workflow.StepNode, sink adapter.EventSink) (adapter.Result, error) {
	if m.executeFunc != nil {
		return m.executeFunc(ctx, sessionID, step, sink)
	}
	return adapter.Result{Outcome: "success"}, nil
}
func (m *loggingMockHandle) CloseSession(ctx context.Context, id string) error { return nil }
func (m *loggingMockHandle) Kill()                                             {}
func (m *loggingMockHandle) Pause(context.Context, string) error               { return nil }
func (m *loggingMockHandle) Resume(context.Context, string) error              { return nil }
func (m *loggingMockHandle) Inspect(context.Context, string) (*v2.InspectResponse, error) {
	return &v2.InspectResponse{}, nil
}
func (m *loggingMockHandle) Snapshot(context.Context, string) (*v2.SnapshotResponse, error) {
	return &v2.SnapshotResponse{}, nil
}
func (m *loggingMockHandle) Restore(context.Context, string, []byte, uint32) error { return nil }

func (m *loggingMockHandle) StartLogStream(ctx context.Context, sessionID string, sink LogEventSink) (func(), error) {
	m.mu.Lock()
	m.started = true
	m.mu.Unlock()
	logCtx, cancel := context.WithCancel(ctx)
	go func() {
		if m.emitTrigger != nil {
			select {
			case <-m.emitTrigger:
			case <-logCtx.Done():
				return
			}
		}
		for _, ev := range m.logEvents {
			if logCtx.Err() != nil {
				return
			}
			_ = sink.Emit(ev)
		}
		// Block until cancelled.
		<-logCtx.Done()
	}()
	return func() {
		cancel()
		m.mu.Lock()
		m.cancelled = true
		m.mu.Unlock()
	}, nil
}

func TestSessionManager_LogStreamStartsAtOpen(t *testing.T) {
	h := &loggingMockHandle{}
	sm := NewSessionManager(nil)

	// StartLogStream should not have been called yet.
	if h.started {
		t.Fatal("expected log stream not started before Open")
	}

	_ = sm.registerSession(context.Background(), "agent", "test", "fail", nil, nil, nil, nil, h, nil, "")
	defer sm.Close(context.Background(), "agent")

	if !h.started {
		t.Fatal("expected log stream started after registerSession")
	}
}

func TestSessionManager_LogStreamCancelledAtClose(t *testing.T) {
	h := &loggingMockHandle{}
	sm := NewSessionManager(nil)

	_ = sm.registerSession(context.Background(), "agent", "test", "fail", nil, nil, nil, nil, h, nil, "")

	_ = sm.Close(context.Background(), "agent")

	if !h.cancelled {
		t.Fatal("expected log stream cancelled at Close")
	}
}

func TestSessionManager_Integration_100Logs10Events_Redaction(t *testing.T) {
	reg := secrets.NewRegistry()
	reg.Register("supersecret")

	trigger := make(chan struct{})
	h := &loggingMockHandle{
		emitTrigger: trigger,
		logEvents: func() []*v2.LogEvent {
			var evs []*v2.LogEvent
			base := time.Now()
			for i := 0; i < 100; i++ {
				evs = append(evs, &v2.LogEvent{
					StreamName: "stdout",
					Line:       []byte(fmt.Sprintf("log-%d supersecret\n", i)),
					Timestamp:  timestamppb.New(base.Add(time.Duration(i) * time.Millisecond)),
				})
			}
			return evs
		}(),
		executeFunc: func(ctx context.Context, sessionID string, step *workflow.StepNode, sink adapter.EventSink) (adapter.Result, error) {
			// Trigger log emission now that currentSink is set.
			close(trigger)
			// Keep Execute alive long enough for log delivery.
			time.Sleep(100 * time.Millisecond)
			base := time.Now()
			for i := 0; i < 10; i++ {
				sink.Adapter("ev", map[string]any{"i": i, "ts": base.Add(time.Duration(i*10+5) * time.Millisecond)})
			}
			return adapter.Result{Outcome: "success"}, nil
		},
	}
	sm := NewSessionManager(nil)
	sm.RedactionRegistry = reg
	_ = sm.registerSession(context.Background(), "agent", "test", "fail", nil, nil, nil, nil, h, nil, "")
	defer sm.Close(context.Background(), "agent")

	collector := &logEventCollector{}
	step := &workflow.StepNode{Name: "run"}
	_, err := sm.Execute(context.Background(), "agent", step, collector)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	// Wait for any trailing log goroutine delivery.
	time.Sleep(50 * time.Millisecond)

	if collector.logCount() != 100 {
		t.Fatalf("expected 100 log lines, got %d", collector.logCount())
	}
	if collector.adapterCount() != 10 {
		t.Fatalf("expected 10 adapter events, got %d", collector.adapterCount())
	}

	// Verify redaction: every log line should have "supersecret" replaced.
	for _, l := range collector.allLogs() {
		if strings.Contains(string(l.chunk), "supersecret") {
			t.Fatalf("expected secret redacted in %q", string(l.chunk))
		}
	}
}

// TestSessionManager_LogLinesRoutedToStepSink verifies that session-level log
// lines received during Execute are forwarded to the step's EventSink.
func TestSessionManager_LogLinesRoutedToStepSink(t *testing.T) {
	logReady := make(chan struct{})
	h := &loggingMockHandle{
		executeFunc: func(ctx context.Context, sessionID string, step *workflow.StepNode, sink adapter.EventSink) (adapter.Result, error) {
			// Signal that Execute is running so the log goroutine can emit.
			close(logReady)
			// Keep Execute alive long enough for log delivery.
			time.Sleep(100 * time.Millisecond)
			return adapter.Result{Outcome: "success"}, nil
		},
	}
	sm := NewSessionManager(nil)
	_ = sm.registerSession(context.Background(), "agent", "test", "fail", nil, nil, nil, nil, h, nil, "")
	defer sm.Close(context.Background(), "agent")

	// The log stream goroutine from registerSession is already running.
	// Start a goroutine that emits log events once Execute begins.
	sess := sm.sessions["agent"]
	go func() {
		<-logReady
		// Wait a tiny bit for currentSink to be set inside Execute.
		time.Sleep(10 * time.Millisecond)
		sess.currentSinkMu.Lock()
		sink := sess.currentSink
		sess.currentSinkMu.Unlock()
		if sink != nil {
			sink.Log("stdout", []byte("line1\n"))
			sink.Log("stdout", []byte("line2\n"))
		}
	}()

	collector := &logEventCollector{}
	step := &workflow.StepNode{Name: "run"}
	_, err := sm.Execute(context.Background(), "agent", step, collector)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	if collector.logCount() == 0 {
		t.Fatal("expected log lines to be routed to step sink")
	}
	var found int
	for _, l := range collector.allLogs() {
		if string(l.chunk) == "line1\n" || string(l.chunk) == "line2\n" {
			found++
		}
	}
	if found != 2 {
		t.Fatalf("expected both log lines in sink, got %v", collector.allLogs())
	}
}

// TestSessionManager_HeartbeatStall_DetectsCrash verifies that if no
// heartbeat has been received for >90s, Execute treats the session as crashed.
func TestSessionManager_HeartbeatStall_DetectsCrash(t *testing.T) {
	h := &loggingMockHandle{}
	sm := NewSessionManager(nil)
	_ = sm.registerSession(context.Background(), "agent", "test", "fail", nil, nil, nil, nil, h, nil, "")
	defer sm.Close(context.Background(), "agent")

	// Artificially set heartbeat to well in the past.
	sess := sm.sessions["agent"]
	sess.hbMonitor.RecordAt(time.Now().Add(-2 * time.Minute))

	collector := &adapterEventCollector{}
	step := &workflow.StepNode{Name: "run"}
	_, err := sm.Execute(context.Background(), "agent", step, collector)
	if err == nil {
		t.Fatal("expected heartbeat stall to produce an error")
	}
	// err should be a crash error, not a context cancellation.
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		t.Fatalf("expected crash error, got context error: %v", err)
	}
	if !collector.saw("session.crash") {
		t.Fatal("expected session.crash event on heartbeat stall")
	}
}

// logEventCollector is an EventSink that captures Log calls.
type logEventCollector struct {
	mu       sync.Mutex
	logs     []logEntry
	adapters []adapterEvent
}

type logEntry struct {
	stream string
	chunk  []byte
}

func (c *logEventCollector) Log(stream string, chunk []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.logs = append(c.logs, logEntry{stream: stream, chunk: append([]byte(nil), chunk...)})
}

func (c *logEventCollector) Adapter(kind string, data any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var payload map[string]any
	if m, ok := data.(map[string]any); ok {
		payload = m
	}
	c.adapters = append(c.adapters, adapterEvent{kind: kind, data: payload})
}

func (c *logEventCollector) logCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.logs)
}

func (c *logEventCollector) allLogs() []logEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]logEntry, len(c.logs))
	copy(out, c.logs)
	return out
}

func (c *logEventCollector) adapterCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.adapters)
}

var _ adapter.EventSink = (*logEventCollector)(nil)

// TestSessionManager_HeartbeatRecent_PreventsStall verifies that a recent
// heartbeat allows Execute to proceed normally.
func TestSessionManager_HeartbeatRecent_PreventsStall(t *testing.T) {
	h := &loggingMockHandle{}
	sm := NewSessionManager(nil)
	_ = sm.registerSession(context.Background(), "agent", "test", "fail", nil, nil, nil, nil, h, nil, "")
	defer sm.Close(context.Background(), "agent")

	// hbMonitor is seeded to now by startLogStream inside registerSession.
	step := &workflow.StepNode{Name: "run"}
	_, err := sm.Execute(context.Background(), "agent", step, &logEventCollector{})
	if err != nil {
		t.Fatalf("expected Execute to succeed with recent heartbeat, got %v", err)
	}
}

// recordingSlogHandler is a slog.Handler that captures every formatted record.
type recordingSlogHandler struct {
	mu      sync.Mutex
	records []string
}

func (h *recordingSlogHandler) Enabled(ctx context.Context, level slog.Level) bool { return true }

//nolint:gocritic // slog.Handler interface requires value receiver for Record.
func (h *recordingSlogHandler) Handle(ctx context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Message + " ")
	r.Attrs(func(a slog.Attr) bool {
		b.WriteString(fmt.Sprintf("%s=%s ", a.Key, a.Value.String()))
		return true
	})
	h.mu.Lock()
	h.records = append(h.records, b.String())
	h.mu.Unlock()
	return nil
}
func (h *recordingSlogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return h
}
func (h *recordingSlogHandler) WithGroup(name string) slog.Handler {
	return h
}
func (h *recordingSlogHandler) all() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, len(h.records))
	copy(out, h.records)
	return out
}

// TestSessionManager_IdleLogRedaction verifies that log lines emitted when no
// Execute is in flight are still redacted.
func TestSessionManager_IdleLogRedaction(t *testing.T) {
	reg := secrets.NewRegistry()
	reg.Register("hiddensecret")

	// Capture slog output so we can assert redaction.
	rec := &recordingSlogHandler{}
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(rec))
	defer slog.SetDefault(oldLogger)

	trigger := make(chan struct{})
	h := &loggingMockHandle{
		emitTrigger: trigger,
		logEvents: []*v2.LogEvent{
			{
				StreamName: "stdout",
				Line:       []byte("init message hiddensecret\n"),
				Timestamp:  timestamppb.Now(),
			},
		},
	}
	sm := NewSessionManager(nil)
	sm.RedactionRegistry = reg
	_ = sm.registerSession(context.Background(), "agent", "test", "fail", nil, nil, nil, nil, h, nil, "")
	defer sm.Close(context.Background(), "agent")

	sess := sm.sessions["agent"]
	// Start the log stream emission without Execute running.
	close(trigger)
	// Wait for log delivery to mergeBuf -> sessionLogAdapterSink -> slog.
	time.Sleep(100 * time.Millisecond)

	// Flush the merge buffer so the idle log line is delivered.
	if sess.mergeBuf != nil {
		sess.mergeBuf.Flush()
	}

	// Verify the secret is redacted in the captured slog output.
	found := false
	for _, line := range rec.all() {
		if strings.Contains(line, "[REDACTED]") {
			found = true
		}
		if strings.Contains(line, "hiddensecret") {
			t.Fatalf("secret 'hiddensecret' leaked in idle-path log output: %q", line)
		}
	}
	if !found {
		t.Fatal("expected redacted token [REDACTED] in idle-path slog output")
	}
}

// TestSessionManager_RespawnRestartsLogStream verifies that after a crash and
// respawn, the log stream is restarted and heartbeat tracking is reset.
func TestSessionManager_RespawnRestartsLogStream(t *testing.T) {
	trigger1 := make(chan struct{})
	trigger2 := make(chan struct{})

	// Original handle fails on first Execute.
	h1 := &loggingMockHandle{
		emitTrigger: trigger1,
		logEvents: []*v2.LogEvent{
			{StreamName: "stdout", Line: []byte("pre-respawn secret\n"), Timestamp: timestamppb.Now()},
		},
		executeFunc: func(ctx context.Context, sessionID string, step *workflow.StepNode, sink adapter.EventSink) (adapter.Result, error) {
			return adapter.Result{}, errors.New("connection reset")
		},
	}

	// Respawned handle succeeds.
	h2 := &loggingMockHandle{
		emitTrigger: trigger2,
		logEvents: []*v2.LogEvent{
			{StreamName: "stdout", Line: []byte("post-respawn secret\n"), Timestamp: timestamppb.Now()},
		},
		executeFunc: func(ctx context.Context, sessionID string, step *workflow.StepNode, sink adapter.EventSink) (adapter.Result, error) {
			return adapter.Result{Outcome: "success"}, nil
		},
	}

	loader := &mockLoaderForRespawn{handles: []*loggingMockHandle{h2}}
	sm := NewSessionManager(loader)
	sm.SetGraph(&workflow.FSMGraph{})
	_ = sm.registerSession(context.Background(), "agent", "test", OnCrashRespawn, nil, nil, nil, nil, h1, nil, "")
	defer sm.Close(context.Background(), "agent")

	step := &workflow.StepNode{Name: "run"}
	_, err := sm.Execute(context.Background(), "agent", step, &logEventCollector{})
	if err != nil {
		t.Fatalf("expected respawn+retry to succeed, got %v", err)
	}

	// After respawn, the new handle's log stream should have been started.
	if !h2.started {
		t.Fatal("expected log stream started on respawned handle")
	}
	// hbMonitor should have been reset to a recent time.
	sess := sm.sessions["agent"]
	lastHB := sess.hbMonitor.Last()
	if time.Since(lastHB) > 90*time.Second {
		t.Fatalf("expected heartbeat reset after respawn, got lastHB=%v", lastHB)
	}
}

// mockLoaderForRespawn is a Loader that returns the next handle from a slice.
type mockLoaderForRespawn struct {
	handles []*loggingMockHandle
	idx     int
}

func (m *mockLoaderForRespawn) Resolve(ctx context.Context, name string) (Handle, error) {
	if m.idx >= len(m.handles) {
		return nil, errors.New("no more handles")
	}
	h := m.handles[m.idx]
	m.idx++
	return h, nil
}
func (m *mockLoaderForRespawn) ResolveWithCustomizer(ctx context.Context, name string, customizer func(string, *exec.Cmd)) (Handle, error) {
	return m.Resolve(ctx, name)
}
func (m *mockLoaderForRespawn) ResolveWithRunnerFunc(ctx context.Context, name string, runner func(string, *exec.Cmd) error) (Handle, error) {
	return m.Resolve(ctx, name)
}
func (m *mockLoaderForRespawn) Shutdown(ctx context.Context) error { return nil }
