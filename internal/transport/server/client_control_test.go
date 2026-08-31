package servertrans

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

// waitForCtlAttach drains one value from f.ctlAttached, failing if it does not
// arrive within the timeout.
func waitForCtlAttach(t *testing.T, f *fakeServer) {
	t.Helper()
	select {
	case <-f.ctlAttached:
	case <-time.After(2 * time.Second):
		t.Fatal("control stream never attached")
	}
}

// TestClientControlStreamDeliversWorkflowAssignment verifies that a
// WorkflowAssignment control message is delivered to AssignmentCh and that the
// drop branch is exercised when the buffered channel is full.
func TestClientControlStreamDeliversWorkflowAssignment(t *testing.T) {
	f := newFakeServer()
	url := startFakeServer(t, f)

	c, err := NewClient(url, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer c.Close()

	if err := c.Register(ctx, "n", "h", "v"); err != nil {
		t.Fatal(err)
	}
	if err := c.StartControl(ctx); err != nil {
		t.Fatalf("StartControl: %v", err)
	}
	waitForCtlAttach(t, f)

	f.controls <- &pb.ControlMessage{Command: &pb.ControlMessage_WorkflowAssignment{WorkflowAssignment: &pb.WorkflowAssignment{
		RunId:          "run-1",
		WorkflowName:   "wf",
		WorkflowSource: "workflow {}",
	}}}

	select {
	case got := <-c.AssignmentCh():
		if got == nil {
			t.Fatal("received nil assignment")
		}
		if got.RunId != "run-1" {
			t.Fatalf("RunId: got %q want run-1", got.RunId)
		}
		if got.WorkflowName != "wf" {
			t.Fatalf("WorkflowName: got %q want wf", got.WorkflowName)
		}
		if got.WorkflowSource != "workflow {}" {
			t.Fatalf("WorkflowSource: got %q", got.WorkflowSource)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("workflow assignment not delivered")
	}

	// Pre-fill the assignment channel to capacity so the next control message
	// must take the non-blocking select default branch and be dropped.
	for i := 0; i < 32; i++ {
		c.assignmentCh <- &pb.WorkflowAssignment{WorkflowName: "filler"}
	}
	f.controls <- &pb.ControlMessage{Command: &pb.ControlMessage_WorkflowAssignment{WorkflowAssignment: &pb.WorkflowAssignment{
		RunId:          "run-dropped",
		WorkflowName:   "real",
		WorkflowSource: "workflow {}",
	}}}

	// Give controlLoop a moment to process the message before draining; without
	// this the non-blocking send may see a slot opened by the concurrent drain.
	time.Sleep(200 * time.Millisecond)

	delivered := 0
	sawReal := false
	drain := time.NewTimer(500 * time.Millisecond)
	defer drain.Stop()
drainLoop:
	for {
		select {
		case got := <-c.AssignmentCh():
			delivered++
			if got.WorkflowName == "real" {
				sawReal = true
			}
		case <-drain.C:
			break drainLoop
		}
	}
	if delivered != 32 {
		t.Fatalf("expected 32 delivered assignments, got %d", delivered)
	}
	if sawReal {
		t.Fatal("expected the dropped assignment to be discarded")
	}
}

// TestClientControlStreamDeliversResumeRun verifies that a ResumeRun control
// message is delivered to ResumeCh and that the drop branch is exercised when
// the channel is full.
func TestClientControlStreamDeliversResumeRun(t *testing.T) {
	f := newFakeServer()
	url := startFakeServer(t, f)

	c, err := NewClient(url, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer c.Close()

	if err := c.Register(ctx, "n", "h", "v"); err != nil {
		t.Fatal(err)
	}
	if err := c.StartControl(ctx); err != nil {
		t.Fatalf("StartControl: %v", err)
	}
	waitForCtlAttach(t, f)

	f.controls <- &pb.ControlMessage{Command: &pb.ControlMessage_ResumeRun{ResumeRun: &pb.ResumeRun{
		RunId: "run-1", Signal: "resume",
	}}}

	select {
	case got := <-c.ResumeCh():
		if got == nil {
			t.Fatal("received nil resume")
		}
		if got.RunId != "run-1" {
			t.Fatalf("RunId: got %q want run-1", got.RunId)
		}
		if got.Signal != "resume" {
			t.Fatalf("Signal: got %q want resume", got.Signal)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("resume run not delivered")
	}

	for i := 0; i < 32; i++ {
		c.resumeCh <- &pb.ResumeRun{RunId: "filler"}
	}
	f.controls <- &pb.ControlMessage{Command: &pb.ControlMessage_ResumeRun{ResumeRun: &pb.ResumeRun{
		RunId:  "real",
		Signal: "resume",
	}}}

	// Allow controlLoop to process the message before draining so the drop
	// branch is hit deterministically.
	time.Sleep(200 * time.Millisecond)

	delivered := 0
	sawReal := false
	drain := time.NewTimer(500 * time.Millisecond)
	defer drain.Stop()
drainLoop:
	for {
		select {
		case got := <-c.ResumeCh():
			delivered++
			if got.RunId == "real" {
				sawReal = true
			}
		case <-drain.C:
			break drainLoop
		}
	}
	if delivered != 32 {
		t.Fatalf("expected 32 delivered resume messages, got %d", delivered)
	}
	if sawReal {
		t.Fatal("expected the dropped resume message to be discarded")
	}
}

// TestClientControlStreamDropsRunCancelWhenFull verifies the default branch of
// the non-blocking RunCancel dispatch.
func TestClientControlStreamDropsRunCancelWhenFull(t *testing.T) {
	f := newFakeServer()
	url := startFakeServer(t, f)

	c, err := NewClient(url, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer c.Close()

	if err := c.Register(ctx, "n", "h", "v"); err != nil {
		t.Fatal(err)
	}
	if err := c.StartControl(ctx); err != nil {
		t.Fatalf("StartControl: %v", err)
	}
	waitForCtlAttach(t, f)

	for i := 0; i < 32; i++ {
		c.runCancelCh <- "filler"
	}
	f.controls <- &pb.ControlMessage{Command: &pb.ControlMessage_RunCancel{RunCancel: &pb.RunCancel{RunId: "real", Reason: "x"}}}

	time.Sleep(200 * time.Millisecond)

	delivered := 0
	sawReal := false
	drain := time.NewTimer(500 * time.Millisecond)
	defer drain.Stop()
drainLoop:
	for {
		select {
		case got := <-c.RunCancelCh():
			delivered++
			if got == "real" {
				sawReal = true
			}
		case <-drain.C:
			break drainLoop
		}
	}
	if delivered != 32 {
		t.Fatalf("expected 32 delivered run.cancel messages, got %d", delivered)
	}
	if sawReal {
		t.Fatal("expected the dropped run.cancel message to be discarded")
	}
}

// TestClientControlStreamLogsCloseError verifies that the control loop logs a
// non-context.Canceled stream close error and reconnects.
func TestClientControlStreamLogsCloseError(t *testing.T) {
	f := newFakeServer()
	f.controlFailAfterMessageCount = 1
	url := startFakeServer(t, f)

	c, err := NewClient(url, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer c.Close()

	if err := c.Register(ctx, "n", "h", "v"); err != nil {
		t.Fatal(err)
	}
	if err := c.StartControl(ctx); err != nil {
		t.Fatalf("StartControl: %v", err)
	}
	waitForCtlAttach(t, f)
	select {
	case <-f.ctlAttached:
	default:
	}

	f.controls <- &pb.ControlMessage{Command: &pb.ControlMessage_ResumeRun{ResumeRun: &pb.ResumeRun{RunId: "run-1", Signal: "resume"}}}

	select {
	case got := <-c.ResumeCh():
		if got.RunId != "run-1" {
			t.Fatalf("RunId: got %q want run-1", got.RunId)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("resume not delivered")
	}

	waitForCtlAttach(t, f)
}

// TestClientControlReconnectAfterStreamClose verifies that the control loop
// reconnects after the server closes the stream.
func TestClientControlReconnectAfterStreamClose(t *testing.T) {
	f := newFakeServer()
	f.controlCloseAfterAttach = true
	f.controlFailOnAttempt = 2
	url := startFakeServer(t, f)

	c, err := NewClient(url, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer c.Close()

	if err := c.Register(ctx, "n", "h", "v"); err != nil {
		t.Fatal(err)
	}
	if err := c.StartControl(ctx); err != nil {
		t.Fatalf("StartControl: %v", err)
	}
	waitForCtlAttach(t, f)
	// Drain the first attach signal so the reconnect can fire again.
	select {
	case <-f.ctlAttached:
	default:
	}

	waitForCtlAttach(t, f)

	if !waitForCond(t, 3*time.Second, func() bool {
		return f.controlAttempts.Load() >= 3
	}) {
		t.Fatalf("expected at least 3 Control attempts, got %d", f.controlAttempts.Load())
	}
}

// TestClientControlReconnectStopsOnClose verifies that a reconnect dial
// failure stops cleanly when the client is closed during the reconnect backoff,
// covering the return inside the non-first Control error branch.
func TestClientControlReconnectStopsOnClose(t *testing.T) {
	f := newFakeServer()
	f.controlCloseAfterAttach = true
	f.controlFailOnAttempt = 2
	url := startFakeServer(t, f)

	c, err := NewClient(url, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := c.Register(ctx, "n", "h", "v"); err != nil {
		t.Fatal(err)
	}
	if err := c.StartControl(ctx); err != nil {
		t.Fatalf("StartControl: %v", err)
	}
	waitForCtlAttach(t, f)

	// Wait for the first reconnect to be attempted and fail.
	if !waitForCond(t, 2*time.Second, func() bool {
		return f.controlAttempts.Load() >= 2
	}) {
		t.Fatalf("expected at least 2 Control attempts, got %d", f.controlAttempts.Load())
	}

	// Closing the client should stop the backoff/reconnect goroutine.
	_ = c.Close()
	requireNoGoroutineLeak(t)
}

// TestClientStartControl_AlreadyCanceled verifies that starting control with
// an already-cancelled context returns immediately and covers the top-level
// context check in controlLoop.
func TestClientStartControl_AlreadyCanceled(t *testing.T) {
	f := newFakeServer()
	url := startFakeServer(t, f)

	c, err := NewClient(url, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if err := c.Register(context.Background(), "n", "h", "v"); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := c.StartControl(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

// TestClientStartControl_ClosedBeforeReady verifies the "stream closed before
// ready" branch in controlLoop.
func TestClientStartControl_ClosedBeforeReady(t *testing.T) {
	f := newFakeServer()
	f.controlCloseBeforeReady = true
	url := startFakeServer(t, f)

	c, err := NewClient(url, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if err := c.Register(context.Background(), "n", "h", "v"); err != nil {
		t.Fatal(err)
	}

	err = c.StartControl(context.Background())
	if err == nil {
		t.Fatal("expected error from StartControl")
	}
	if !strings.Contains(err.Error(), "closed before ready") {
		t.Fatalf("expected 'closed before ready' error, got %v", err)
	}
}

// TestClientStartControl_ContextCancel verifies that cancelling the context
// while waiting for ControlReady returns ctx.Err().
func TestClientStartControl_ContextCancel(t *testing.T) {
	f := newFakeServer()
	f.controlSkipReady = true
	url := startFakeServer(t, f)

	c, err := NewClient(url, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if err := c.Register(context.Background(), "n", "h", "v"); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var startErr error
	go func() {
		startErr = c.StartControl(ctx)
		close(done)
	}()

	// Give the goroutine time to enter Control before cancelling.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("StartControl did not return after context cancel")
	}
	if !errors.Is(startErr, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", startErr)
	}
}

// TestClientStartControl_Timeout verifies that StartControl returns a timeout
// error when the server never sends ControlReady.
func TestClientStartControl_Timeout(t *testing.T) {
	f := newFakeServer()
	f.controlSkipReady = true
	url := startFakeServer(t, f)

	c, err := NewClient(url, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if err := c.Register(context.Background(), "n", "h", "v"); err != nil {
		t.Fatal(err)
	}

	// Use a cancellable context so we can stop the retry goroutine after the
	// timeout error is observed.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err = c.StartControl(ctx)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if err.Error() != "control stream: timed out waiting for ready" {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestClientCreateRun_NotRegistered verifies CreateRun returns an error when the
// client has not been registered.
func TestClientCreateRun_NotRegistered(t *testing.T) {
	requireNoGoroutineLeak(t)

	c, err := NewClient("http://localhost:9999", newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, err = c.CreateRun(ctx, "wf", "hcl")
	if err == nil {
		t.Fatal("expected error from CreateRun before Register")
	}
	if err.Error() != "not registered" {
		t.Fatalf("CreateRun error: got %q want \"not registered\"", err.Error())
	}
}

// TestClientCreateRun_ServerError verifies that a server error from CreateRun
// is propagated to the caller.
func TestClientCreateRun_ServerError(t *testing.T) {
	f := newFakeServer()
	f.createRunErr = errors.New("create run failed")
	url := startFakeServer(t, f)

	c, err := NewClient(url, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer c.Close()

	if err := c.Register(ctx, "n", "h", "v"); err != nil {
		t.Fatal(err)
	}
	_, err = c.CreateRun(ctx, "wf", "hcl")
	if err == nil {
		t.Fatal("expected error from CreateRun")
	}
	if !strings.Contains(err.Error(), "create run failed") {
		t.Fatalf("CreateRun error: got %v", err)
	}
}

// TestClientReattachRun_Dispatch verifies that ReattachRun sends the expected
// request payload to the server.
func TestClientReattachRun_Dispatch(t *testing.T) {
	f := newFakeServer()
	url := startFakeServer(t, f)

	c, err := NewClient(url, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer c.Close()

	_, err = c.ReattachRun(ctx, "run-99", f.criteriaID)
	if err != nil {
		t.Fatalf("ReattachRun: %v", err)
	}

	f.mu.Lock()
	got := f.reattachRunReq
	f.mu.Unlock()
	if got == nil {
		t.Fatal("server did not receive ReattachRun request")
	}
	if got.RunId != "run-99" {
		t.Fatalf("RunId: got %q want run-99", got.RunId)
	}
	if got.CriteriaId != f.criteriaID {
		t.Fatalf("CriteriaId: got %q want %q", got.CriteriaId, f.criteriaID)
	}
}

// TestClientResume_Dispatch verifies that Resume sends the expected payload,
// including the empty-payload branch.
func TestClientResume_Dispatch(t *testing.T) {
	f := newFakeServer()
	url := startFakeServer(t, f)

	c, err := NewClient(url, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer c.Close()

	if err := c.Register(ctx, "n", "h", "v"); err != nil {
		t.Fatal(err)
	}

	t.Run("empty_payload", func(t *testing.T) {
		_, err := c.Resume(ctx, "run-1", "received", nil)
		if err != nil {
			t.Fatalf("Resume: %v", err)
		}
		f.mu.Lock()
		got := f.lastResumeReq
		f.mu.Unlock()
		if got == nil {
			t.Fatal("server did not receive Resume request")
		}
		if got.RunId != "run-1" {
			t.Fatalf("RunId: got %q want run-1", got.RunId)
		}
		if got.Signal != "received" {
			t.Fatalf("Signal: got %q want received", got.Signal)
		}
		if len(got.Payload) != 0 {
			t.Fatalf("expected empty payload, got %v", got.Payload)
		}
	})
}

// TestClientRegister_ServerError verifies that a server error from Register is
// propagated to the caller.
func TestClientRegister_ServerError(t *testing.T) {
	f := newFakeServer()
	f.registerErr = errors.New("register failed")
	url := startFakeServer(t, f)

	c, err := NewClient(url, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer c.Close()

	err = c.Register(ctx, "n", "h", "v")
	if err == nil {
		t.Fatal("expected error from Register")
	}
	if !strings.Contains(err.Error(), "register failed") {
		t.Fatalf("Register error: got %v", err)
	}
}

// TestClientResume_ServerError verifies that a server error from Resume is
// propagated to the caller.
func TestClientResume_ServerError(t *testing.T) {
	f := newFakeServer()
	f.resumeErr = errors.New("resume failed")
	url := startFakeServer(t, f)

	c, err := NewClient(url, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer c.Close()

	if err := c.Register(ctx, "n", "h", "v"); err != nil {
		t.Fatal(err)
	}
	_, err = c.Resume(ctx, "run-1", "received", map[string]string{"outcome": "ok"})
	if err == nil {
		t.Fatal("expected error from Resume")
	}
	if !strings.Contains(err.Error(), "resume failed") {
		t.Fatalf("Resume error: got %v", err)
	}
}
