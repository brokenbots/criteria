package adapterhost

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/zclconf/go-cty/cty"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapter/secrets"
	"github.com/brokenbots/criteria/workflow"
)

type mockRedactionHandle struct {
	result adapter.Result
	// emit, when set, is invoked with the sink Execute receives — the
	// session-wrapped (redacting) sink — so tests can emit structured
	// events exactly as a real adapter would.
	emit func(sink adapter.EventSink)
}

func (m *mockRedactionHandle) Info(ctx context.Context) (Info, error) {
	return Info{}, nil
}
func (m *mockRedactionHandle) OpenSession(ctx context.Context, id string, config, secrets map[string]string) error {
	return nil
}
func (m *mockRedactionHandle) Execute(ctx context.Context, sessionID string, step *workflow.StepNode, sink adapter.EventSink, rejection *v2.ExecutionRejection) (adapter.Result, error) {
	if m.emit != nil {
		m.emit(sink)
	}
	return m.result, nil
}
func (m *mockRedactionHandle) CloseSession(ctx context.Context, id string) error {
	return nil
}
func (m *mockRedactionHandle) Kill()                                {}
func (m *mockRedactionHandle) Pause(context.Context, string) error  { return nil }
func (m *mockRedactionHandle) Resume(context.Context, string) error { return nil }
func (m *mockRedactionHandle) Inspect(context.Context, string) (*v2.InspectResponse, error) {
	return &v2.InspectResponse{}, nil
}
func (m *mockRedactionHandle) Snapshot(context.Context, string) (*v2.SnapshotResponse, error) {
	return &v2.SnapshotResponse{}, nil
}
func (m *mockRedactionHandle) Restore(context.Context, string, []byte, uint32) error { return nil }

