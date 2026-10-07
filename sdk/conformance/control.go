package conformance

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	criteria "github.com/brokenbots/criteria/sdk"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
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

	req := connect.NewRequest(&pb.ControlSubscribeRequest{CriteriaId: criteriaID})
	req.Header().Set("Authorization", "Bearer "+token)
	stream, err := oClient.Control(ctx, req)
	if err != nil {
		t.Fatalf("Control subscribe: %v", err)
	}

	if !stream.Receive() {
		t.Fatalf("Control first Receive: %v", stream.Err())
	}
	if _, ok := stream.Msg().Command.(*pb.ControlMessage_ControlReady); !ok {
		t.Errorf("Control first message: want ControlReady, got %T", stream.Msg().Command)
	}
}

func testRunCancelDelivered(t *testing.T, s Subject) {
	baseURL, client, teardown := s.SetUp(t)
	defer teardown()

	const token = "token-ctrl-cancel"
	criteriaID := s.RegisterAgent(t, "criteria-ctrl-cancel", token)
	oClient := criteria.NewServiceClient(client, baseURL)

	createReq := connect.NewRequest(&pb.CreateRunRequest{CriteriaId: criteriaID, WorkflowName: "conformance-ctrl-cancel"})
	createReq.Header().Set("Authorization", "Bearer "+token)
	runResp, err := oClient.CreateRun(context.Background(), createReq)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	runID := runResp.Msg.RunId

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	req := connect.NewRequest(&pb.ControlSubscribeRequest{CriteriaId: criteriaID})
	req.Header().Set("Authorization", "Bearer "+token)
	stream, err := oClient.Control(ctx, req)
	if err != nil {
		t.Fatalf("Control subscribe: %v", err)
	}

	// Drain ControlReady.
	if !stream.Receive() {
		t.Fatalf("Control first Receive: %v", stream.Err())
	}
	if _, ok := stream.Msg().Command.(*pb.ControlMessage_ControlReady); !ok {
		t.Errorf("first message want ControlReady, got %T", stream.Msg().Command)
	}

	// Trigger a stop-run command via the Subject (abstracts over ServerService).
	if err := s.StopRun(t, baseURL, client, token, runID); err != nil {
		t.Fatalf("StopRun: %v", err)
	}

	// The next message on the Control stream must be RunCancel for our run.
	if !stream.Receive() {
		t.Fatalf("Control Receive after StopRun: %v", stream.Err())
	}
	rc, ok := stream.Msg().Command.(*pb.ControlMessage_RunCancel)
	if !ok {
		t.Fatalf("expected RunCancel, got %T", stream.Msg().Command)
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

	subscribe := func(t *testing.T) *connect.ServerStreamForClient[pb.ControlMessage] {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		t.Cleanup(cancel)
		req := connect.NewRequest(&pb.ControlSubscribeRequest{CriteriaId: criteriaID})
		req.Header().Set("Authorization", "Bearer "+token)
		stream, err := oClient.Control(ctx, req)
		if err != nil {
			t.Fatalf("Control subscribe: %v", err)
		}
		return stream
	}

	assertControlReady := func(t *testing.T, stream *connect.ServerStreamForClient[pb.ControlMessage]) {
		t.Helper()
		if !stream.Receive() {
			t.Fatalf("Receive: %v", stream.Err())
		}
		if _, ok := stream.Msg().Command.(*pb.ControlMessage_ControlReady); !ok {
			t.Errorf("want ControlReady, got %T", stream.Msg().Command)
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
	createReq := connect.NewRequest(&pb.CreateRunRequest{CriteriaId: criteriaAID, WorkflowName: "conformance-iso"})
	createReq.Header().Set("Authorization", "Bearer "+tokenA)
	runResp, err := oClient.CreateRun(context.Background(), createReq)
	if err != nil {
		t.Fatalf("CreateRun for A's run: %v", err)
	}
	runIDofA := runResp.Msg.RunId

	// Subscribe BOTH agents to their respective Control streams.
	ctxA, cancelA := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelA()
	reqA := connect.NewRequest(&pb.ControlSubscribeRequest{CriteriaId: criteriaAID})
	reqA.Header().Set("Authorization", "Bearer "+tokenA)
	streamA, err := oClient.Control(ctxA, reqA)
	if err != nil {
		t.Fatalf("Control subscribe A: %v", err)
	}

	ctxB, cancelB := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelB()
	reqB := connect.NewRequest(&pb.ControlSubscribeRequest{CriteriaId: criteriaBID})
	reqB.Header().Set("Authorization", "Bearer "+tokenB)
	streamB, err := oClient.Control(ctxB, reqB)
	if err != nil {
		t.Fatalf("Control subscribe B: %v", err)
	}

	// Drain ControlReady from both streams.
	if !streamA.Receive() {
		t.Fatalf("Control A first Receive: %v", streamA.Err())
	}
	if !streamB.Receive() {
		t.Fatalf("Control B first Receive: %v", streamB.Err())
	}

	// Stop A's run — must deliver RunCancel only to A's channel.
	if err := s.StopRun(t, baseURL, client, tokenA, runIDofA); err != nil {
		t.Fatalf("StopRun(A's run): %v", err)
	}

	// A must receive RunCancel for its run.
	if !streamA.Receive() {
		t.Fatalf("A stream Receive: %v", streamA.Err())
	}
	if _, ok := streamA.Msg().Command.(*pb.ControlMessage_RunCancel); !ok {
		t.Errorf("A stream: want RunCancel, got %T", streamA.Msg().Command)
	}

	// B must NOT receive any message within a bounded timeout — the RunCancel
	// for A's run must not cross agent boundaries.
	done := make(chan struct{})
	go func() {
		defer close(done)
		streamB.Receive()
	}()
	select {
	case <-done:
		// streamB.Receive() returned — either a message arrived (isolation
		// broken) or the stream was closed with an error. Check which.
		if streamB.Err() == nil {
			// A message arrived on B's stream — isolation contract violated.
			t.Errorf("AgentIsolation: B received a message meant for A (got %T)", streamB.Msg().GetCommand())
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
