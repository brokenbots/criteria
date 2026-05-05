package engine

import (
	"context"
	"fmt"
	"strings"
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

// failingInitPlugin tracks session operations but fails on init for a specific session ID
type failingInitPlugin struct {
	fakePlugin
	mu              sync.Mutex
	opensCount      int
	closesCount     int
	failOnSessionID string // which session to fail on
	shouldFail      bool   // whether to fail
}

func (p *failingInitPlugin) OpenSession(ctx context.Context, sessionID string, config map[string]string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.opensCount++
	if p.shouldFail && sessionID == p.failOnSessionID {
		return fmt.Errorf("adapter initialization failed")
	}
	return nil
}

func (p *failingInitPlugin) CloseSession(ctx context.Context, sessionID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closesCount++
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
    target = adapter.noop
    outcome "success" { next = "done" }
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
	eng := New(g, loader, sink)

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
    target = adapter.noop
    outcome "success" { next = "done" }
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
	eng := New(g, loader, sink)

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

// TestEngine_AdapterTeardownOnError verifies adapters are torn down even if step execution returns an error.
func TestEngine_AdapterTeardownOnError(t *testing.T) {
	g := compile(t, `
workflow "test" {
  version       = "0.1"
  initial_state = "fail_step"
  target_state  = "done"

  step "fail_step" {
    target = adapter.noop
    outcome "success" { next = "done" }
  }

  state "done" {
    terminal = true
    success  = true
  }
}`)

	trackingPlugin := &lifecycleTrackingPlugin{
		fakePlugin: fakePlugin{name: "noop", outcome: "success", err: fmt.Errorf("step execution failed")},
	}

	loader := &fakeLoader{plugins: map[string]plugin.Plugin{
		"noop": trackingPlugin,
	}}

	sink := &lifecycleTrackingSink{}
	eng := New(g, loader, sink)

	// Run - should fail due to step error, but adapters should still be torn down
	ctx := context.Background()
	err := eng.Run(ctx)
	// Error expected because the step returns an error
	if err == nil {
		t.Fatal("Run should have failed due to step error, but got nil")
	}

	// Verify adapter was still closed even though step errored
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
    target = adapter.noop_a
    outcome "success" { next = "step2" }
  }

  step "step2" {
    target = adapter.noop_b
    outcome "success" { next = "done" }
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
	eng := New(g, loader, sink)

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
	// Expected sequence: noop_a:opened, noop_b:opened, noop_b:closed, noop_a:closed
	// Filter to only opened and closed events
	var lifecycleEvents []string
	for _, evt := range sink.adapterLifecycleEvents {
		if strings.HasSuffix(evt, ":opened") || strings.HasSuffix(evt, ":closed") {
			lifecycleEvents = append(lifecycleEvents, evt)
		}
	}

	expected := []string{
		"noop_a.default:opened",
		"noop_b.default:opened",
		"noop_b.default:closed",
		"noop_a.default:closed",
	}

	if len(lifecycleEvents) < len(expected) {
		t.Errorf("expected at least %d lifecycle events (opened/closed only), got %d: %v",
			len(expected), len(lifecycleEvents), lifecycleEvents)
	} else {
		// Verify the exact sequence
		for i, expectedEvt := range expected {
			if i < len(lifecycleEvents) && lifecycleEvents[i] != expectedEvt {
				t.Errorf("at position %d: expected %q, got %q (filtered sequence: %v)",
					i, expectedEvt, lifecycleEvents[i], lifecycleEvents)
			}
		}
	}
}

// TestEngine_AdapterTeardownOnCancel verifies adapters are torn down even when the run context is cancelled,
// demonstrating that teardown uses context.WithoutCancel to complete cleanup.
func TestEngine_AdapterTeardownOnCancel(t *testing.T) {
	g := compile(t, `
workflow "test" {
  version       = "0.1"
  initial_state = "step1"
  target_state  = "done"

  step "step1" {
    target = adapter.noop
    outcome "success" { next = "done" }
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
	eng := New(g, loader, sink)

	// Create a cancelled context to simulate early cancellation
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	// Run with cancelled context - should fail due to cancellation
	_ = eng.Run(ctx)

	// Verify adapter was still closed despite context cancellation
	trackingPlugin.mu.Lock()
	closesCount := trackingPlugin.closesCount
	trackingPlugin.mu.Unlock()

	if closesCount != 1 {
		t.Errorf("expected adapter to be closed once despite cancellation, was closed %d times", closesCount)
	}
}

// TestEngine_AdapterInitFailureRollsBack verifies that when a second adapter fails to initialize,
// all previously provisioned adapters are rolled back in reverse order.
func TestEngine_AdapterInitFailureRollsBack(t *testing.T) {
	g := compile(t, `
workflow "test" {
  version       = "0.1"
  initial_state = "step1"
  target_state  = "done"

  step "step1" {
    target = adapter.noop_a
    outcome "success" { next = "step2" }
  }

  step "step2" {
    target = adapter.noop_b
    outcome "success" { next = "done" }
  }

  state "done" {
    terminal = true
    success  = true
  }
}`)

	trackingA := &lifecycleTrackingPlugin{
		fakePlugin: fakePlugin{name: "noop_a", outcome: "success"},
	}

	// noop_b will fail to initialize
	failingPlugin := &failingInitPlugin{
		fakePlugin:      fakePlugin{name: "noop_b", outcome: "success"},
		failOnSessionID: "noop_b.default",
		shouldFail:      true,
	}

	loader := &fakeLoader{plugins: map[string]plugin.Plugin{
		"noop_a": trackingA,
		"noop_b": failingPlugin,
	}}

	sink := &lifecycleTrackingSink{}
	eng := New(g, loader, sink)

	// Run should fail due to adapter B init failure
	err := eng.Run(context.Background())
	if err == nil {
		t.Fatal("Run should have failed due to adapter B init failure, but got nil")
	}

	// Verify adapter A was opened and then closed (rolled back)
	trackingA.mu.Lock()
	aOpens := trackingA.opensCount
	aCloses := trackingA.closesCount
	trackingA.mu.Unlock()

	if aOpens != 1 {
		t.Errorf("adapter A: expected 1 open, got %d", aOpens)
	}
	if aCloses != 1 {
		t.Errorf("adapter A: expected 1 close (rollback), got %d", aCloses)
	}

	// Verify adapter B was attempted to open but never closed
	failingPlugin.mu.Lock()
	bOpens := failingPlugin.opensCount
	bCloses := failingPlugin.closesCount
	failingPlugin.mu.Unlock()

	if bOpens != 1 {
		t.Errorf("adapter B: expected 1 open attempt, got %d", bOpens)
	}
	if bCloses != 0 {
		t.Errorf("adapter B: expected 0 closes (never opened), got %d", bCloses)
	}
}
