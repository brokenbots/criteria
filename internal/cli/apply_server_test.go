package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/brokenbots/criteria/internal/cli/applytest"
	servertrans "github.com/brokenbots/criteria/internal/transport/server"
)

// twoStepWorkflow is a minimal two-step shell workflow used by happy-path tests.
const twoStepWorkflow = `
workflow "two_step" {
  version       = "0.1"
  initial_state = "step_one"
  target_state  = "done"

  step "step_one" {
    adapter = "shell"
    input { command = "echo step_one" }
    outcome "success" { transition_to = "step_two" }
    outcome "failure" { transition_to = "done" }
  }

  step "step_two" {
    adapter = "shell"
    input { command = "echo step_two" }
    outcome "success" { transition_to = "done" }
    outcome "failure" { transition_to = "done" }
  }

  state "done" {
    terminal = true
    success  = true
  }
}
`

// cancelWorkflow has a slow step_two so a RunCancel can arrive before it completes.
// step_two intentionally has no "failure" outcome so context.Canceled propagates
// as an error instead of being silently routed through the failure transition.
const cancelWorkflow = `
workflow "cancel_test" {
  version       = "0.1"
  initial_state = "step_one"
  target_state  = "done"

  step "step_one" {
    adapter = "shell"
    input { command = "echo step_one" }
    outcome "success" { transition_to = "step_two" }
    outcome "failure" { transition_to = "done" }
  }

  step "step_two" {
    adapter = "shell"
    input { command = "sleep 30" }
    outcome "success" { transition_to = "done" }
  }

  state "done" {
    terminal = true
    success  = true
  }
}
`

// pauseResumeWorkflow has a wait/signal node between step_one and step_three.
const pauseResumeWorkflow = `
workflow "pause_resume" {
  version       = "0.1"
  initial_state = "step_one"
  target_state  = "done"

  step "step_one" {
    adapter = "shell"
    input { command = "echo step_one" }
    outcome "success" { transition_to = "gate" }
    outcome "failure" { transition_to = "done" }
  }

  wait "gate" {
    signal = "resume"
    outcome "received" { transition_to = "step_three" }
  }

  step "step_three" {
    adapter = "shell"
    input { command = "echo step_three" }
    outcome "success" { transition_to = "done" }
    outcome "failure" { transition_to = "done" }
  }

  state "done" {
    terminal = true
    success  = true
  }
}
`

// TestRunApplyServer_HappyPath exercises the full server-mode apply path through
// runApplyServer against an in-memory fake server.
func TestRunApplyServer_HappyPath(t *testing.T) {
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())
	fake := applytest.New(t)

	wfPath := writeWorkflowFile(t, twoStepWorkflow)
	opts := applyOptions{
		workflowPath: wfPath,
		serverURL:    fake.URL(),
		name:         "test-agent",
	}
	if err := runApplyServer(context.Background(), opts); err != nil {
		t.Fatalf("runApplyServer: %v", err)
	}

	if !fake.HasStepEntered("step_one") {
		t.Error("expected StepEntered for step_one")
	}
	if !fake.HasStepEntered("step_two") {
		t.Error("expected StepEntered for step_two")
	}
	if !fake.HasEventOfType("RunCompleted") {
		t.Error("expected RunCompleted event")
	}
}

