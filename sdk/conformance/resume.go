package conformance

import (
	"context"
	"testing"
	"time"

	connect "connectrpc.com/connect/v2"

	criteria "github.com/brokenbots/criteria/sdk"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

// testResumeCorrectness verifies the Resume RPC contract.
//
// Test cases:
//  1. Signal-mode wait: WaitEntered event puts run in paused state; Resume
//     with matching signal returns accepted=true and persists WaitResumed.
//  2. Signal mismatch: Resume with wrong signal returns accepted=false,
//     reason="signal_mismatch".
//  3. Non-paused run: Resume on a non-paused run returns accepted=false,
//     reason="run_not_paused".
//  4. Approval: ApprovalRequested puts run in paused state; Resume with
//     decision=approved returns accepted=true and persists ApprovalDecision.
//  5. (Skipped) Durable resume across orchestrator restart — deferred until
//     the durable-resume capability lands.
func testResumeCorrectness(t *testing.T, s Subject) {
	t.Run("WaitSignalResume", func(t *testing.T) {
		testResumeWaitSignal(t, s)
	})
	t.Run("SignalMismatch", func(t *testing.T) {
		testResumeSignalMismatch(t, s)
	})
	t.Run("NotPaused", func(t *testing.T) {
		t.Run("PendingRun", func(t *testing.T) { testResumeNotPausedPending(t, s) })
		t.Run("TerminalRun", func(t *testing.T) { testResumeNotPausedTerminal(t, s) })
	})
	t.Run("ApprovalDecision", func(t *testing.T) {
		testResumeApprovalDecision(t, s)
	})
	t.Run("DurableAcrossRestart", func(t *testing.T) {
		// Deferred: when the durable-resume path lands, this skip lifts and
		// the test asserts that a Resume call from a disconnected agent
		// can recover the signal on reconnect.
		t.Skip("durable resume across orchestrator restart not yet implemented")
	})
}

func pauseRunViaWaitEntered(t *testing.T, oClient criteria.ServiceClient, token, runID, signal string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream := authSubmitStream(t, oClient, ctx, token)
	env := criteria.NewEnvelope(runID, &pb.WaitEntered{
		Node:   signal,
		Signal: signal,
		Mode:   "signal",
	})
	env.CorrelationId = "pause-via-wait-" + signal
	if err := stream.Send(env); err != nil {
		t.Fatalf("pauseRunViaWaitEntered Send: %v", err)
	}
	if _, err := stream.Receive(); err != nil {
		t.Fatalf("pauseRunViaWaitEntered Receive ack: %v", err)
	}
	_ = stream.CloseSend()
	submitDrain(stream)
}

func pauseRunViaApproval(t *testing.T, oClient criteria.ServiceClient, token, runID, node string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream := authSubmitStream(t, oClient, ctx, token)
	env := criteria.NewEnvelope(runID, &pb.ApprovalRequested{
		Node: node,
	})
	env.CorrelationId = "pause-via-approval-" + node
	if err := stream.Send(env); err != nil {
		t.Fatalf("pauseRunViaApproval Send: %v", err)
	}
	if _, err := stream.Receive(); err != nil {
		t.Fatalf("pauseRunViaApproval Receive ack: %v", err)
	}
	_ = stream.CloseSend()
	submitDrain(stream)
}

func testResumeWaitSignal(t *testing.T, s Subject) {
	baseURL, client, teardown := s.SetUp(t)
	defer teardown()

	const (
		token  = "token-resume-wait"
		signal = "gate-alpha"
	)
	criteriaID := s.RegisterAgent(t, "criteria-resume-wait", token)
	oClient := criteria.NewServiceClient(client, baseURL)
	runID := authCreateRun(t, oClient, token, criteriaID, "conformance-resume-wait")

	// Put the run in paused state by submitting a WaitEntered with signal.
	pauseRunViaWaitEntered(t, oClient, token, runID, signal)

	// Call Resume with the correct signal.
	resumeCtx, resumeInfo := connect.NewClientContext(context.Background())
	resumeInfo.RequestHeader().Set("Authorization", "Bearer "+token)
	resumeResp, err := oClient.Resume(resumeCtx, &pb.ResumeRequest{
		RunId:  runID,
		Signal: signal,
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if !resumeResp.Accepted {
		t.Errorf("Resume: accepted=false reason=%q, want accepted=true", resumeResp.Reason)
	}

	// Assert WaitResumed event is durably persisted before Resume returned.
	// We query immediately — no sleep — to verify the atomicity guarantee.
	events := s.ListRunEvents(t, baseURL, client, token, runID, 0)
	var foundResumed bool
	for _, ev := range events {
		if _, ok := ev.Payload.(*pb.Envelope_WaitResumed); ok {
			wr := ev.Payload.(*pb.Envelope_WaitResumed).WaitResumed
			if wr.Signal == signal {
				foundResumed = true
				break
			}
		}
	}
	if !foundResumed {
		t.Errorf("WaitResumed event not found in ListRunEvents after Resume returned (expected durable persistence)")
	}
}

func testResumeSignalMismatch(t *testing.T, s Subject) {
	baseURL, client, teardown := s.SetUp(t)
	defer teardown()

	const (
		token  = "token-resume-mismatch"
		signal = "gate-beta"
	)
	criteriaID := s.RegisterAgent(t, "criteria-resume-mismatch", token)
	oClient := criteria.NewServiceClient(client, baseURL)
	runID := authCreateRun(t, oClient, token, criteriaID, "conformance-resume-mismatch")
	pauseRunViaWaitEntered(t, oClient, token, runID, signal)

	resumeCtx, resumeInfo := connect.NewClientContext(context.Background())
	resumeInfo.RequestHeader().Set("Authorization", "Bearer "+token)
	resp, err := oClient.Resume(resumeCtx, &pb.ResumeRequest{
		RunId:  runID,
		Signal: "wrong-signal",
	})
	if err != nil {
		t.Fatalf("Resume: unexpected error: %v", err)
	}
	if resp.Accepted {
		t.Errorf("Resume with wrong signal: accepted=true, want false")
	}
	if resp.Reason != "signal_mismatch" {
		t.Errorf("Resume with wrong signal: reason=%q, want %q", resp.Reason, "signal_mismatch")
	}
}

// testResumeNotPausedPending asserts that Resume on a run that was never paused
// (pending/running state) returns accepted=false, reason="run_not_paused".
func testResumeNotPausedPending(t *testing.T, s Subject) {
	baseURL, client, teardown := s.SetUp(t)
	defer teardown()

	const token = "token-resume-notpaused"
	criteriaID := s.RegisterAgent(t, "criteria-resume-notpaused", token)
	oClient := criteria.NewServiceClient(client, baseURL)
	runID := authCreateRun(t, oClient, token, criteriaID, "conformance-resume-notpaused")
	// Do NOT pause the run.

	assertNotPaused(t, oClient, token, runID)
}

// testResumeNotPausedTerminal asserts that Resume on a terminal run (one that
// has received RunCompleted) returns accepted=false, reason="run_not_paused".
//
// This sub-test is regression-resistant against implementations that return a
// distinct reason for terminal runs (e.g. "run_terminal"): any such deviation
// from the spec would break the assertion.
func testResumeNotPausedTerminal(t *testing.T, s Subject) {
	baseURL, client, teardown := s.SetUp(t)
	defer teardown()

	const token = "token-resume-terminal"
	criteriaID := s.RegisterAgent(t, "criteria-resume-terminal", token)
	oClient := criteria.NewServiceClient(client, baseURL)
	runID := authCreateRun(t, oClient, token, criteriaID, "conformance-resume-terminal")

	// Drive the run to a terminal state by submitting RunCompleted.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream := authSubmitStream(t, oClient, ctx, token)
	env := criteria.NewEnvelope(runID, &pb.RunCompleted{})
	env.CorrelationId = "terminal-completed"
	if err := stream.Send(env); err != nil {
		t.Fatalf("Send RunCompleted: %v", err)
	}
	if _, err := stream.Receive(); err != nil {
		t.Fatalf("Receive ack for RunCompleted: %v", err)
	}
	_ = stream.CloseSend()
	submitDrain(stream)

	assertNotPaused(t, oClient, token, runID)
}

// assertNotPaused calls Resume on runID and asserts the response is
// accepted=false, reason="run_not_paused". Used by both NotPaused sub-tests.
func assertNotPaused(t *testing.T, oClient criteria.ServiceClient, token, runID string) {
	t.Helper()
	resumeCtx, resumeInfo := connect.NewClientContext(context.Background())
	resumeInfo.RequestHeader().Set("Authorization", "Bearer "+token)
	resp, err := oClient.Resume(resumeCtx, &pb.ResumeRequest{
		RunId:  runID,
		Signal: "any",
	})
	if err != nil {
		t.Fatalf("Resume: unexpected error: %v", err)
	}
	if resp.Accepted {
		t.Errorf("Resume on non-paused run: accepted=true, want false")
	}
	if resp.Reason != "run_not_paused" {
		t.Errorf("Resume on non-paused run: reason=%q, want %q", resp.Reason, "run_not_paused")
	}
}

func testResumeApprovalDecision(t *testing.T, s Subject) {
	baseURL, client, teardown := s.SetUp(t)
	defer teardown()

	const (
		token = "token-resume-approval"
		node  = "approve-gate"
	)
	criteriaID := s.RegisterAgent(t, "criteria-resume-approval", token)
	oClient := criteria.NewServiceClient(client, baseURL)
	runID := authCreateRun(t, oClient, token, criteriaID, "conformance-resume-approval")
	pauseRunViaApproval(t, oClient, token, runID, node)

	resumeCtx, resumeInfo := connect.NewClientContext(context.Background())
	resumeInfo.RequestHeader().Set("Authorization", "Bearer "+token)
	resp, err := oClient.Resume(resumeCtx, &pb.ResumeRequest{
		RunId:  runID,
		Signal: node,
		Payload: map[string]string{
			"decision": "approved",
			"actor":    "tester",
		},
	})
	if err != nil {
		t.Fatalf("Resume (approval): %v", err)
	}
	if !resp.Accepted {
		t.Errorf("Resume (approval): accepted=false reason=%q, want accepted=true", resp.Reason)
	}

	// Assert ApprovalDecision event is durably persisted.
	events := s.ListRunEvents(t, baseURL, client, token, runID, 0)
	var foundDecision bool
	for _, ev := range events {
		if ad, ok := ev.Payload.(*pb.Envelope_ApprovalDecision); ok {
			if ad.ApprovalDecision.Node == node && ad.ApprovalDecision.Decision == "approved" {
				foundDecision = true
				break
			}
		}
	}
	if !foundDecision {
		t.Errorf("ApprovalDecision event not found in ListRunEvents after Resume returned (expected durable persistence)")
	}
}
