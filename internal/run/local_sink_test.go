package run

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/brokenbots/criteria/events"
	"github.com/brokenbots/criteria/internal/engine"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

type sinkLine struct {
	Seq         int64           `json:"seq"`
	RunID       string          `json:"run_id"`
	PayloadType string          `json:"payload_type"`
	Payload     json.RawMessage `json:"payload"`
}

// TestLocalSink_RunPausedResumedEvents asserts the NDJSON sink emits
// RunPaused only for boundary pauses (mode "external", CRI-255) and emits
// RunResumed on re-entry, sharing one event vocabulary with the server path.
func TestLocalSink_RunPausedResumedEvents(t *testing.T) {
	var buf bytes.Buffer
	sink := &LocalSink{RunID: "run-ctrl-local", Out: &buf}
	decode := func(t *testing.T) sinkLine {
		t.Helper()
		line := strings.TrimSpace(buf.String())
		var l sinkLine
		if err := json.Unmarshal([]byte(line), &l); err != nil {
			t.Fatalf("invalid NDJSON line %q: %v", line, err)
		}
		return l
	}

	sink.OnRunPaused("build", "signal", "ready")
	if buf.Len() != 0 {
		t.Fatalf("non-external pause emitted %q, want no RunPaused event", buf.String())
	}

	buf.Reset()
	sink.OnRunPaused("wait-for-merge", "external", "")
	l := decode(t)
	if l.PayloadType != "RunPaused" {
		t.Fatalf("payload type = %q, want RunPaused", l.PayloadType)
	}
	var paused pb.RunPaused
	if err := protojson.Unmarshal(l.Payload, &paused); err != nil {
		t.Fatalf("invalid RunPaused payload: %v", err)
	}
	if paused.Node != "wait-for-merge" || paused.Mode != "external" {
		t.Errorf("unexpected RunPaused payload: %+v", &paused)
	}

	buf.Reset()
	sink.OnRunResumed("wait-for-merge")
	l = decode(t)
	if l.PayloadType != "RunResumed" {
		t.Fatalf("payload type = %q, want RunResumed", l.PayloadType)
	}
	var resumed pb.RunResumed
	if err := protojson.Unmarshal(l.Payload, &resumed); err != nil {
		t.Fatalf("invalid RunResumed payload: %v", err)
	}
	if resumed.Node != "wait-for-merge" {
		t.Errorf("RunResumed node = %q, want wait-for-merge", resumed.Node)
	}
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

	// CRI-236: the token rides the wire in the provision event; the secret
	// value is deliberately part of the serialized line now.
	secretToken := "super-secret-token-value"
	event := &engine.AdapterLifecycleEvent{
		RunID:             sink.RunID,
		ScopeName:         "root",
		ScopeInstanceID:   "11111111-1111-1111-1111-111111111111",
		AdapterName:       "noop",
		AdapterType:       "noop",
		EnvironmentType:   "remote",
		EnvironmentName:   "prod",
		Digest:            "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ImageReference:    "ghcr.io/criteria-adapters/noop:1.2.3-image",
		ShimListenAddress: "127.0.0.1:0",
		TokenRef:          "/run/data/tokens/noop-root.token",
		Token:             secretToken,
		Status:            "provision_wanted",
	}

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
		"adapter_type":        "noop",
		"environment_type":    "remote",
		"environment_name":    "prod",
		"digest":              "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"image_reference":     "ghcr.io/criteria-adapters/noop:1.2.3-image",
		"shim_listen_address": "127.0.0.1:0",
		"token_ref":           "/run/data/tokens/noop-root.token",
		"accept_token":        secretToken,
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
	// CRI-236: the raw token value is now deliberately carried on the wire in
	// the provision event's accept_token field (asserted via wantData above);
	// the redaction registry (workflow secrets) is a separate channel and must
	// never receive adapter accept tokens.

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
	// CRI-233: an event carrying no environment identity (the zero value — the
	// shape an old-criteria producer emits) must still produce a valid payload;
	// the identity keys surface as empty strings so consumers treat them as
	// "no environment grouping" without special-casing missing keys.
	if got := secondPayload.Data["environment_type"]; got != "" {
		t.Errorf("second environment_type: got %v, want empty string (no identity on this event)", got)
	}
	if got := secondPayload.Data["environment_name"]; got != "" {
		t.Errorf("second environment_name: got %v, want empty string (no identity on this event)", got)
	}
	// CRI-236: released events never carry a token.
	if got := secondPayload.Data["accept_token"]; got != "" {
		t.Errorf("second accept_token: got %v, want empty string (released events carry no token)", got)
	}
}

