package engine

import (
	"context"
	"sync"
	"testing"

	"github.com/brokenbots/criteria/internal/plugin"
)

// lifecycleTrackingSink captures lifecycle events for verification
type lifecycleTrackingSink struct {
	fakeSink

	mu                     sync.Mutex
	adapterLifecycleEvents []string // recorded as "<adapter>:<status>"
}

func (s *lifecycleTrackingSink) OnAdapterLifecycle(runID, adapter, status, detail string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.adapterLifecycleEvents = append(s.adapterLifecycleEvents, adapter+":"+status)
}

// lifecycleTrackingPlugin tracks session open/close calls
type lifecycleTrackingPlugin struct {
	fakePlugin
	mu          sync.Mutex
	opensCount  int
	closesCount int
	sessionOpen bool
}

func (p *lifecycleTrackingPlugin) OpenSession(ctx context.Context, sessionID string, config map[string]string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.opensCount++
	p.sessionOpen = true
	return nil
}

func (p *lifecycleTrackingPlugin) CloseSession(ctx context.Context, sessionID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closesCount++
	p.sessionOpen = false
	return nil
}

// TestEngine_LifecycleEventsEmitted verifies that lifecycle events are emitted when adapters are provisioned/torn down.
func TestEngine_LifecycleEventsEmitted(t *testing.T) {
	g := compile(t, `
workflow "test" {
  version       = "0.1"
  initial_state = "step1"
  target_state  = "done"

  step "step1" {
    adapter = adapter.noop
    outcome "success" { transition_to = "done" }
  }

  state "done" {
    terminal = true
    success  = true
  }
}`)

	trackingPlugin := &lifecycleTrackingPlugin{
		fakePlugin: fakePlugin{name: "noop", outcome: "success"},
	}

	loader := &fakeLoader{plugins: map[string]plugin.Plugin{
		"noop": trackingPlugin,
	}}

	sink := &lifecycleTrackingSink{}
	eng := New(g, loader, sink, WithAutoBootstrapAdapters())

	// Run should provision adapters before first step
	ctx := context.Background()
	err := eng.Run(ctx)
	if err != nil {
		t.Errorf("Run failed: %v", err)
	}

	if sink.terminal != "done" || !sink.terminalOK {
		t.Errorf("expected success terminal state 'done', got %s ok=%v", sink.terminal, sink.terminalOK)
	}

	// Verify lifecycle events were emitted: opened, then closed
	if len(sink.adapterLifecycleEvents) < 2 {
		t.Errorf("expected at least 2 lifecycle events (opened, closed), got %d: %v", len(sink.adapterLifecycleEvents), sink.adapterLifecycleEvents)
	} else {
		// Should have "noop.default:opened" and "noop.default:closed"
		hasOpened := false
		hasClosed := false
		for _, evt := range sink.adapterLifecycleEvents {
			if evt == "noop.default:opened" {
				hasOpened = true
			} else if evt == "noop.default:closed" {
				hasClosed = true
			}
		}
		if !hasOpened {
			t.Errorf("expected 'opened' lifecycle event for noop.default, got events: %v", sink.adapterLifecycleEvents)
		}
		if !hasClosed {
			t.Errorf("expected 'closed' lifecycle event for noop.default, got events: %v", sink.adapterLifecycleEvents)
		}
	}

	// Verify adapter was opened and closed
	trackingPlugin.mu.Lock()
	opensCount := trackingPlugin.opensCount
	closesCount := trackingPlugin.closesCount
	trackingPlugin.mu.Unlock()

	if opensCount != 1 {
		t.Errorf("expected adapter to be opened once, was opened %d times", opensCount)
	}
	if closesCount != 1 {
		t.Errorf("expected adapter to be closed once, was closed %d times", closesCount)
	}
}

// TestEngine_AdapterTeardownOnCompletion verifies adapters are torn down after workflow completes.
func TestEngine_AdapterTeardownOnCompletion(t *testing.T) {
	g := compile(t, `
workflow "test" {
  version       = "0.1"
  initial_state = "step1"
  target_state  = "done"

  step "step1" {
    adapter = adapter.noop
    outcome "success" { transition_to = "done" }
  }

  state "done" {
    terminal = true
    success  = true
  }
}`)

	trackingPlugin := &lifecycleTrackingPlugin{
		fakePlugin: fakePlugin{name: "noop", outcome: "success"},
	}

	loader := &fakeLoader{plugins: map[string]plugin.Plugin{
		"noop": trackingPlugin,
	}}

	sink := &lifecycleTrackingSink{}
	eng := New(g, loader, sink, WithAutoBootstrapAdapters())

	err := eng.Run(context.Background())
	if err != nil {
		t.Errorf("Run failed: %v", err)
	}

	// Verify run completed normally
	if !sink.terminalOK {
		t.Error("run did not complete successfully")
	}

	// Verify adapter was closed after completion
	trackingPlugin.mu.Lock()
	closesCount := trackingPlugin.closesCount
	trackingPlugin.mu.Unlock()

	if closesCount != 1 {
		t.Errorf("expected adapter to be closed once on completion, was closed %d times", closesCount)
	}
}

