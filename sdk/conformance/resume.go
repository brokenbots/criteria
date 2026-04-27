package conformance

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	pb "github.com/brokenbots/overseer/sdk/pb/overseer/v1"
	overseer "github.com/brokenbots/overseer/sdk"
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
//     the durable-resume capability lands (tracked in PLAN.md as a future
//     conformance lane).
func testResumeCorrectness(t *testing.T, s Subject) {
	t.Run("WaitSignalResume", func(t *testing.T) {
		testResumeWaitSignal(t, s)
	})
	t.Run("SignalMismatch", func(t *testing.T) {
		testResumeSignalMismatch(t, s)
	})
	t.Run("NotPaused", func(t *testing.T) {
		t.Run("PendingRun", func(t *testing.T) { testResumeNotPaused_Pending(t, s) })
		t.Run("TerminalRun", func(t *testing.T) { testResumeNotPaused_Terminal(t, s) })
	})
	t.Run("ApprovalDecision", func(t *testing.T) {
		testResumeApprovalDecision(t, s)
	})
	t.Run("DurableAcrossRestart", func(t *testing.T) {
		// Deferred: when the durable-resume path lands, this skip lifts and
		// the test asserts that a Resume call from a disconnected overseer
		// can recover the signal on reconnect. Tracked in PLAN.md.
		t.Skip("durable resume across orchestrator restart not yet implemented; tracked in PLAN.md")
	})
}

