package events

import (
	"encoding/json"
	"testing"
)

func TestNewEnvelopeRoundTrip(t *testing.T) {
	env, err := New("run-1", TypeStepOutcome, StepOutcome{
		Step:       "build",
		Outcome:    "success",
		DurationMS: 123,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if env.SchemaVersion != SchemaVersion {
		t.Fatalf("schema version: got %d", env.SchemaVersion)
	}
	if env.Type != TypeStepOutcome {
		t.Fatalf("type: got %s", env.Type)
	}

	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back Envelope
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.RunID != "run-1" {
		t.Fatalf("run id: %s", back.RunID)
	}
	var p StepOutcome
	if err := json.Unmarshal(back.Payload, &p); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if p.Outcome != "success" {
		t.Fatalf("outcome: %s", p.Outcome)
	}
}