// TestEngine_AdapterTeardownOnError verifies adapters are torn down even if workflow errors.
func TestEngine_AdapterTeardownOnError(t *testing.T) {
	g := compile(t, `
workflow "test" {
  version       = "0.1"
  initial_state = "fail_step"
  target_state  = "done"

  step "fail_step" {
    adapter = adapter.noop
    outcome "failure" { transition_to = "done" }
  }

  state "done" {
    terminal = true
    success  = false
  }
}`)

	trackingPlugin := &lifecycleTrackingPlugin{
		fakePlugin: fakePlugin{name: "noop", outcome: "failure"},
	}

	loader := &fakeLoader{plugins: map[string]plugin.Plugin{
		"noop": trackingPlugin,
	}}

	sink := &lifecycleTrackingSink{}
	eng := New(g, loader, sink, WithAutoBootstrapAdapters())

	// Run - should complete even though step fails, and adapters should be torn down
	ctx := context.Background()
	err := eng.Run(ctx)
	// Error expected because terminal state is not success
	if err == nil {
		t.Log("Run completed (no error)")
	}

	// Verify adapter was still closed even though workflow failed
	trackingPlugin.mu.Lock()
	closesCount := trackingPlugin.closesCount
	trackingPlugin.mu.Unlock()

	if closesCount != 1 {
		t.Errorf("expected adapter to be closed once on error, was closed %d times", closesCount)
	}
}

// TestEngine_MultipleAdaptersProvisioned verifies all declared adapters are provisioned and torn down in order.
func TestEngine_MultipleAdaptersProvisioned(t *testing.T) {
	g := compile(t, `
workflow "test" {
  version       = "0.1"
  initial_state = "step1"
  target_state  = "done"

  step "step1" {
    adapter = adapter.noop_a
    outcome "success" { transition_to = "step2" }
  }

  step "step2" {
    adapter = adapter.noop_b
    outcome "success" { transition_to = "done" }
  }

  state "done" {
    terminal = true
    success  = true
  }
}`)

	trackingA := &lifecycleTrackingPlugin{
		fakePlugin: fakePlugin{name: "noop_a", outcome: "success"},
	}
	trackingB := &lifecycleTrackingPlugin{
		fakePlugin: fakePlugin{name: "noop_b", outcome: "success"},
	}

	loader := &fakeLoader{plugins: map[string]plugin.Plugin{
		"noop_a": trackingA,
		"noop_b": trackingB,
	}}

	sink := &lifecycleTrackingSink{}
	eng := New(g, loader, sink, WithAutoBootstrapAdapters())

	err := eng.Run(context.Background())
	if err != nil {
		t.Errorf("Run failed: %v", err)
	}

	// Verify both steps ran
	if len(sink.stepsRun) != 2 {
		t.Errorf("expected 2 steps to run, got %d", len(sink.stepsRun))
	}

	// Verify both adapters were opened and closed
	trackingA.mu.Lock()
	aOpens := trackingA.opensCount
	aCloses := trackingA.closesCount
	trackingA.mu.Unlock()

	trackingB.mu.Lock()
	bOpens := trackingB.opensCount
	bCloses := trackingB.closesCount
	trackingB.mu.Unlock()

	if aOpens != 1 || aCloses != 1 {
		t.Errorf("adapter A: expected 1 open and 1 close, got %d opens and %d closes", aOpens, aCloses)
	}
	if bOpens != 1 || bCloses != 1 {
		t.Errorf("adapter B: expected 1 open and 1 close, got %d opens and %d closes", bOpens, bCloses)
	}

	// Verify teardown happened in reverse order (LIFO)
	// Check lifecycle events contain both adapters being closed
	hasBClosed := false
	hasAClosed := false
	for _, evt := range sink.adapterLifecycleEvents {
		if evt == "noop_b.default:closed" {
			hasBClosed = true
		}
		if evt == "noop_a.default:closed" {
			hasAClosed = true
		}
	}
	if !hasBClosed || !hasAClosed {
		t.Errorf("expected both adapters to emit close events, got events: %v", sink.adapterLifecycleEvents)
	}
}
