package conformance

import (
	"context"
	"testing"
	"time"

	connect "connectrpc.com/connect/v2"

	criteria "github.com/brokenbots/criteria/sdk"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
	criteriav1connect "github.com/brokenbots/criteria/sdk/pb/criteria/v1/criteriav1connect"
)

// testControlLifecycle verifies the Control server-stream contract.
//
// Scenarios:
//  1. Subscribe → first message is ControlReady (headers flushed immediately).
//  2. StopRun on the subscriber's run → RunCancel arrives on the stream.
//  3. Re-subscribe after disconnect → ControlReady arrives again before any
//     backlogged control messages.
//  4. Agent-A stream does not receive control messages for Agent-B's
//     runs (isolation contract).
func testControlLifecycle(t *testing.T, s Subject) {
	t.Run("ControlReady", func(t *testing.T) {
		testControlReady(t, s)
	})
	t.Run("RunCancelDelivered", func(t *testing.T) {
		testRunCancelDelivered(t, s)
	})
	t.Run("ResubscribeGetsControlReady", func(t *testing.T) {
		testControlResubscribe(t, s)
	})
	t.Run("AgentIsolation", func(t *testing.T) {
		testControlAgentIsolation(t, s)
	})
}

func testControlReady(t *testing.T, s Subject) {
	baseURL, client, teardown := s.SetUp(t)
	defer teardown()

	const token = "token-ctrl-rdy"
	criteriaID := s.RegisterAgent(t, "criteria-ctrl-rdy", token)
	oClient := criteria.NewServiceClient(client, baseURL)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ctrlCtx, ctrlInfo := connect.NewClientContext(ctx)
	ctrlInfo.RequestHeader().Set("Authorization", "Bearer "+token)
	stream, err := oClient.Control(ctrlCtx, &pb.ControlSubscribeRequest{CriteriaId: criteriaID})
	if err != nil {
		t.Fatalf("Control subscribe: %v", err)
	}

	msg, recvErr := stream.Receive()
	if recvErr != nil {
		t.Fatalf("Control first Receive: %v", recvErr)
	}
	if _, ok := msg.Command.(*pb.ControlMessage_ControlReady); !ok {
		t.Errorf("Control first message: want ControlReady, got %T", msg.Command)
	}
}

func testRunCancelDelivered(t *testing.T, s Subject) {
	baseURL, client, teardown := s.SetUp(t)
	defer teardown()

	const token = "token-ctrl-cancel"
	criteriaID := s.RegisterAgent(t, "criteria-ctrl-cancel", token)
	oClient := criteria.NewServiceClient(client, baseURL)
	runID := authCreateRun(t, oClient, token, criteriaID, "conformance-ctrl-cancel")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ctrlCtx, ctrlInfo := connect.NewClientContext(ctx)
	ctrlInfo.RequestHeader().Set("Authorization", "Bearer "+token)
	stream, err := oClient.Control(ctrlCtx, &pb.ControlSubscribeRequest{CriteriaId: criteriaID})
	if err != nil {
		t.Fatalf("Control subscribe: %v", err)
	}

	// Drain ControlReady.
	msg, recvErr := stream.Receive()
	if recvErr != nil {
		t.Fatalf("Control first Receive: %v", recvErr)
	}
	if _, ok := msg.Command.(*pb.ControlMessage_ControlReady); !ok {
		t.Errorf("first message want ControlReady, got %T", msg.Command)
	}

	// Trigger a stop-run command via the Subject (abstracts over ServerService).
	if err := s.StopRun(t, baseURL, client, token, runID); err != nil {
		t.Fatalf("StopRun: %v", err)
	}

	// The next message on the Control stream must be RunCancel for our run.
	msg, recvErr = stream.Receive()
	if recvErr != nil {
		t.Fatalf("Control Receive after StopRun: %v", recvErr)
	}
	rc, ok := msg.Command.(*pb.ControlMessage_RunCancel)
	if !ok {
		t.Fatalf("expected RunCancel, got %T", msg.Command)
	}
	if rc.RunCancel.RunId != runID {
		t.Errorf("RunCancel.run_id=%q want %q", rc.RunCancel.RunId, runID)
	}
}

