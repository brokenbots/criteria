package run

// prompt_sink_test.go — the agent.prompt_injected event surface at the run
// sink layer (CRI-259 R8, ADR-0006 D5). Pins the D5 field mapping on both
// the local ND-JSON stream and the server publish path, the MultiSink
// fan-out, the console no-op, and the observation path: the injected event
// is recoverable in run-event order between the addressed step's
// StepEntered and StepOutcome.

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

func TestLocalSink_OnAgentPromptInjected_NDJSONPayloadAndOrder(t *testing.T) {
	var buf bytes.Buffer
	sink := &LocalSink{RunID: "run-local-prompt", Out: &buf}

	sink.OnRunStarted("wf", "a")
	sink.OnStepEntered("a", "promptable", 1)
	sink.OnAgentPromptInjected("a", "promptable.default", "mid-turn nudge", "agent-owner", time.Now().UTC())
	sink.OnStepOutcome("a", "success", 5*time.Millisecond, nil)

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("line count: got %d want 4", len(lines))
	}
	wantTypes := []string{"RunStarted", "StepEntered", "AgentPromptInjected", "StepOutcome"}
	var injectedLine sinkLine
	for i, line := range lines {
		var got sinkLine
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Fatalf("line %d json: %v", i+1, err)
		}
		if got.PayloadType != wantTypes[i] {
			t.Fatalf("line %d payload_type: got %q want %q", i+1, got.PayloadType, wantTypes[i])
		}
		if got.PayloadType == "AgentPromptInjected" {
			injectedLine = got
		}
	}
	// Criterion 8 (run-event order): the injected envelope is recoverable
	// between the addressed step's StepEntered and StepOutcome.
	if injectedLine.PayloadType != "AgentPromptInjected" {
		t.Fatal("injected envelope not found in stream")
	}

	var payload pb.AgentPromptInjected
	if err := protojson.Unmarshal(injectedLine.Payload, &payload); err != nil {
		t.Fatalf("payload json: %v", err)
	}
	if payload.GetStep() != "a" {
		t.Errorf("payload step = %q, want %q", payload.GetStep(), "a")
	}
	if payload.GetSessionId() != "promptable.default" {
		t.Errorf("payload session_id = %q", payload.GetSessionId())
	}
	if payload.GetPrompt() != "mid-turn nudge" {
		t.Errorf("payload prompt = %q", payload.GetPrompt())
	}
	if payload.GetCaller() != "agent-owner" {
		t.Errorf("payload caller = %q", payload.GetCaller())
	}
	if payload.GetDeliveredAt() == nil || payload.GetDeliveredAt().AsTime().IsZero() {
		t.Error("payload delivered_at missing")
	}
}

// TestSink_PublishCarriesAgentPromptInjected covers the publish path: the
// envelope's payload is the AgentPromptInjected oneof arm and its stable
// discriminator resolves to agent.prompt_injected.
func TestSink_PublishCarriesAgentPromptInjected(t *testing.T) {
	fp := &fakePublisher{}
	sink := &Sink{RunID: "run-pub", Client: fp}

	deliveredAt := time.Now().UTC()
	sink.OnAgentPromptInjected("a", "promptable.default", "nudge", "agent-owner", deliveredAt)

	if len(fp.published) != 1 {
		t.Fatalf("published %d envelopes, want 1", len(fp.published))
	}
	env := fp.published[0]
	if got := events.TypeString(env); got != "agent.prompt_injected" {
		t.Fatalf("discriminator = %q, want agent.prompt_injected", got)
	}
	payload := env.GetAgentPromptInjected()
	if payload == nil {
		t.Fatalf("payload is %T, want *pb.AgentPromptInjected", env.GetPayload())
	}
	if payload.GetStep() != "a" || payload.GetSessionId() != "promptable.default" {
		t.Errorf("step/session_id = %q/%q", payload.GetStep(), payload.GetSessionId())
	}
	if payload.GetPrompt() != "nudge" || payload.GetCaller() != "agent-owner" {
		t.Errorf("prompt/caller = %q/%q", payload.GetPrompt(), payload.GetCaller())
	}
	if ts := payload.GetDeliveredAt(); ts == nil || !ts.AsTime().Equal(deliveredAt) {
		t.Errorf("delivered_at = %v, want %v", ts, deliveredAt)
	}
}

// TestMultiSink_OnAgentPromptInjectedFansOutToAllChildren pins the fan-out:
// every child sink receives the injected event exactly once.
func TestMultiSink_OnAgentPromptInjectedFansOutToAllChildren(t *testing.T) {
	var a, b recordingSink
	var sink engine.Sink = NewMultiSink(&a, &b)

	sink.OnAgentPromptInjected("s", "sess", "nudge", "caller", time.Now().UTC())

	if got := a.calls.Load(); got != 1 {
		t.Errorf("child a prompt events: got %d want 1", got)
	}
	if got := b.calls.Load(); got != 1 {
		t.Errorf("child b prompt events: got %d want 1", got)
	}
}

// TestConsoleSink_OnAgentPromptInjectedIsNoOp covers the console no-op: the
// call must not panic and must not render anything into the progress view
// (the durable record lives in the run sink, not the console view).
func TestConsoleSink_OnAgentPromptInjectedIsNoOp(t *testing.T) {
	var buf bytes.Buffer
	sink := NewConsoleSink(&buf, []string{"open"}, false, nil)

	sink.OnStepEntered("open", "demo", 1)
	sink.OnAgentPromptInjected("open", "promptable.default", "nudge", "agent-owner", time.Now().UTC())
	sink.OnStepOutcome("open", "success", time.Millisecond, nil)

	out := stripANSI(buf.String())
	if strings.Contains(strings.ToLower(out), "prompt") {
		t.Fatalf("console rendered prompt content; output:\n%s", out)
	}
	if !strings.Contains(out, "✓ success") {
		t.Fatalf("console step rendering changed; output:\n%s", out)
	}
}
