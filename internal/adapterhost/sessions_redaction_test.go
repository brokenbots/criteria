package adapterhost

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/zclconf/go-cty/cty"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapter/secrets"
	v2 "github.com/brokenbots/criteria/sdk/pb/criteria/v2"
	"github.com/brokenbots/criteria/workflow"
)

type mockRedactionHandle struct {
	result adapter.Result
}

func (m *mockRedactionHandle) Info(ctx context.Context) (Info, error) {
	return Info{}, nil
}
func (m *mockRedactionHandle) OpenSession(ctx context.Context, id string, config, secrets map[string]string) error {
	return nil
}
func (m *mockRedactionHandle) Execute(ctx context.Context, sessionID string, step *workflow.StepNode, sink adapter.EventSink) (adapter.Result, error) {
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

	_, err := sm.Execute(context.Background(), "agent", step, &adapterEventCollector{})
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

	_, err := sm.Execute(context.Background(), "agent", step, &adapterEventCollector{})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
}