// TestExecuteServerRun_Cancellation verifies that a RunCancel message from the
// server terminates executeServerRun with context.Canceled.
func TestExecuteServerRun_Cancellation(t *testing.T) {
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())
	fake := applytest.New(t)
	fake.Execution = applytest.ApplyExecution{CancelAt: "step_two"}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	log := newApplyLogger()
	wfPath := writeWorkflowFile(t, cancelWorkflow)
	src, graph, loader, err := compileForExecution(ctx, wfPath, log)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	defer func() { _ = loader.Shutdown(context.WithoutCancel(ctx)) }()

	copts := servertrans.Options{TLSMode: servertrans.TLSDisable}
	client, runID, err := setupServerRun(ctx, log, graph, src, fake.URL(), "test", &copts, cancel)
	if err != nil {
		t.Fatalf("setupServerRun: %v", err)
	}
	defer client.Close()

	state := newLocalRunState(runID, graph.Name, fake.URL())
	opts := applyOptions{workflowPath: wfPath, serverURL: fake.URL()}
	err = executeServerRun(ctx, log, loader, client, state, graph, opts)
	if err == nil {
		t.Fatal("expected error from cancelled run")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if !fake.HasStepEntered("step_two") {
		t.Error("expected fake to have received StepEntered for step_two before cancel")
	}
}

// TestExecuteServerRun_TimeoutPropagation verifies that context.DeadlineExceeded
// propagates correctly when the run context expires mid-step.
func TestExecuteServerRun_TimeoutPropagation(t *testing.T) {
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())
	fake := applytest.New(t)

	bgCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	log := newApplyLogger()
	wfPath := writeWorkflowFile(t, `
workflow "timeout_test" {
  version       = "0.1"
  initial_state = "step_one"
  target_state  = "done"

  step "step_one" {
    adapter = "shell"
    input { command = "sleep 1" }
    outcome "success" { transition_to = "done" }
  }

  state "done" {
    terminal = true
    success  = true
  }
}
`)
	src, graph, loader, err := compileForExecution(bgCtx, wfPath, log)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	defer func() { _ = loader.Shutdown(context.WithoutCancel(bgCtx)) }()

	copts := servertrans.Options{TLSMode: servertrans.TLSDisable}
	client, runID, err := setupServerRun(bgCtx, log, graph, src, fake.URL(), "test", &copts, cancel)
	if err != nil {
		t.Fatalf("setupServerRun: %v", err)
	}
	defer client.Close()

	// Only executeServerRun uses the short-lived timeout context.
	timeoutCtx, timeoutCancel := context.WithTimeout(bgCtx, 200*time.Millisecond)
	defer timeoutCancel()

	state := newLocalRunState(runID, graph.Name, fake.URL())
	opts := applyOptions{workflowPath: wfPath, serverURL: fake.URL()}
	err = executeServerRun(timeoutCtx, log, loader, client, state, graph, opts)
	if err == nil {
		t.Fatal("expected error from timed-out run")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded, got %v", err)
	}
}

// TestSetupServerRun_TLSDisable verifies that setupServerRun returns a client
// with TLSMode=disable and a non-empty run ID.
func TestSetupServerRun_TLSDisable(t *testing.T) {
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())
	fake := applytest.New(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	log := newApplyLogger()
	wfPath := writeWorkflowFile(t, twoStepWorkflow)
	src, graph, loader, err := compileForExecution(ctx, wfPath, log)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	defer func() { _ = loader.Shutdown(context.WithoutCancel(ctx)) }()

	copts := servertrans.Options{TLSMode: servertrans.TLSDisable}
	client, runID, err := setupServerRun(ctx, log, graph, src, fake.URL(), "test", &copts, cancel)
	if err != nil {
		t.Fatalf("setupServerRun: %v", err)
	}
	defer client.Close()

	if client.TLSMode() != servertrans.TLSDisable {
		t.Errorf("expected TLSDisable, got %q", client.TLSMode())
	}
	if runID == "" {
		t.Error("expected non-empty run ID")
	}
}