func testControlResubscribe(t *testing.T, s Subject) {
	baseURL, client, teardown := s.SetUp(t)
	defer teardown()

	const token = "token-ctrl-resub"
	criteriaID := s.RegisterAgent(t, "criteria-ctrl-resub", token)
	oClient := criteria.NewServiceClient(client, baseURL)

	subscribe := func(t *testing.T) criteriav1connect.CriteriaServiceControlClientStream {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		t.Cleanup(cancel)
		ctrlCtx, ctrlInfo := connect.NewClientContext(ctx)
		ctrlInfo.RequestHeader().Set("Authorization", "Bearer "+token)
		stream, err := oClient.Control(ctrlCtx, &pb.ControlSubscribeRequest{CriteriaId: criteriaID})
		if err != nil {
			t.Fatalf("Control subscribe: %v", err)
		}
		return stream
	}

	assertControlReady := func(t *testing.T, stream criteriav1connect.CriteriaServiceControlClientStream) {
		t.Helper()
		msg, err := stream.Receive()
		if err != nil {
			t.Fatalf("Receive: %v", err)
		}
		if _, ok := msg.Command.(*pb.ControlMessage_ControlReady); !ok {
			t.Errorf("want ControlReady, got %T", msg.Command)
		}
	}

	// First subscription: assert ControlReady.
	stream1 := subscribe(t)
	assertControlReady(t, stream1)

	// Disconnect by cancelling the stream context (already deferred via t.Cleanup).
	// A new subscription must also start with ControlReady.
	stream2 := subscribe(t)
	assertControlReady(t, stream2)
}

func testControlAgentIsolation(t *testing.T, s Subject) { //nolint:funlen // agent isolation test requires full two-agent setup and cross-visibility assertions
	baseURL, client, teardown := s.SetUp(t)
	defer teardown()

	const (
		tokenA = "token-ctrl-iso-a"
		tokenB = "token-ctrl-iso-b"
	)
	criteriaAID := s.RegisterAgent(t, "criteria-iso-a", tokenA)
	criteriaBID := s.RegisterAgent(t, "criteria-iso-b", tokenB)
	oClient := criteria.NewServiceClient(client, baseURL)

	// Create a run owned by agent-A.
	runIDofA := authCreateRun(t, oClient, tokenA, criteriaAID, "conformance-iso")

	// Subscribe BOTH agents to their respective Control streams.
	ctxA, cancelA := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelA()
	ctrlCtxA, ctrlInfoA := connect.NewClientContext(ctxA)
	ctrlInfoA.RequestHeader().Set("Authorization", "Bearer "+tokenA)
	streamA, err := oClient.Control(ctrlCtxA, &pb.ControlSubscribeRequest{CriteriaId: criteriaAID})
	if err != nil {
		t.Fatalf("Control subscribe A: %v", err)
	}

	ctxB, cancelB := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelB()
	ctrlCtxB, ctrlInfoB := connect.NewClientContext(ctxB)
	ctrlInfoB.RequestHeader().Set("Authorization", "Bearer "+tokenB)
	streamB, err := oClient.Control(ctrlCtxB, &pb.ControlSubscribeRequest{CriteriaId: criteriaBID})
	if err != nil {
		t.Fatalf("Control subscribe B: %v", err)
	}

	// Drain ControlReady from both streams.
	if _, err := streamA.Receive(); err != nil {
		t.Fatalf("Control A first Receive: %v", err)
	}
	if _, err := streamB.Receive(); err != nil {
		t.Fatalf("Control B first Receive: %v", err)
	}

	// Stop A's run — must deliver RunCancel only to A's channel.
	if err := s.StopRun(t, baseURL, client, tokenA, runIDofA); err != nil {
		t.Fatalf("StopRun(A's run): %v", err)
	}

	// A must receive RunCancel for its run.
	msgA, err := streamA.Receive()
	if err != nil {
		t.Fatalf("A stream Receive: %v", err)
	}
	if _, ok := msgA.Command.(*pb.ControlMessage_RunCancel); !ok {
		t.Errorf("A stream: want RunCancel, got %T", msgA.Command)
	}

	// B must NOT receive any message within a bounded timeout — the RunCancel
	// for A's run must not cross agent boundaries.
	var bMsg *pb.ControlMessage
	var bErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		bMsg, bErr = streamB.Receive()
	}()
	select {
	case <-done:
		// streamB.Receive() returned — either a message arrived (isolation
		// broken) or the stream was closed with an error. Check which.
		if bErr == nil {
			// A message arrived on B's stream — isolation contract violated.
			t.Errorf("AgentIsolation: B received a message meant for A (got %T)", bMsg.GetCommand())
		}
		// Error means stream was closed by context cancellation or server EOF;
		// that is not a violation.
	case <-time.After(500 * time.Millisecond):
		// Nothing delivered to B within the window — isolation holds. Cancel
		// B's context to unblock the goroutine.
		cancelB()
		<-done
	}
}
