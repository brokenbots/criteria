package run

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/brokenbots/criteria/internal/engine"
)

// TestLocalSink_OnAdapterLifecycleEvent_ScopeSessionPending pins the
// local-mode wire encoding of the KB-232 half-window signal: the named
// scope_session_pending status encodes kind adapter.lifecycle.scope_session_pending
// with the shim's diagnostics counters present so a local ND-JSON tail shows
// dialed/rejected counts for the pending scope.
func TestLocalSink_OnAdapterLifecycleEvent_ScopeSessionPending(t *testing.T) {
	var buf bytes.Buffer
	sink := &LocalSink{RunID: "kb232-run-local", Out: &buf}

	sink.OnAdapterLifecycleEvent(&engine.AdapterLifecycleEvent{
		RunID:              sink.RunID,
		AdapterName:        "intake",
		AdapterType:        "shell",
		ScopeName:          "run_handler",
		ScopeInstanceID:    "9c1a2b3c-4d5e-6f70-8192-a3b4c5d6e7f8",
		EnvironmentName:    "prod",
		Status:             engine.ScopeSessionPendingStatus,
		SessionWaitSeconds: 35,
		SessionDials:       4,
		SessionRejections:  3,
	})

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected 1 ND-JSON line, got %d", len(lines))
	}
	var line sinkLine
	if err := json.Unmarshal([]byte(lines[0]), &line); err != nil {
		t.Fatalf("unmarshal line: %v", err)
	}
	if line.RunID != sink.RunID {
		t.Errorf("run_id: got %q want %q", line.RunID, sink.RunID)
	}
	if line.PayloadType != "AdapterEvent" {
		t.Errorf("payload_type: got %q want AdapterEvent", line.PayloadType)
	}

	type adapterEventPayload struct {
		Adapter string         `json:"adapter"`
		Kind    string         `json:"kind"`
		Data    map[string]any `json:"data"`
	}
	var payload adapterEventPayload
	if err := json.Unmarshal(line.Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload.Adapter != "intake" {
		t.Errorf("adapter: got %q want intake", payload.Adapter)
	}
	if payload.Kind != "adapter.lifecycle.scope_session_pending" {
		t.Errorf("kind: got %q want adapter.lifecycle.scope_session_pending", payload.Kind)
	}
	for _, f := range []struct {
		key  string
		want int
	}{
		{"session_wait_seconds", 35},
		{"session_dials", 4},
		{"session_rejections", 3},
	} {
		got, ok := payload.Data[f.key]
		if !ok {
			t.Errorf("data.%s missing", f.key)
			continue
		}
		num, ok := got.(float64)
		if !ok {
			t.Errorf("data.%s: got %T want a JSON number", f.key, got)
			continue
		}
		if int(num) != f.want {
			t.Errorf("data.%s: got %d want %d", f.key, int(num), f.want)
		}
	}
	if got, ok := payload.Data["scope_name"]; !ok || got != "run_handler" {
		t.Errorf("scope_name: got %v want run_handler", got)
	}
	if got, ok := payload.Data["environment_name"]; !ok || got != "prod" {
		t.Errorf("environment_name: got %v want prod", got)
	}
}

// TestLocalSink_OnAdapterLifecycleEvent_OtherStatusesHaveNoSessionFields pins
// that the session diagnostics stay exclusive to the scope_session_pending
// kind: no other adapter lifecycle status grows the session_* keys.
func TestLocalSink_OnAdapterLifecycleEvent_OtherStatusesHaveNoSessionFields(t *testing.T) {
	var buf bytes.Buffer
	sink := &LocalSink{RunID: "kb232-run-local", Out: &buf}

	sink.OnAdapterLifecycleEvent(&engine.AdapterLifecycleEvent{
		RunID:           sink.RunID,
		AdapterName:     "intake",
		AdapterType:     "shell",
		EnvironmentName: "prod",
		Status:          "provision_wanted",
	})

	var line sinkLine
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected 1 ND-JSON line, got %d", len(lines))
	}
	if err := json.Unmarshal([]byte(lines[0]), &line); err != nil {
		t.Fatalf("unmarshal line: %v", err)
	}
	type adapterEventPayload struct {
		Kind string         `json:"kind"`
		Data map[string]any `json:"data"`
	}
	var payload adapterEventPayload
	if err := json.Unmarshal(line.Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload.Kind != "adapter.lifecycle.provision_wanted" {
		t.Errorf("kind: got %q want adapter.lifecycle.provision_wanted", payload.Kind)
	}
	for _, key := range []string{"session_wait_seconds", "session_dials", "session_rejections"} {
		if _, ok := payload.Data[key]; ok {
			t.Errorf("provision_wanted must not carry %s", key)
		}
	}
}

// TestSink_OnAdapterLifecycleEvent_ScopeSessionPending pins the server-mode
// wire encoding (KB-232 item 3): the published AdapterEvent payload carries
// the session diagnostics so the orchestrator can read them off the
// lifecycle envelope the same way it reads provision events.
func TestSink_OnAdapterLifecycleEvent_ScopeSessionPending(t *testing.T) {
	fp := &fakePublisher{}
	s := &Sink{RunID: "kb232-run-1", Client: fp, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	s.OnAdapterLifecycleEvent(&engine.AdapterLifecycleEvent{
		RunID:              "kb232-run-1",
		AdapterName:        "intake",
		AdapterType:        "shell",
		ScopeName:          "run_handler",
		ScopeInstanceID:    "9c1a2b3c-4d5e-6f70-8192-a3b4c5d6e7f8",
		EnvironmentType:    "remote",
		EnvironmentName:    "prod",
		Status:             engine.ScopeSessionPendingStatus,
		SessionWaitSeconds: 35,
		SessionDials:       4,
		SessionRejections:  3,
	})

	if len(fp.published) != 1 {
		t.Fatalf("expected 1 published envelope, got %d", len(fp.published))
	}
	ae := fp.published[0].GetAdapterEvent()
	if ae == nil {
		t.Fatalf("payload is %T, want *pb.AdapterEvent", fp.published[0].Payload)
	}
	if ae.Kind != "adapter.lifecycle.scope_session_pending" {
		t.Errorf("kind: got %q want adapter.lifecycle.scope_session_pending", ae.Kind)
	}
	if ae.Adapter != "intake" {
		t.Errorf("adapter: got %q want intake", ae.Adapter)
	}
	if got := ae.Data.Fields["adapter_type"].GetStringValue(); got != "shell" {
		t.Errorf("adapter_type: got %q want shell", got)
	}
	if got := ae.Data.Fields["scope_name"].GetStringValue(); got != "run_handler" {
		t.Errorf("scope_name: got %q want run_handler", got)
	}
	if got := ae.Data.Fields["scope_instance_id"].GetStringValue(); got != "9c1a2b3c-4d5e-6f70-8192-a3b4c5d6e7f8" {
		t.Errorf("scope_instance_id: got %q want the scope instance id", got)
	}
	if got := ae.Data.Fields["environment_name"].GetStringValue(); got != "prod" {
		t.Errorf("environment_name: got %q want prod", got)
	}
	if got := ae.Data.Fields["session_wait_seconds"].GetNumberValue(); got != 35 {
		t.Errorf("session_wait_seconds: got %v want 35", got)
	}
	if got := ae.Data.Fields["session_dials"].GetNumberValue(); got != 4 {
		t.Errorf("session_dials: got %v want 4", got)
	}
	if got := ae.Data.Fields["session_rejections"].GetNumberValue(); got != 3 {
		t.Errorf("session_rejections: got %v want 3", got)
	}
}
