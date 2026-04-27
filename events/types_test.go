package events_test

import (
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/brokenbots/overseer/events"
	pb "github.com/brokenbots/overseer/sdk/pb/overseer/v1"
)

func TestNewEnvelopeRoundTrip(t *testing.T) {
	env := events.NewEnvelope("run-1", &pb.StepOutcome{
		Step:       "build",
		Outcome:    "success",
		DurationMs: 123,
	})
	if env.SchemaVersion != events.SchemaVersion {
		t.Fatalf("schema version: got %d", env.SchemaVersion)
	}
	if env.RunId != "run-1" {
		t.Fatalf("run id: %q", env.RunId)
	}
	if events.TypeString(env) != "step.outcome" {
		t.Fatalf("type string: %q", events.TypeString(env))
	}
	if events.IsTerminal(env) {
		t.Fatalf("step.outcome should not be terminal")
	}

	raw, err := protojson.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back pb.Envelope
	if err := protojson.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !proto.Equal(env, &back) {
		t.Fatalf("round trip mismatch:\nwant: %+v\nback: %+v", env, &back)
	}
	if got := back.GetStepOutcome(); got == nil || got.Outcome != "success" {
		t.Fatalf("payload: %+v", got)
	}
}

func TestIsTerminal(t *testing.T) {
	if !events.IsTerminal(events.NewEnvelope("r", &pb.RunCompleted{})) {
		t.Fatal("run.completed should be terminal")
	}
	if !events.IsTerminal(events.NewEnvelope("r", &pb.RunFailed{})) {
		t.Fatal("run.failed should be terminal")
	}
	if events.IsTerminal(events.NewEnvelope("r", &pb.StepEntered{})) {
		t.Fatal("step.entered should not be terminal")
	}
	if events.IsTerminal(nil) {
		t.Fatal("nil envelope should not be terminal")
	}
}
