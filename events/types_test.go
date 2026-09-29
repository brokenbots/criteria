package events_test

import (
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/brokenbots/criteria/events"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
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

// TestNewEnvelope_WorkflowGraphs pins the CRI-278 payload type through the
// envelope machinery: construction, discriminator, non-terminal classification,
// and protojson round trip.
func TestNewEnvelope_WorkflowGraphs(t *testing.T) {
	env := events.NewEnvelope("run-1", &pb.WorkflowGraphs{
		Subworkflows: []*pb.SubworkflowGraph{{
			Name:       "inner_task",
			SourcePath: "./subworkflows/inner",
			Body:       `{"name":"inner_task"}`,
		}},
	})
	if events.TypeString(env) != "workflow.graphs" {
		t.Fatalf("type string: %q", events.TypeString(env))
	}
	if events.IsTerminal(env) {
		t.Fatal("workflow.graphs should not be terminal")
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
	graphs := back.GetWorkflowGraphs()
	if graphs == nil || len(graphs.Subworkflows) != 1 ||
		graphs.Subworkflows[0].Name != "inner_task" ||
		graphs.Subworkflows[0].SourcePath != "./subworkflows/inner" {
		t.Fatalf("payload: %+v", graphs)
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

// TestNewEnvelope_CheckpointPointer pins the CRI-203 advisory checkpoint
// pointer through the envelope machinery: construction, discriminator,
// non-terminal classification, and protojson round trip with every pointer
// field intact. The pointer carries metadata only — never checkpoint bytes.
func TestNewEnvelope_CheckpointPointer(t *testing.T) {
	env := events.NewEnvelope("run-1", &pb.CheckpointPointer{
		StateId:      "copilot.exec/0000000001",
		AdapterKind:  "copilot",
		StateSchema:  "session/v1",
		StateDigest:  "sha256:abcd1234",
		StateSize:    4096,
		Granularity:  "step",
		SessionId:    "copilot.exec",
	})
	if got := env.GetCheckpointPointer(); got == nil {
		t.Fatalf("payload not set as checkpoint_pointer arm: %+v", env)
	}
	if events.TypeString(env) != "checkpoint.pointer" {
		t.Fatalf("type string: %q", events.TypeString(env))
	}
	if events.IsTerminal(env) {
		t.Fatal("checkpoint.pointer should not be terminal")
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

	// Parapet/ListRunEvents consumers see the pointer through this payload;
	// assert every mapped field (HARD requirement) survives the parser.
	ptr := back.GetCheckpointPointer()
	if ptr == nil {
		t.Fatalf("checkpoint_pointer arm lost on round trip: payload %q", raw)
	}
	for name, want := range map[string]string{
		"StateId":     "copilot.exec/0000000001",
		"AdapterKind": "copilot",
		"StateSchema": "session/v1",
		"StateDigest": "sha256:abcd1234",
		"SessionId":   "copilot.exec",
	} {
		got := map[string]string{
			"StateId":     ptr.GetStateId(),
			"AdapterKind": ptr.GetAdapterKind(),
			"StateSchema": ptr.GetStateSchema(),
			"StateDigest": ptr.GetStateDigest(),
			"SessionId":   ptr.GetSessionId(),
		}[name]
		if got != want {
			t.Errorf("pointer %s = %q, want %q", name, got, want)
		}
	}
	if size := ptr.GetStateSize(); size != 4096 {
		t.Errorf("stateSize = %d, want 4096", size)
	}
	if gran := ptr.GetGranularity(); gran != "step" {
		t.Errorf("granularity = %q, want %q", gran, "step")
	}
}
