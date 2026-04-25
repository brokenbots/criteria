package plugin

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/brokenbots/overlord/overseer/internal/adapter"
	"github.com/brokenbots/overlord/workflow"
)

type recordingLoader struct {
	inner Loader

	mu      sync.Mutex
	plugins []Plugin
}

func (l *recordingLoader) Resolve(ctx context.Context, name string) (Plugin, error) {
	p, err := l.inner.Resolve(ctx, name)
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	l.plugins = append(l.plugins, p)
	l.mu.Unlock()
	return p, nil
}

func (l *recordingLoader) Shutdown(ctx context.Context) error { return l.inner.Shutdown(ctx) }

func (l *recordingLoader) lastPlugin() Plugin {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.plugins) == 0 {
		return nil
	}
	return l.plugins[len(l.plugins)-1]
}

type adapterEventCollector struct {
	mu    sync.Mutex
	kinds []string
}

func (c *adapterEventCollector) Log(string, []byte) {}

func (c *adapterEventCollector) Adapter(kind string, _ any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.kinds = append(c.kinds, kind)
}

func (c *adapterEventCollector) saw(kind string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, k := range c.kinds {
		if k == kind {
			return true
		}
	}
	return false
}

func TestSessionManagerOpenExecuteClose(t *testing.T) {
	pluginBin := buildNoopPlugin(t)
	base := NewLoaderWithDiscovery(func(string) (string, error) { return pluginBin, nil })
	loader := &recordingLoader{inner: base}
	t.Cleanup(func() {
		_ = loader.Shutdown(context.Background())
	})

	sm := NewSessionManager(loader)
	if err := sm.Open(context.Background(), "agent", "noop", OnCrashFail, map[string]string{"bootstrap": "1"}); err != nil {
		t.Fatalf("open: %v", err)
	}

	res, err := sm.Execute(context.Background(), "agent", &workflow.StepNode{Name: "run"}, &adapterEventCollector{})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res.Outcome != "success" {
		t.Fatalf("outcome=%q want success", res.Outcome)
	}

	if err := sm.Close(context.Background(), "agent"); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := sm.Close(context.Background(), "agent"); err != nil {
		t.Fatalf("idempotent close: %v", err)
	}
}

func TestSessionManagerUnknownExecuteAndDoubleOpen(t *testing.T) {
	pluginBin := buildNoopPlugin(t)
	loader := NewLoaderWithDiscovery(func(string) (string, error) { return pluginBin, nil })
	t.Cleanup(func() {
		_ = loader.Shutdown(context.Background())
	})

	sm := NewSessionManager(loader)
	_, err := sm.Execute(context.Background(), "missing", &workflow.StepNode{Name: "run"}, &adapterEventCollector{})
	if !errors.Is(err, ErrUnknownSession) {
		t.Fatalf("execute unknown err=%v", err)
	}
	if err := sm.Open(context.Background(), "agent", "noop", OnCrashFail, nil); err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := sm.Open(context.Background(), "agent", "noop", OnCrashFail, nil); !errors.Is(err, ErrSessionAlreadyOpen) {
		t.Fatalf("double open err=%v", err)
	}
}

func TestSessionManagerCrashPolicyFail(t *testing.T) {
	pluginBin := buildNoopPlugin(t)
	base := NewLoaderWithDiscovery(func(string) (string, error) { return pluginBin, nil })
	loader := &recordingLoader{inner: base}
	t.Cleanup(func() {
		_ = loader.Shutdown(context.Background())
	})

	sm := NewSessionManager(loader)
	if err := sm.Open(context.Background(), "agent", "noop", OnCrashFail, nil); err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := sm.Execute(context.Background(), "agent", &workflow.StepNode{Name: "first"}, &adapterEventCollector{}); err != nil {
		t.Fatalf("first execute: %v", err)
	}
	loader.lastPlugin().Kill()

	sink := &adapterEventCollector{}
	result, err := sm.Execute(context.Background(), "agent", &workflow.StepNode{Name: "second"}, sink)
	if err == nil {
		t.Fatal("expected crash error")
	}
	if result.Outcome != "failure" {
		t.Fatalf("outcome=%q want failure", result.Outcome)
	}
	if !sink.saw("session.crash") {
		t.Fatal("expected session.crash adapter event")
	}
}

func TestSessionManagerCrashPolicyRespawn(t *testing.T) {
	pluginBin := buildNoopPlugin(t)
	base := NewLoaderWithDiscovery(func(string) (string, error) { return pluginBin, nil })
	loader := &recordingLoader{inner: base}
	t.Cleanup(func() {
		_ = loader.Shutdown(context.Background())
	})

	sm := NewSessionManager(loader)
	if err := sm.Open(context.Background(), "agent", "noop", OnCrashRespawn, nil); err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := sm.Execute(context.Background(), "agent", &workflow.StepNode{Name: "first"}, &adapterEventCollector{}); err != nil {
		t.Fatalf("first execute: %v", err)
	}
	loader.lastPlugin().Kill()

	sink := &adapterEventCollector{}
	result, err := sm.Execute(context.Background(), "agent", &workflow.StepNode{Name: "second"}, sink)
	if err != nil {
		t.Fatalf("execute with respawn: %v", err)
	}
	if result.Outcome != "success" {
		t.Fatalf("outcome=%q want success", result.Outcome)
	}
	if !sink.saw("session.respawned") {
		t.Fatal("expected session.respawned adapter event")
	}
}

func TestSessionManagerCrashPolicyAbortRun(t *testing.T) {
	pluginBin := buildNoopPlugin(t)
	base := NewLoaderWithDiscovery(func(string) (string, error) { return pluginBin, nil })
	loader := &recordingLoader{inner: base}
	t.Cleanup(func() {
		_ = loader.Shutdown(context.Background())
	})

	sm := NewSessionManager(loader)
	if err := sm.Open(context.Background(), "agent", "noop", OnCrashAbortRun, nil); err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := sm.Execute(context.Background(), "agent", &workflow.StepNode{Name: "first"}, &adapterEventCollector{}); err != nil {
		t.Fatalf("first execute: %v", err)
	}
	loader.lastPlugin().Kill()

	_, err := sm.Execute(context.Background(), "agent", &workflow.StepNode{Name: "second"}, &adapterEventCollector{})
	var fatal *FatalRunError
	if !errors.As(err, &fatal) {
		t.Fatalf("error=%v want FatalRunError", err)
	}
}

var _ adapter.EventSink = (*adapterEventCollector)(nil)
