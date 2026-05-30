package adapterhost

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapter/secrets"
	v2 "github.com/brokenbots/criteria/sdk/pb/criteria/v2"
	"github.com/brokenbots/criteria/workflow"
	"google.golang.org/protobuf/types/known/timestamppb"
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
func (m *loggingMockHandle) Kill() {}

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

	_ = sm.registerSession(context.Background(), "agent", "test", "fail", nil, nil, nil, h, nil)
	defer sm.Close(context.Background(), "agent")

	if !h.started {
		t.Fatal("expected log stream started after registerSession")
	}
}

func TestSessionManager_LogStreamCancelledAtClose(t *testing.T) {
	h := &loggingMockHandle{}
	sm := NewSessionManager(nil)

	_ = sm.registerSession(context.Background(), "agent", "test", "fail", nil, nil, nil, h, nil)

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
	_ = sm.registerSession(context.Background(), "agent", "test", "fail", nil, nil, nil, h, nil)
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
	_ = sm.registerSession(context.Background(), "agent", "test", "fail", nil, nil, nil, h, nil)
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
	_ = sm.registerSession(context.Background(), "agent", "test", "fail", nil, nil, nil, h, nil)
	defer sm.Close(context.Background(), "agent")

	// Artificially set lastHeartbeat to well in the past.
	sess := sm.sessions["agent"]
	sess.lastHeartbeat.Store(time.Now().Add(-2 * time.Minute).UnixNano())

	collector := &adapterEventCollector{}
	step := &workflow.StepNode{Name: "run"}
	_, err := sm.Execute(context.Background(), "agent", step, collector)
	if err == nil {
		t.Fatal("expected heartbeat stall to produce an error")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		// Should be a crash error, not a context error.
	}
	if !collector.saw("session.crash") {
		t.Fatal("expected session.crash event on heartbeat stall")
	}
}

// logEventCollector is an EventSink that captures Log calls.
type logEventCollector struct {
	mu    sync.Mutex
	logs  []logEntry
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

func (c *logEventCollector) saw(kind string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, a := range c.adapters {
		if a.kind == kind {
			return true
		}
	}
	return false
}

func (c *logEventCollector) adapterCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.adapters)
}

func (c *logEventCollector) allAdapters() []adapterEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]adapterEvent, len(c.adapters))
	copy(out, c.adapters)
	return out
}

var _ adapter.EventSink = (*logEventCollector)(nil)
