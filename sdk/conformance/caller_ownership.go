package conformance

import (
	"context"
	"net/http"
	"testing"
	"time"

	"connectrpc.com/connect"

	pb "github.com/brokenbots/overseer/sdk/pb/overseer/v1"
	overseer "github.com/brokenbots/overseer/sdk"
)

// testCallerOwnership asserts the caller-ownership contract for every mutating
// OverseerService RPC. Two overseers (A and B) are registered with distinct
// tokens. B-token calls against A-owned resources must return
// connect.CodePermissionDenied.
//
// RPCs tested:
//   - Heartbeat: B cannot heartbeat as A.
//   - CreateRun: B cannot create a run claiming A's overseer_id.
//   - ReattachRun: B cannot reattach to A's run.
//   - SubmitEvents: B cannot submit events for A's run.
//   - Control: B cannot subscribe to A's control channel.
//   - Resume: B cannot resume A's run.
//   - Register: without a bootstrap credential returns Unimplemented (or
//     the documented bootstrap-required error).
func testCallerOwnership(t *testing.T, s Subject) {
	t.Run("Heartbeat", func(t *testing.T) {
		testOwnership_Heartbeat(t, s)
	})
	t.Run("CreateRun", func(t *testing.T) {
		testOwnership_CreateRun(t, s)
	})
	t.Run("ReattachRun", func(t *testing.T) {
		testOwnership_ReattachRun(t, s)
	})
	t.Run("SubmitEvents", func(t *testing.T) {
		testOwnership_SubmitEvents(t, s)
	})
	t.Run("Control", func(t *testing.T) {
		testOwnership_Control(t, s)
	})
	t.Run("Resume", func(t *testing.T) {
		testOwnership_Resume(t, s)
	})
	t.Run("RegisterBootstrapGate", func(t *testing.T) {
		testOwnership_RegisterBootstrapGate(t, s)
	})
}

// ownershipSetup registers two overseers (owner A and attacker B) and creates
// a run owned by A. It returns the clients and IDs needed for ownership tests.
func ownershipSetup(t *testing.T, s Subject) (
	baseURL string,
	client *http.Client,
	oClient overseer.ServiceClient,
	ownerID, ownerToken, attackerID, attackerToken, runID string,
) {
	t.Helper()
	var teardown func()
	baseURL, client, teardown = s.SetUp(t)
	t.Cleanup(teardown)

	ownerToken = "tok-owner"
	attackerToken = "tok-attacker"
	ownerID = s.RegisterOverseer(t, "owner", ownerToken)
	attackerID = s.RegisterOverseer(t, "attacker", attackerToken)

	oClient = overseer.NewServiceClient(client, baseURL)

	createReq := connect.NewRequest(&pb.CreateRunRequest{OverseerId: ownerID, WorkflowName: "ownership-wf"})
	createReq.Header().Set("Authorization", "Bearer "+ownerToken)
	runResp, err := oClient.CreateRun(context.Background(), createReq)
	if err != nil {
		t.Fatalf("ownershipSetup: CreateRun: %v", err)
	}
	runID = runResp.Msg.RunId
	return
}

func testOwnership_Heartbeat(t *testing.T, s Subject) {
	_, _, oClient, ownerID, _, _, attackerToken, _ := ownershipSetup(t, s)
	req := connect.NewRequest(&pb.HeartbeatRequest{OverseerId: ownerID})
	req.Header().Set("Authorization", "Bearer "+attackerToken)
	_, err := oClient.Heartbeat(context.Background(), req)
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("Heartbeat cross-owner: want CodePermissionDenied, got code=%v err=%v",
			connect.CodeOf(err), err)
	}
}

func testOwnership_CreateRun(t *testing.T, s Subject) {
	_, _, oClient, ownerID, _, _, attackerToken, _ := ownershipSetup(t, s)
	req := connect.NewRequest(&pb.CreateRunRequest{OverseerId: ownerID, WorkflowName: "wf"})
	req.Header().Set("Authorization", "Bearer "+attackerToken)
	_, err := oClient.CreateRun(context.Background(), req)
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("CreateRun cross-owner: want CodePermissionDenied, got code=%v err=%v",
			connect.CodeOf(err), err)
	}
}

func testOwnership_ReattachRun(t *testing.T, s Subject) {
	_, _, oClient, _, _, attackerID, attackerToken, runID := ownershipSetup(t, s)
	req := connect.NewRequest(&pb.ReattachRunRequest{RunId: runID, OverseerId: attackerID})
	req.Header().Set("Authorization", "Bearer "+attackerToken)
	_, err := oClient.ReattachRun(context.Background(), req)
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("ReattachRun cross-owner: want CodePermissionDenied, got code=%v err=%v",
			connect.CodeOf(err), err)
	}
}

