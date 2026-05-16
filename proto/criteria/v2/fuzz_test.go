package criteriav2_test

import (
	"testing"

	"google.golang.org/protobuf/proto"

	criteriav2 "github.com/brokenbots/criteria/proto/criteria/v2"
)

// FuzzUnmarshalAdapterMessages feeds random bytes to proto.Unmarshal for each
// top-level wire message type, guarding against panics from malformed inputs
// received from networked adapters (WS20).
func FuzzUnmarshalAdapterMessages(f *testing.F) {
	// Seed corpus from well-formed messages so the fuzzer starts from valid
	// states before exploring mutations.
	seeds := []proto.Message{
		&criteriav2.InfoRequest{},
		&criteriav2.InfoResponse{Name: "seed"},
		&criteriav2.OpenSessionRequest{SessionId: "s", Secrets: map[string]string{"k": "v"}},
		&criteriav2.OpenSessionResponse{},
		&criteriav2.ExecuteRequest{SessionId: "s", StepName: "step"},
		&criteriav2.ExecuteEvent{},
		&criteriav2.LogRequest{SessionId: "s"},
		&criteriav2.LogEvent{},
		&criteriav2.PermissionEvent{},
		&criteriav2.PermissionDecision{},
		&criteriav2.PauseRequest{SessionId: "s"},
		&criteriav2.PauseResponse{},
		&criteriav2.ResumeRequest{SessionId: "s"},
		&criteriav2.ResumeResponse{},
		&criteriav2.SnapshotRequest{SessionId: "s"},
		&criteriav2.SnapshotResponse{State: []byte("state"), SchemaVersion: 1},
		&criteriav2.RestoreRequest{SessionId: "s", State: []byte("state")},
		&criteriav2.RestoreResponse{},
		&criteriav2.InspectRequest{SessionId: "s"},
		&criteriav2.InspectResponse{CurrentStep: "step-a"},
		&criteriav2.CloseSessionRequest{SessionId: "s"},
		&criteriav2.CloseSessionResponse{},
		&criteriav2.SnapshotVersionMismatch{Have: 1, Want: 2},
		&criteriav2.Heartbeat{StreamName: "execute"},
		&criteriav2.Chunk{Seq: 0, Total: 1, Final: true},
	}
	for _, seed := range seeds {
		b, err := proto.Marshal(seed)
		if err != nil {
			f.Fatalf("failed to marshal seed %T: %v", seed, err)
		}
		f.Add(b)
	}

	// Unmarshal targets — one per top-level message type.
	targets := []func([]byte) error{
		func(b []byte) error { return proto.Unmarshal(b, &criteriav2.InfoRequest{}) },
		func(b []byte) error { return proto.Unmarshal(b, &criteriav2.InfoResponse{}) },
		func(b []byte) error { return proto.Unmarshal(b, &criteriav2.OpenSessionRequest{}) },
		func(b []byte) error { return proto.Unmarshal(b, &criteriav2.OpenSessionResponse{}) },
		func(b []byte) error { return proto.Unmarshal(b, &criteriav2.ExecuteRequest{}) },
		func(b []byte) error { return proto.Unmarshal(b, &criteriav2.ExecuteEvent{}) },
		func(b []byte) error { return proto.Unmarshal(b, &criteriav2.LogRequest{}) },
		func(b []byte) error { return proto.Unmarshal(b, &criteriav2.LogEvent{}) },
		func(b []byte) error { return proto.Unmarshal(b, &criteriav2.PermissionEvent{}) },
		func(b []byte) error { return proto.Unmarshal(b, &criteriav2.PermissionDecision{}) },
		func(b []byte) error { return proto.Unmarshal(b, &criteriav2.PauseRequest{}) },
		func(b []byte) error { return proto.Unmarshal(b, &criteriav2.PauseResponse{}) },
		func(b []byte) error { return proto.Unmarshal(b, &criteriav2.ResumeRequest{}) },
		func(b []byte) error { return proto.Unmarshal(b, &criteriav2.ResumeResponse{}) },
		func(b []byte) error { return proto.Unmarshal(b, &criteriav2.SnapshotRequest{}) },
		func(b []byte) error { return proto.Unmarshal(b, &criteriav2.SnapshotResponse{}) },
		func(b []byte) error { return proto.Unmarshal(b, &criteriav2.RestoreRequest{}) },
		func(b []byte) error { return proto.Unmarshal(b, &criteriav2.RestoreResponse{}) },
		func(b []byte) error { return proto.Unmarshal(b, &criteriav2.InspectRequest{}) },
		func(b []byte) error { return proto.Unmarshal(b, &criteriav2.InspectResponse{}) },
		func(b []byte) error { return proto.Unmarshal(b, &criteriav2.CloseSessionRequest{}) },
		func(b []byte) error { return proto.Unmarshal(b, &criteriav2.CloseSessionResponse{}) },
		func(b []byte) error { return proto.Unmarshal(b, &criteriav2.SnapshotVersionMismatch{}) },
		func(b []byte) error { return proto.Unmarshal(b, &criteriav2.Heartbeat{}) },
		func(b []byte) error { return proto.Unmarshal(b, &criteriav2.Chunk{}) },
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		for _, target := range targets {
			// We only care that no panic occurs; unmarshal errors are expected.
			_ = target(data)
		}
	})
}