// TestLocalSink_OnCheckpointPointer_NDJSONPinsCRI203 pins the checkpoint
// pointer through the local run's ND-JSON wire exactly as runstate's castle
// seam reads it: PascalCase payload_type "CheckpointPointer" with a protojson
// camelCase payload. The parser round-trip at the end proves the events
// package (the parapet/ListRunEvents consumer) accepts the emitted line.
func TestLocalSink_OnCheckpointPointer_NDJSONPinsCRI203(t *testing.T) {
	var buf bytes.Buffer
	sink := &LocalSink{RunID: "run-local-ck", Out: &buf}

	sink.OnRunStarted("ck-wf", "exec")
	sink.OnCheckpointPointer(&engine.CheckpointPointerEvent{
		RunID:       "run-local-ck",
		SessionID:   "copilot.exec",
		AdapterKind: "copilot",
		AdapterName: "exec",
		StateID:     "copilot.exec/0000000001",
		StateSchema: "session/v1",
		StateDigest: "sha256:abcd1234",
		StateSize:   4096,
		Granularity: "per-step",
	})
	sink.OnRunCompleted("done", true)

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("line count: got %d want 3", len(lines))
	}
	var line sinkLine
	if err := json.Unmarshal([]byte(lines[1]), &line); err != nil {
		t.Fatalf("pointer line json: %v", err)
	}
	if line.PayloadType != "CheckpointPointer" {
		t.Fatalf("payload_type = %q; want CheckpointPointer", line.PayloadType)
	}
	if line.Seq != 2 || line.RunID != "run-local-ck" {
		t.Errorf("seq/run_id = %d/%q", line.Seq, line.RunID)
	}

	// The castle consumer re-parses the payload with the events package: the
	// emitted payload must decode into the oneof arm with every field intact,
	// and the discriminator must classify as checkpoint.pointer (non-terminal).
	env := events.NewEnvelope("run-local-ck", &pb.CheckpointPointer{
		StateId:     "copilot.exec/0000000001",
		AdapterKind: "copilot",
		StateSchema: "session/v1",
		StateDigest: "sha256:abcd1234",
		StateSize:   4096,
		Granularity: "per-step",
		SessionId:   "copilot.exec",
	})
	if events.TypeString(env) != "checkpoint.pointer" {
		t.Fatalf("discriminator mismatch: %q", events.TypeString(env))
	}
	got := env.GetCheckpointPointer()
	if got.GetStateId() != "copilot.exec/0000000001" ||
		got.GetAdapterKind() != "copilot" ||
		got.GetStateSchema() != "session/v1" ||
		got.GetStateDigest() != "sha256:abcd1234" ||
		got.GetGranularity() != "per-step" ||
		got.GetSessionId() != "copilot.exec" ||
		got.GetStateSize() != 4096 {
		t.Errorf("pointer fields drifted on the local wire: %+v", got)
	}
	// The payload the LocalSink rendered must survive protojson re-parse —
	// same marshal format on both sides (camelCase keys).
	var reparsed pb.CheckpointPointer
	if err := protojson.Unmarshal(line.Payload, &reparsed); err != nil {
		t.Fatalf("emitted payload not parseable: %v", err)
	}
	if reparsed.GetStateSize() != 4096 || reparsed.GetStateId() != got.GetStateId() {
		t.Errorf("emitted payload drift: %+v", &reparsed)
	}
}