// TestSetupServerRun_MTLSMissingCert verifies that setupServerRun returns an
// error with the expected message when mTLS is configured without certificates.
func TestSetupServerRun_MTLSMissingCert(t *testing.T) {
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())

	log := newApplyLogger()
	copts := servertrans.Options{TLSMode: servertrans.TLSMutual}
	_, _, err := setupServerRun(context.Background(), log, nil, nil, "http://localhost:9999", "test", &copts, nil)
	if err == nil {
		t.Fatal("expected error for mtls without cert")
	}
	if !strings.Contains(err.Error(), "mtls requires --tls-cert and --tls-key") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestDrainResumeCycles_PauseThenResume verifies that drainResumeCycles (via
// executeServerRun) correctly handles a pause/resume cycle: the engine pauses
// at a wait node, the fake sends ResumeRun, and the run completes.
func TestDrainResumeCycles_PauseThenResume(t *testing.T) {
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())
	fake := applytest.New(t)
	fake.Execution = applytest.ApplyExecution{
		InjectPauseAt: "gate",
		ResumeAfter:   100 * time.Millisecond,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	log := newApplyLogger()
	wfPath := writeWorkflowFile(t, pauseResumeWorkflow)
	src, graph, loader, err := compileForExecution(ctx, wfPath, log)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	defer func() { _ = loader.Shutdown(context.WithoutCancel(ctx)) }()

	copts := servertrans.Options{TLSMode: servertrans.TLSDisable}
	client, runID, err := setupServerRun(ctx, log, graph, src, fake.URL(), "test", &copts, cancel)
	if err != nil {
		t.Fatalf("setupServerRun: %v", err)
	}
	defer client.Close()

	state := newLocalRunState(runID, graph.Name, fake.URL())
	opts := applyOptions{workflowPath: wfPath, serverURL: fake.URL()}
	if err := executeServerRun(ctx, log, loader, client, state, graph, opts); err != nil {
		t.Fatalf("executeServerRun: %v", err)
	}

	if !fake.HasEventOfType("WaitEntered") {
		t.Error("expected WaitEntered event for gate")
	}
	if !fake.HasEventOfType("WaitResumed") {
		t.Error("expected WaitResumed event after resume")
	}
	if !fake.HasStepEntered("step_three") {
		t.Error("expected StepEntered for step_three after resume")
	}
	if !fake.HasEventOfType("RunCompleted") {
		t.Error("expected RunCompleted after full run")
	}
}

// TestDrainResumeCycles_StreamDropAndReconnect verifies that a stream drop
// during a resumed run is handled transparently: the client reconnects, replays
// from since_seq, and the run completes.
func TestDrainResumeCycles_StreamDropAndReconnect(t *testing.T) {
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())
	fake := applytest.New(t)
	fake.Execution = applytest.ApplyExecution{
		InjectPauseAt: "gate",
		ResumeAfter:   50 * time.Millisecond,
		DropStreamAt:  "step_three", // drop events stream when step_three starts
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	log := newApplyLogger()
	wfPath := writeWorkflowFile(t, pauseResumeWorkflow)
	src, graph, loader, err := compileForExecution(ctx, wfPath, log)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	defer func() { _ = loader.Shutdown(context.WithoutCancel(ctx)) }()

	copts := servertrans.Options{TLSMode: servertrans.TLSDisable}
	client, runID, err := setupServerRun(ctx, log, graph, src, fake.URL(), "test", &copts, cancel)
	if err != nil {
		t.Fatalf("setupServerRun: %v", err)
	}
	defer client.Close()

	state := newLocalRunState(runID, graph.Name, fake.URL())
	opts := applyOptions{workflowPath: wfPath, serverURL: fake.URL()}
	if err := executeServerRun(ctx, log, loader, client, state, graph, opts); err != nil {
		t.Fatalf("executeServerRun: %v", err)
	}

	if !fake.HasEventOfType("RunCompleted") {
		t.Error("expected RunCompleted after reconnect and full run")
	}
	if !fake.HasStepEntered("step_three") {
		t.Error("expected StepEntered for step_three")
	}

	// Verify the reconnect sent a since_seq header.
	hdrs := fake.SinceSeqHeaders()
	hasSince := false
	for _, h := range hdrs {
		if h != "" {
			hasSince = true
			break
		}
	}
	if !hasSince {
		t.Errorf("expected at least one reconnect with non-empty since_seq, got headers: %v", hdrs)
	}
}