func testOwnership_SubmitEvents(t *testing.T, s Subject) {
	_, _, oClient, _, _, _, attackerToken, runID := ownershipSetup(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream := oClient.SubmitEvents(ctx)
	stream.RequestHeader().Set("Authorization", "Bearer "+attackerToken)
	env := overseer.NewEnvelope(runID, &pb.StepLog{Step: "s", Stream: pb.LogStream_LOG_STREAM_STDOUT, Chunk: "x"})
	env.CorrelationId = "own-submit"
	if err := stream.Send(env); err != nil {
		// Server may close the stream before the client reads — validate the
		// error code rather than silently passing on any failure.
		if connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Errorf("SubmitEvents cross-owner Send: want CodePermissionDenied on early rejection, got code=%v err=%v", connect.CodeOf(err), err)
		}
		return
	}
	_, err := stream.Receive()
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("SubmitEvents cross-owner: want CodePermissionDenied, got code=%v err=%v",
			connect.CodeOf(err), err)
	}
}

func testOwnership_Control(t *testing.T, s Subject) {
	_, _, oClient, ownerID, _, _, attackerToken, _ := ownershipSetup(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req := connect.NewRequest(&pb.ControlSubscribeRequest{OverseerId: ownerID})
	req.Header().Set("Authorization", "Bearer "+attackerToken)
	stream, err := oClient.Control(ctx, req)
	if err != nil {
		if connect.CodeOf(err) == connect.CodePermissionDenied {
			return
		}
		t.Fatalf("Control cross-owner: unexpected error: %v", err)
	}
	// Server may send the rejection as the first stream message.
	stream.Receive() // return value is bool; error is in stream.Err()
	if connect.CodeOf(stream.Err()) != connect.CodePermissionDenied {
		t.Errorf("Control cross-owner: want CodePermissionDenied, got code=%v err=%v",
			connect.CodeOf(stream.Err()), stream.Err())
	}
}

func testOwnership_Resume(t *testing.T, s Subject) {
	baseURL, client, teardown := s.SetUp(t)
	defer teardown()

	ownerToken := "tok-res-owner"
	attackerToken := "tok-res-attacker"
	ownerID := s.RegisterOverseer(t, "res-owner", ownerToken)
	_ = s.RegisterOverseer(t, "res-attacker", attackerToken)
	oClient := overseer.NewServiceClient(client, baseURL)

	createReq := connect.NewRequest(&pb.CreateRunRequest{OverseerId: ownerID, WorkflowName: "own-resume-wf"})
	createReq.Header().Set("Authorization", "Bearer "+ownerToken)
	runResp, err := oClient.CreateRun(context.Background(), createReq)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	runID := runResp.Msg.RunId
	pauseRunViaWaitEntered(t, oClient, ownerToken, runID, "gate-own")

	req := connect.NewRequest(&pb.ResumeRequest{RunId: runID, Signal: "gate-own"})
	req.Header().Set("Authorization", "Bearer "+attackerToken)
	_, err = oClient.Resume(context.Background(), req)
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("Resume cross-owner: want CodePermissionDenied, got code=%v err=%v",
			connect.CodeOf(err), err)
	}
}

func testOwnership_RegisterBootstrapGate(t *testing.T, s Subject) {
	// The conformance test cannot configure the bootstrap credential on the
	// server (that's implementation-specific). We verify that Register without
	// any credential either returns Unimplemented (no bootstrap configured) or
	// Unauthenticated (bootstrap required but not provided). Both are acceptable
	// per the SDK doc contract.
	//
	// Note: the Subject uses RegisterOverseer (direct store path) for test setup,
	// so the bootstrap mechanism is not exercised there. This test exercises the
	// wire-level Register RPC directly.
	baseURL, client, teardown := s.SetUp(t)
	defer teardown()

	oClient := overseer.NewServiceClient(client, baseURL)
	_, err := oClient.Register(context.Background(), connect.NewRequest(&pb.RegisterRequest{Name: "x"}))
	code := connect.CodeOf(err)
	if code != connect.CodeUnimplemented && code != connect.CodeUnauthenticated {
		t.Errorf("Register without bootstrap token: want CodeUnimplemented or CodeUnauthenticated, got code=%v err=%v",
			code, err)
	}
}