func pauseRunViaWaitEntered(t *testing.T, oClient overseer.ServiceClient, token, runID, signal string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream := oClient.SubmitEvents(ctx)
	stream.RequestHeader().Set("Authorization", "Bearer "+token)
	env := overseer.NewEnvelope(runID, &pb.WaitEntered{
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
	stream.CloseRequest()
	for {
		if _, err := stream.Receive(); err != nil {
			break
		}
	}
}

func pauseRunViaApproval(t *testing.T, oClient overseer.ServiceClient, token, runID, node string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream := oClient.SubmitEvents(ctx)
	stream.RequestHeader().Set("Authorization", "Bearer "+token)
	env := overseer.NewEnvelope(runID, &pb.ApprovalRequested{
		Node: node,
	})
	env.CorrelationId = "pause-via-approval-" + node
	if err := stream.Send(env); err != nil {
		t.Fatalf("pauseRunViaApproval Send: %v", err)
	}
	if _, err := stream.Receive(); err != nil {
		t.Fatalf("pauseRunViaApproval Receive ack: %v", err)
	}
	stream.CloseRequest()
	for {
		if _, err := stream.Receive(); err != nil {
			break
		}
	}
}

func testResumeWaitSignal(t *testing.T, s Subject) {
	baseURL, client, teardown := s.SetUp(t)
	defer teardown()

	const (
		token  = "token-resume-wait"
		signal = "gate-alpha"
	)
	overseerID := s.RegisterOverseer(t, "overseer-resume-wait", token)
	oClient := overseer.NewServiceClient(client, baseURL)

	createReq := connect.NewRequest(&pb.CreateRunRequest{OverseerId: overseerID, WorkflowName: "conformance-resume-wait"})
	createReq.Header().Set("Authorization", "Bearer "+token)
	runResp, err := oClient.CreateRun(context.Background(), createReq)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	runID := runResp.Msg.RunId

	// Put the run in paused state by submitting a WaitEntered with signal.
	pauseRunViaWaitEntered(t, oClient, token, runID, signal)

	// Call Resume with the correct signal.
	resumeReq := connect.NewRequest(&pb.ResumeRequest{
		RunId:  runID,
		Signal: signal,
	})
	resumeReq.Header().Set("Authorization", "Bearer "+token)
	resumeResp, err := oClient.Resume(context.Background(), resumeReq)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if !resumeResp.Msg.Accepted {
		t.Errorf("Resume: accepted=false reason=%q, want accepted=true", resumeResp.Msg.Reason)
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
	overseerID := s.RegisterOverseer(t, "overseer-resume-mismatch", token)
	oClient := overseer.NewServiceClient(client, baseURL)

	createReq := connect.NewRequest(&pb.CreateRunRequest{OverseerId: overseerID, WorkflowName: "conformance-resume-mismatch"})
	createReq.Header().Set("Authorization", "Bearer "+token)
	runResp, err := oClient.CreateRun(context.Background(), createReq)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	runID := runResp.Msg.RunId
	pauseRunViaWaitEntered(t, oClient, token, runID, signal)

	resumeReq := connect.NewRequest(&pb.ResumeRequest{
		RunId:  runID,
		Signal: "wrong-signal",
	})
	resumeReq.Header().Set("Authorization", "Bearer "+token)
	resp, err := oClient.Resume(context.Background(), resumeReq)
	if err != nil {
		t.Fatalf("Resume: unexpected error: %v", err)
	}
	if resp.Msg.Accepted {
		t.Errorf("Resume with wrong signal: accepted=true, want false")
	}
	if resp.Msg.Reason != "signal_mismatch" {
		t.Errorf("Resume with wrong signal: reason=%q, want %q", resp.Msg.Reason, "signal_mismatch")
	}
}

// testResumeNotPaused_Pending asserts that Resume on a run that was never paused
// (pending/running state) returns accepted=false, reason="run_not_paused".
func testResumeNotPaused_Pending(t *testing.T, s Subject) {
	baseURL, client, teardown := s.SetUp(t)
	defer teardown()

	const token = "token-resume-notpaused"
	overseerID := s.RegisterOverseer(t, "overseer-resume-notpaused", token)
	oClient := overseer.NewServiceClient(client, baseURL)

	createReq := connect.NewRequest(&pb.CreateRunRequest{OverseerId: overseerID, WorkflowName: "conformance-resume-notpaused"})
	createReq.Header().Set("Authorization", "Bearer "+token)
	runResp, err := oClient.CreateRun(context.Background(), createReq)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	runID := runResp.Msg.RunId
	// Do NOT pause the run.

	assertNotPaused(t, oClient, token, runID)
}

// testResumeNotPaused_Terminal asserts that Resume on a terminal run (one that
// has received RunCompleted) returns accepted=false, reason="run_not_paused".
//
// This sub-test is regression-resistant against implementations that return a
// distinct reason for terminal runs (e.g. "run_terminal"): any such deviation
// from the spec would break the assertion.
func testResumeNotPaused_Terminal(t *testing.T, s Subject) {
	baseURL, client, teardown := s.SetUp(t)
	defer teardown()

	const token = "token-resume-terminal"
	overseerID := s.RegisterOverseer(t, "overseer-resume-terminal", token)
	oClient := overseer.NewServiceClient(client, baseURL)

	createReq := connect.NewRequest(&pb.CreateRunRequest{OverseerId: overseerID, WorkflowName: "conformance-resume-terminal"})
	createReq.Header().Set("Authorization", "Bearer "+token)
	runResp, err := oClient.CreateRun(context.Background(), createReq)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	runID := runResp.Msg.RunId

	// Drive the run to a terminal state by submitting RunCompleted.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream := oClient.SubmitEvents(ctx)
	stream.RequestHeader().Set("Authorization", "Bearer "+token)
	env := overseer.NewEnvelope(runID, &pb.RunCompleted{})
	env.CorrelationId = "terminal-completed"
	if err := stream.Send(env); err != nil {
		t.Fatalf("Send RunCompleted: %v", err)
	}
	if _, err := stream.Receive(); err != nil {
		t.Fatalf("Receive ack for RunCompleted: %v", err)
	}
	_ = stream.CloseRequest()
	for {
		if _, err := stream.Receive(); err != nil {
			break
		}
	}

	assertNotPaused(t, oClient, token, runID)
}

// assertNotPaused calls Resume on runID and asserts the response is
// accepted=false, reason="run_not_paused". Used by both NotPaused sub-tests.
func assertNotPaused(t *testing.T, oClient overseer.ServiceClient, token, runID string) {
	t.Helper()
	resumeReq := connect.NewRequest(&pb.ResumeRequest{
		RunId:  runID,
		Signal: "any",
	})
	resumeReq.Header().Set("Authorization", "Bearer "+token)
	resp, err := oClient.Resume(context.Background(), resumeReq)
	if err != nil {
		t.Fatalf("Resume: unexpected error: %v", err)
	}
	if resp.Msg.Accepted {
		t.Errorf("Resume on non-paused run: accepted=true, want false")
	}
	if resp.Msg.Reason != "run_not_paused" {
		t.Errorf("Resume on non-paused run: reason=%q, want %q", resp.Msg.Reason, "run_not_paused")
	}
}

func testResumeApprovalDecision(t *testing.T, s Subject) {
	baseURL, client, teardown := s.SetUp(t)
	defer teardown()

	const (
		token = "token-resume-approval"
		node  = "approve-gate"
	)
	overseerID := s.RegisterOverseer(t, "overseer-resume-approval", token)
	oClient := overseer.NewServiceClient(client, baseURL)

	createReq := connect.NewRequest(&pb.CreateRunRequest{OverseerId: overseerID, WorkflowName: "conformance-resume-approval"})
	createReq.Header().Set("Authorization", "Bearer "+token)
	runResp, err := oClient.CreateRun(context.Background(), createReq)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	runID := runResp.Msg.RunId
	pauseRunViaApproval(t, oClient, token, runID, node)

	resumeReq := connect.NewRequest(&pb.ResumeRequest{
		RunId:  runID,
		Signal: node,
		Payload: map[string]string{
			"decision": "approved",
			"actor":    "tester",
		},
	})
	resumeReq.Header().Set("Authorization", "Bearer "+token)
	resp, err := oClient.Resume(context.Background(), resumeReq)
	if err != nil {
		t.Fatalf("Resume (approval): %v", err)
	}
	if !resp.Msg.Accepted {
		t.Errorf("Resume (approval): accepted=false reason=%q, want accepted=true", resp.Msg.Reason)
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