// TestSessionManagerExecute_RegistersSensitiveOutputs verifies that when an
// adapter returns outputs and the step's OutputSchema marks a field as
// sensitive, the value is registered with the RedactionRegistry.
func TestSessionManagerExecute_RegistersSensitiveOutputs(t *testing.T) {
	reg := secrets.NewRegistry()
	sm := NewSessionManager(nil)
	sm.RedactionRegistry = reg

	// Manually inject a session with a mock handle.
	sess := &Session{
		Name:    "agent",
		Adapter: "test",
		handle: &mockRedactionHandle{
			result: adapter.Result{
				Outcome: "success",
				Outputs: map[string]cty.Value{
					"token":  cty.StringVal("secret123"),
					"public": cty.StringVal("hello"),
				},
			},
		},
		closing: atomic.Bool{},
	}
	sm.sessions["agent"] = sess

	step := &workflow.StepNode{
		Name: "run",
		OutputSchema: map[string]workflow.ConfigField{
			"token":  {Sensitive: true},
			"public": {Sensitive: false},
		},
	}

	_, err := sm.Execute(context.Background(), "agent", step, &adapterEventCollector{}, nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	if got := reg.Redact("secret123"); got != "[REDACTED]" {
		t.Errorf("expected sensitive output to be redacted, got %q", got)
	}
	if got := reg.Redact("hello"); got != "hello" {
		t.Errorf("expected non-sensitive output NOT to be redacted, got %q", got)
	}
}

// TestSessionManagerExecute_NoRegistryNilPanic verifies that Execute does not
// panic when RedactionRegistry is nil and the adapter returns sensitive outputs.
func TestSessionManagerExecute_NoRegistryNilPanic(t *testing.T) {
	sm := NewSessionManager(nil)
	// RedactionRegistry is nil.

	sess := &Session{
		Name:    "agent",
		Adapter: "test",
		handle: &mockRedactionHandle{
			result: adapter.Result{
				Outcome: "success",
				Outputs: map[string]cty.Value{
					"token": cty.StringVal("secret123"),
				},
			},
		},
		closing: atomic.Bool{},
	}
	sm.sessions["agent"] = sess

	step := &workflow.StepNode{
		Name: "run",
		OutputSchema: map[string]workflow.ConfigField{
			"token": {Sensitive: true},
		},
	}

	_, err := sm.Execute(context.Background(), "agent", step, &adapterEventCollector{}, nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
}

// TestSessionManagerExecute_MasksRegisteredSecretsInStructuredEvents is the
// sink-boundary round trip through the session manager: structured events an
// adapter emits on its session event sink must reach the caller's sink with
// every registered value masked. Two declaration orders are covered: a
// secret registered before the execute (e.g. a sensitive input registered
// when the session opened) and one registered by the same manager after an
// earlier execute's sensitive outputs — the handler returns its result
// before registerSensitiveOutputs runs, so an event in that execute would
// carry the value raw; the next execute must mask it.
func TestSessionManagerExecute_MasksRegisteredSecretsInStructuredEvents(t *testing.T) {
	reg := secrets.NewRegistry()
	reg.Register("input-secret-111")

	sm := NewSessionManager(nil)
	sm.RedactionRegistry = reg

	handle := &mockRedactionHandle{}
	step := &workflow.StepNode{
		Name: "run",
		OutputSchema: map[string]workflow.ConfigField{
			"out": {Sensitive: true},
			"c":   {Sensitive: false},
		},
	}
	newSession := func() *Session {
		return &Session{
			Name:    "agent",
			Adapter: "test",
			handle:  handle,
			closing: atomic.Bool{},
		}
	}

	t.Run("pre-declared secret masked during execute", func(t *testing.T) {
		sm.sessions["agent"] = newSession()
		handle.result = adapter.Result{Outcome: "success"}
		handle.emit = func(sink adapter.EventSink) {
			sink.Adapter("callee.started", map[string]any{
				"input": "input-secret-111",
				"note":  "safe",
			})
		}

		collector := &adapterEventCollector{}
		if _, err := sm.Execute(context.Background(), "agent", step, collector, nil); err != nil {
			t.Fatalf("execute: %v", err)
		}

		data, ok := collector.first("callee.started")
		if !ok {
			t.Fatal("callee.started event not captured")
		}
		if got := data["input"]; got != "[REDACTED]" {
			t.Errorf("input: got %v, want [REDACTED]", got)
		}
		if got := data["note"]; got != "safe" {
			t.Errorf("note: got %v, want safe", got)
		}
	})

	t.Run("sensitive output of an earlier execute masked in the next", func(t *testing.T) {
		// First execute: the handler returns a sensitive output; the
		// manager registers it after the handler returns.
		sm.sessions["agent"] = newSession()
		handle.result = adapter.Result{
			Outcome: "success",
			Outputs: map[string]cty.Value{
				"out": cty.StringVal("output-secret-444"),
				"c":   cty.StringVal("public-value"),
			},
		}
		handle.emit = nil
		first := &adapterEventCollector{}
		if _, err := sm.Execute(context.Background(), "agent", step, first, nil); err != nil {
			t.Fatalf("first execute: %v", err)
		}
		if got := reg.Redact("output-secret-444"); got != "[REDACTED]" {
			t.Fatalf("output-secret-444 should be registered after the first execute, redact=%q", got)
		}

		// Second execute: an event carries that value; the wrapped sink
		// must mask it because the registry now holds it.
		sm.sessions["agent"] = newSession()
		handle.result = adapter.Result{Outcome: "success"}
		handle.emit = func(sink adapter.EventSink) {
			sink.Adapter("callee.progress", map[string]any{"echo": "output-secret-444"})
		}
		second := &adapterEventCollector{}
		if _, err := sm.Execute(context.Background(), "agent", step, second, nil); err != nil {
			t.Fatalf("second execute: %v", err)
		}

		data, ok := second.first("callee.progress")
		if !ok {
			t.Fatal("callee.progress event not captured")
		}
		if got := data["echo"]; got != "[REDACTED]" {
			t.Errorf("echo: got %v, want [REDACTED]", got)
		}
	})
}
