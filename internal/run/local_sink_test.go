package run

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/brokenbots/criteria/internal/engine"
)

type sinkLine struct {
	Seq         int64           `json:"seq"`
	RunID       string          `json:"run_id"`
	PayloadType string          `json:"payload_type"`
	Payload     json.RawMessage `json:"payload"`
}

func TestLocalSink_EncodesNDJSONAndMonotonicSeq(t *testing.T) {
	var buf bytes.Buffer
	checkpointCalls := 0
	sink := &LocalSink{
		RunID: "run-local-1",
		Out:   &buf,
		CheckpointFn: func(step string, attempt int) {
			checkpointCalls++
			if step != "step1" || attempt != 1 {
				t.Fatalf("unexpected checkpoint call: step=%s attempt=%d", step, attempt)
			}
		},
	}

	sink.OnRunStarted("wf", "step1")
	sink.OnStepEntered("step1", "noop", 1)
	stepSink := sink.StepEventSink("step1")
	stepSink.Log("stdout", []byte("hello\n"))
	stepSink.Adapter("custom.event", map[string]any{"k": "v"})
	sink.OnStepOutcome("step1", "success", 17*time.Millisecond, nil)
	sink.OnStepTransition("step1", "done", "success")
	sink.OnRunCompleted("done", true)

	if checkpointCalls != 1 {
		t.Fatalf("checkpoint calls: got %d want 1", checkpointCalls)
	}

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 7 {
		t.Fatalf("line count: got %d want 7", len(lines))
	}

	wantTypes := []string{"RunStarted", "StepEntered", "StepLog", "AdapterEvent", "StepOutcome", "StepTransition", "RunCompleted"}
	for i, line := range lines {
		var got sinkLine
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Fatalf("line %d json: %v", i+1, err)
		}
		if got.Seq != int64(i+1) {
			t.Fatalf("line %d seq: got %d want %d", i+1, got.Seq, i+1)
		}
		if got.RunID != "run-local-1" {
			t.Fatalf("line %d run_id: got %q", i+1, got.RunID)
		}
		if got.PayloadType != wantTypes[i] {
			t.Fatalf("line %d payload_type: got %q want %q", i+1, got.PayloadType, wantTypes[i])
		}
		if len(got.Payload) == 0 {
			t.Fatalf("line %d payload must not be empty", i+1)
		}
	}
}

func TestLocalSink_OnAdapterLifecycleEvent(t *testing.T) {
	var buf bytes.Buffer
	sink := &LocalSink{RunID: "run-local-1", Out: &buf}

	event := &engine.AdapterLifecycleEvent{
		RunID:             sink.RunID,
		ScopeName:         "root",
		ScopeInstanceID:   "11111111-1111-1111-1111-111111111111",
		AdapterName:       "noop",
		Digest:            "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ShimListenAddress: "127.0.0.1:0",
		TokenRef:          "/run/data/tokens/noop-root.token",
		Status:            "provision_wanted",
	}
	secretToken := "super-secret-token-value"

	sink.OnAdapterLifecycleEvent(event)

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected 1 ND-JSON line, got %d", len(lines))
	}

	var line sinkLine
	if err := json.Unmarshal([]byte(lines[0]), &line); err != nil {
		t.Fatalf("unmarshal line: %v", err)
	}
	if line.Seq != 1 {
		t.Errorf("seq: got %d want 1", line.Seq)
	}
	if line.PayloadType != "AdapterEvent" {
		t.Fatalf("payload_type: got %q want AdapterEvent", line.PayloadType)
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
	if payload.Adapter != "noop" {
		t.Errorf("adapter: got %q want noop", payload.Adapter)
	}
	if payload.Kind != "adapter.lifecycle.provision_wanted" {
		t.Errorf("kind: got %q want adapter.lifecycle.provision_wanted", payload.Kind)
	}

	wantData := map[string]string{
		"run_id":              "run-local-1",
		"scope_name":          "root",
		"scope_instance_id":   "11111111-1111-1111-1111-111111111111",
		"adapter":             "noop",
		"digest":              "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"shim_listen_address": "127.0.0.1:0",
		"token_ref":           "/run/data/tokens/noop-root.token",
	}
	for k, want := range wantData {
		got, ok := payload.Data[k]
		if !ok {
			t.Errorf("data missing %q", k)
			continue
		}
		if got != want {
			t.Errorf("data %q: got %v want %q", k, got, want)
		}
	}
	if len(payload.Data) != len(wantData) {
		t.Errorf("data field count: got %d want %d", len(payload.Data), len(wantData))
	}
	if strings.Contains(buf.String(), secretToken) {
		t.Errorf("buffer must not contain raw token value, only token_ref path")
	}

	// Second call with a different status should produce a new line and increment seq.
	sink.OnAdapterLifecycleEvent(&engine.AdapterLifecycleEvent{
		RunID:       sink.RunID,
		AdapterName: "noop",
		Status:      "released",
	})

	lines = strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 ND-JSON lines after second event, got %d", len(lines))
	}
	var second sinkLine
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatalf("unmarshal second line: %v", err)
	}
	if second.Seq != 2 {
		t.Errorf("second seq: got %d want 2", second.Seq)
	}
	var secondPayload adapterEventPayload
	if err := json.Unmarshal(second.Payload, &secondPayload); err != nil {
		t.Fatalf("unmarshal second payload: %v", err)
	}
	if secondPayload.Kind != "adapter.lifecycle.released" {
		t.Errorf("second kind: got %q want adapter.lifecycle.released", secondPayload.Kind)
	}
}
