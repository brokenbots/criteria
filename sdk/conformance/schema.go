package conformance

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	criteria "github.com/brokenbots/criteria/sdk"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

// testSchemaVersion verifies schema-version enforcement.
//
// Assertions:
//  1. criteria.SchemaVersion equals 1 (the v0.1 SDK constant).
//  2. Envelopes submitted with schema_version=1 (the current version) are accepted.
//  3. Envelopes submitted with schema_version=2 are rejected with
//     connect.CodeFailedPrecondition. This gate prevents schema drift from
//     silently producing corrupt rows in the event store.
//  4. The schema_version field on retrieved envelopes equals criteria.SchemaVersion,
//     confirming the orchestrator persists and returns the submitted version.
func testSchemaVersion(t *testing.T, s Subject) {
	t.Run("ConstantIsOne", func(t *testing.T) {
		if criteria.SchemaVersion != 1 {
			t.Errorf("criteria.SchemaVersion = %d, want 1", criteria.SchemaVersion)
		}
	})

	t.Run("CurrentVersionAccepted", func(t *testing.T) {
		testSchemaVersionAccepted(t, s)
	})

	t.Run("FutureVersionRejected", func(t *testing.T) {
		testSchemaFutureVersionRejected(t, s)
	})

	t.Run("PersistedVersionMatchesSDK", func(t *testing.T) {
		testSchemaPersistedVersionMatchesSDK(t, s)
	})
}

func testSchemaVersionAccepted(t *testing.T, s Subject) {
	baseURL, client, teardown := s.SetUp(t)
	defer teardown()

	const token = "token-schema-v1"
	criteriaID := s.RegisterAgent(t, "criteria-schema-v1", token)
	oClient := criteria.NewServiceClient(client, baseURL)

	createReq := connect.NewRequest(&pb.CreateRunRequest{CriteriaId: criteriaID, WorkflowName: "conformance-schema-v1"})
	createReq.Header().Set("Authorization", "Bearer "+token)
	runResp, err := oClient.CreateRun(context.Background(), createReq)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	runID := runResp.Msg.RunId

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream := oClient.SubmitEvents(ctx)
	stream.RequestHeader().Set("Authorization", "Bearer "+token)

	env := criteria.NewEnvelope(runID, &pb.StepLog{Step: "s", Stream: pb.LogStream_LOG_STREAM_STDOUT, Chunk: "schema-ok"})
	env.CorrelationId = "schema-v1-ok"
	// criteria.NewEnvelope stamps SchemaVersion=1; confirm it's accepted.
	if err := stream.Send(env); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if _, err := stream.Receive(); err != nil {
		t.Fatalf("Receive ack: %v (schema_version=%d should be accepted)", err, env.SchemaVersion)
	}
	stream.CloseRequest()
	for {
		if _, recvErr := stream.Receive(); recvErr != nil {
			break
		}
	}
}

func testSchemaFutureVersionRejected(t *testing.T, s Subject) {
	baseURL, client, teardown := s.SetUp(t)
	defer teardown()

	const token = "token-schema-v2"
	criteriaID := s.RegisterAgent(t, "criteria-schema-v2", token)
	oClient := criteria.NewServiceClient(client, baseURL)

	createReq := connect.NewRequest(&pb.CreateRunRequest{CriteriaId: criteriaID, WorkflowName: "conformance-schema-v2"})
	createReq.Header().Set("Authorization", "Bearer "+token)
	runResp, err := oClient.CreateRun(context.Background(), createReq)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	runID := runResp.Msg.RunId

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream := oClient.SubmitEvents(ctx)
	stream.RequestHeader().Set("Authorization", "Bearer "+token)

	env := criteria.NewEnvelope(runID, &pb.StepLog{Step: "s", Stream: pb.LogStream_LOG_STREAM_STDOUT, Chunk: "future"})
	env.SchemaVersion = 2 // manually override to simulate a future SDK
	env.CorrelationId = "schema-v2-reject"

	if err := stream.Send(env); err != nil {
		// Server may close the stream before the client reads — that's still a
		// rejection. Validate the error code rather than silently passing.
		if connect.CodeOf(err) != connect.CodeFailedPrecondition {
			t.Errorf("schema_version=2 Send: want CodeFailedPrecondition on early rejection, got code=%v err=%v", connect.CodeOf(err), err)
		}
		return
	}
	_, recvErr := stream.Receive()
	code := connect.CodeOf(recvErr)
	if code != connect.CodeFailedPrecondition {
		t.Errorf("schema_version=2: want CodeFailedPrecondition, got code=%v err=%v", code, recvErr)
	}
}

func testSchemaPersistedVersionMatchesSDK(t *testing.T, s Subject) {
	baseURL, client, teardown := s.SetUp(t)
	defer teardown()

	const token = "token-schema-persist"
	criteriaID := s.RegisterAgent(t, "criteria-schema-persist", token)
	oClient := criteria.NewServiceClient(client, baseURL)

	createReq := connect.NewRequest(&pb.CreateRunRequest{CriteriaId: criteriaID, WorkflowName: "conformance-schema-persist"})
	createReq.Header().Set("Authorization", "Bearer "+token)
	runResp, err := oClient.CreateRun(context.Background(), createReq)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	runID := runResp.Msg.RunId

	const corrID = "schema-persist-check"

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream := oClient.SubmitEvents(ctx)
	stream.RequestHeader().Set("Authorization", "Bearer "+token)
	env := criteria.NewEnvelope(runID, &pb.StepLog{Step: "s", Stream: pb.LogStream_LOG_STREAM_STDOUT, Chunk: "version-check"})
	env.CorrelationId = corrID
	if err := stream.Send(env); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if _, err := stream.Receive(); err != nil {
		t.Fatalf("Receive ack: %v", err)
	}
	stream.CloseRequest()
	for {
		if _, recvErr := stream.Receive(); recvErr != nil {
			break
		}
	}

	events := s.ListRunEvents(t, baseURL, client, token, runID, 0)
	var found *pb.Envelope
	for _, ev := range events {
		if ev.CorrelationId == corrID {
			found = ev
			break
		}
	}
	if found == nil {
		t.Fatalf("event not found in ListRunEvents (corr=%s)", corrID)
	}
	if int(found.SchemaVersion) != criteria.SchemaVersion {
		t.Errorf("persisted schema_version=%d want %d", found.SchemaVersion, criteria.SchemaVersion)
	}
}
