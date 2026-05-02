package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/goleak"

	"github.com/brokenbots/criteria/internal/cli/applytest"
	"github.com/brokenbots/criteria/internal/engine"
	servertrans "github.com/brokenbots/criteria/internal/transport/server"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

// requireNoGoroutineLeak registers a t.Cleanup that calls goleak.VerifyNone(t)
// after all other cleanups for this test have run. Because t.Cleanup is LIFO,
// registering this first ensures it runs last — after the fake server and
// transport client have been closed — so HTTP/2 connection goroutines are gone
// by the time the leak assertion fires.
//
// Call this as the very first statement of any test that creates an engine or
// fake-server instance, before any t.TempDir or applytest.New calls.
func requireNoGoroutineLeak(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { goleak.VerifyNone(t) })
}

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
// runApplyServer against an in-memory fake server. It verifies that client
// submissions arrive in order and that the terminal RunCompleted event follows
// all step events. Server-mode apply routes directly to runApplyServer and does
// not write a local events file (eventsPath is not set and is not used in this
// path).
func TestRunApplyServer_HappyPath(t *testing.T) {
	requireNoGoroutineLeak(t)
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

	evts := fake.Events()

	// findFirst returns the index of the first event satisfying match, or -1.
	findFirst := func(match func(*pb.Envelope) bool) int {
		for i, e := range evts {
			if match(e) {
				return i
			}
		}
		return -1
	}

	iStep1 := findFirst(func(e *pb.Envelope) bool {
		se := e.GetStepEntered()
		return se != nil && se.Step == "step_one"
	})
	iStep2 := findFirst(func(e *pb.Envelope) bool {
		se := e.GetStepEntered()
		return se != nil && se.Step == "step_two"
	})
	iDone := findFirst(func(e *pb.Envelope) bool { return e.GetRunCompleted() != nil })

	if iStep1 == -1 {
		t.Fatal("expected StepEntered for step_one")
	}
	if iStep2 == -1 {
		t.Fatal("expected StepEntered for step_two")
	}
	if iDone == -1 {
		t.Fatal("expected RunCompleted event")
	}
	if iStep1 >= iStep2 {
		t.Errorf("step_one StepEntered (idx %d) not before step_two StepEntered (idx %d)", iStep1, iStep2)
	}
	if iStep2 >= iDone {
		t.Errorf("step_two StepEntered (idx %d) not before RunCompleted (idx %d)", iStep2, iDone)
	}
}

// TestExecuteServerRun_Cancellation verifies that a RunCancel message from the
// server terminates executeServerRun with context.Canceled, that the step
// checkpoint was written before the cancel propagated, and that the checkpoint
// is cleaned up on exit.
func TestExecuteServerRun_Cancellation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("cancelWorkflow uses the Unix sleep command")
	}
	requireNoGoroutineLeak(t)
	stateDir := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", stateDir)
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

	// Run executeServerRun in a goroutine so we can observe the checkpoint
	// written before the cancel propagates (the defer inside executeServerRun
	// removes it on return, so we must read it while the function is still
	// executing).
	runErr := make(chan error, 1)
	go func() { runErr <- executeServerRun(ctx, log, loader, client, state, graph, opts) }()

	// Poll the checkpoint file at 1ms intervals, capturing data the moment the
	// step_two checkpoint appears. The window between OnStepEntered writing it
	// and executeServerRun's deferred cleanup spans multiple goroutine switches
	// (loopback I/O, control channel hops, process kill), so 1ms polling
	// reliably captures it before deletion.
	cpPath := filepath.Join(stateDir, "runs", runID+".json")
	var cpData []byte
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, readErr := os.ReadFile(cpPath)
		if readErr == nil {
			var cp StepCheckpoint
			if json.Unmarshal(data, &cp) == nil && cp.CurrentStep == "step_two" {
				cpData = append([]byte{}, data...) // deep-copy before file may be removed
				break
			}
		}
		time.Sleep(1 * time.Millisecond)
	}
	if cpData == nil {
		t.Fatal("step_two checkpoint not observed within 5s")
	}
	var cp StepCheckpoint
	if err := json.Unmarshal(cpData, &cp); err != nil {
		t.Fatalf("decode checkpoint: %v", err)
	}
	if cp.CurrentStep != "step_two" {
		t.Errorf("checkpoint current_step: got %q, want %q", cp.CurrentStep, "step_two")
	}

	// Wait for executeServerRun to return.
	err = <-runErr
	if err == nil {
		t.Fatal("expected error from cancelled run")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}

	// The deferred RemoveStepCheckpoint inside executeServerRun must have run.
	if _, statErr := os.Stat(cpPath); !os.IsNotExist(statErr) {
		t.Errorf("expected checkpoint to be cleaned up after cancel; stat err: %v", statErr)
	}
}

// TestExecuteServerRun_TimeoutPropagation verifies that context.DeadlineExceeded
// propagates correctly when the run context expires while drainResumeCycles is
// waiting for a ResumeRun signal that the fake server never sends.
func TestExecuteServerRun_TimeoutPropagation(t *testing.T) {
	requireNoGoroutineLeak(t)
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())
	// InjectPauseAt triggers the wait hook; NeverResume prevents the fake from
	// sending a ResumeRun, so drainResumeCycles stalls until ctx.Done() fires.
	fake := applytest.New(t)
	fake.Execution = applytest.ApplyExecution{
		InjectPauseAt: "gate",
		NeverResume:   true,
	}

	bgCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	log := newApplyLogger()
	wfPath := writeWorkflowFile(t, pauseResumeWorkflow)
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
	timeoutCtx, timeoutCancel := context.WithTimeout(bgCtx, 500*time.Millisecond)
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
// with TLSMode=disable and a UUID v4 run ID.
func TestSetupServerRun_TLSDisable(t *testing.T) {
	requireNoGoroutineLeak(t)
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
	id, parseErr := uuid.Parse(runID)
	if parseErr != nil {
		t.Fatalf("run ID %q is not a valid UUID: %v", runID, parseErr)
	}
	if id.Version() != 4 {
		t.Errorf("run ID %q: expected UUID v4, got version %d", runID, id.Version())
	}
}

// TestSetupServerRun_TLSEnable verifies that setupServerRun connects over TLS
// when the server presents a certificate trusted via the configured CA file.
func TestSetupServerRun_TLSEnable(t *testing.T) {
	requireNoGoroutineLeak(t)
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())
	fake := applytest.NewTLS(t)

	// Write the fake's CA certificate to a temp file so the client can trust it.
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, fake.CACertPEM(), 0o600); err != nil {
		t.Fatalf("write CA file: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	log := newApplyLogger()
	wfPath := writeWorkflowFile(t, twoStepWorkflow)
	src, graph, loader, err := compileForExecution(ctx, wfPath, log)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	defer func() { _ = loader.Shutdown(context.WithoutCancel(ctx)) }()

	copts := servertrans.Options{TLSMode: servertrans.TLSEnable, CAFile: caFile}
	client, runID, err := setupServerRun(ctx, log, graph, src, fake.URL(), "test", &copts, cancel)
	if err != nil {
		t.Fatalf("setupServerRun with TLS: %v", err)
	}
	defer client.Close()

	if client.TLSMode() != servertrans.TLSEnable {
		t.Errorf("expected TLSEnable, got %q", client.TLSMode())
	}
	id, parseErr := uuid.Parse(runID)
	if parseErr != nil {
		t.Fatalf("run ID %q is not a valid UUID: %v", runID, parseErr)
	}
	if id.Version() != 4 {
		t.Errorf("run ID %q: expected UUID v4, got version %d", runID, id.Version())
	}
}

// TestSetupServerRun_MTLS verifies that setupServerRun connects over mutual TLS
// when both the server CA and the client certificate are configured correctly.
func TestSetupServerRun_MTLS(t *testing.T) {
	requireNoGoroutineLeak(t)
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())
	fake := applytest.NewMTLS(t)

	tmpDir := t.TempDir()
	caFile := filepath.Join(tmpDir, "ca.pem")
	certFile := filepath.Join(tmpDir, "client.pem")
	keyFile := filepath.Join(tmpDir, "client.key")
	if err := os.WriteFile(caFile, fake.CACertPEM(), 0o600); err != nil {
		t.Fatalf("write CA file: %v", err)
	}
	if err := os.WriteFile(certFile, fake.ClientCertPEM(), 0o600); err != nil {
		t.Fatalf("write cert file: %v", err)
	}
	if err := os.WriteFile(keyFile, fake.ClientKeyPEM(), 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	log := newApplyLogger()
	wfPath := writeWorkflowFile(t, twoStepWorkflow)
	src, graph, loader, err := compileForExecution(ctx, wfPath, log)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	defer func() { _ = loader.Shutdown(context.WithoutCancel(ctx)) }()

	copts := servertrans.Options{
		TLSMode:  servertrans.TLSMutual,
		CAFile:   caFile,
		CertFile: certFile,
		KeyFile:  keyFile,
	}
	client, runID, err := setupServerRun(ctx, log, graph, src, fake.URL(), "test", &copts, cancel)
	if err != nil {
		t.Fatalf("setupServerRun with mTLS: %v", err)
	}
	defer client.Close()

	if client.TLSMode() != servertrans.TLSMutual {
		t.Errorf("expected TLSMutual, got %q", client.TLSMode())
	}
	id, parseErr := uuid.Parse(runID)
	if parseErr != nil {
		t.Fatalf("run ID %q is not a valid UUID: %v", runID, parseErr)
	}
	if id.Version() != 4 {
		t.Errorf("run ID %q: expected UUID v4, got version %d", runID, id.Version())
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

// TestDrainResumeCycles_PauseThenResume verifies drainResumeCycles directly:
// the first engine run pauses at the wait node, the checkpoint is asserted,
// then drainResumeCycles receives the resume signal and completes the run.
func TestDrainResumeCycles_PauseThenResume(t *testing.T) {
	requireNoGoroutineLeak(t)
	stateDir := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", stateDir)
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

	// Build the sink and engine exactly as executeServerRun would, but without
	// the deferred checkpoint cleanup so we can assert its state between cycles.
	var eng *engine.Engine
	sink := buildServerSink(ctx, client, runID, graph, wfPath, fake.URL(), log,
		func() map[string]int {
			if eng != nil {
				return eng.VisitCounts()
			}
			return nil
		})

	eng = engine.New(graph, loader, sink, engine.WithWorkflowDir(filepath.Dir(wfPath)))
	if err := eng.Run(ctx); err != nil {
		t.Fatalf("first engine run: %v", err)
	}
	if !sink.IsPaused() {
		t.Fatal("expected engine to be paused at the gate wait node")
	}

	// Verify the checkpoint was written for the last step before the wait node.
	cpPath := filepath.Join(stateDir, "runs", runID+".json")
	cpData, readErr := os.ReadFile(cpPath)
	if readErr != nil {
		t.Fatalf("checkpoint not written before pause: %v", readErr)
	}
	var cpBefore StepCheckpoint
	if err := json.Unmarshal(cpData, &cpBefore); err != nil {
		t.Fatalf("decode checkpoint: %v", err)
	}
	if cpBefore.CurrentStep != "step_one" {
		t.Errorf("pre-resume checkpoint current_step: got %q, want %q", cpBefore.CurrentStep, "step_one")
	}

	// Call drainResumeCycles directly — it blocks until the fake sends ResumeRun.
	if err := drainResumeCycles(ctx, log, loader, sink, client, state, graph, opts, eng); err != nil {
		t.Fatalf("drainResumeCycles: %v", err)
	}
	// Flush queued events to the fake server before asserting receipt.
	drainCtx, drainCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	client.Drain(drainCtx)
	drainCancel()

	if !fake.HasStepEntered("step_three") {
		t.Error("expected StepEntered for step_three after resume")
	}
	if !fake.HasEventOfType("WaitResumed") {
		t.Error("expected WaitResumed event after resume")
	}
	if !fake.HasEventOfType("RunCompleted") {
		t.Error("expected RunCompleted after full resume cycle")
	}

	// After drainResumeCycles the checkpoint reflects the post-resume step.
	cpData, readErr = os.ReadFile(cpPath)
	if readErr != nil {
		t.Fatalf("post-resume checkpoint not written: %v", readErr)
	}
	var cpAfter StepCheckpoint
	if err := json.Unmarshal(cpData, &cpAfter); err != nil {
		t.Fatalf("decode post-resume checkpoint: %v", err)
	}
	if cpAfter.CurrentStep != "step_three" {
		t.Errorf("post-resume checkpoint current_step: got %q, want %q", cpAfter.CurrentStep, "step_three")
	}
}

// TestDrainResumeCycles_StreamDropAndReconnect verifies that a stream drop
// during a resumed run is handled transparently by calling drainResumeCycles
// directly: the client reconnects, replays from since_seq, and the run
// completes.
func TestDrainResumeCycles_StreamDropAndReconnect(t *testing.T) {
	requireNoGoroutineLeak(t)
	stateDir := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", stateDir)
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

	// Run the engine to the pause point, then call drainResumeCycles directly.
	var eng *engine.Engine
	sink := buildServerSink(ctx, client, runID, graph, wfPath, fake.URL(), log,
		func() map[string]int {
			if eng != nil {
				return eng.VisitCounts()
			}
			return nil
		})

	eng = engine.New(graph, loader, sink, engine.WithWorkflowDir(filepath.Dir(wfPath)))
	if err := eng.Run(ctx); err != nil {
		t.Fatalf("first engine run: %v", err)
	}
	if !sink.IsPaused() {
		t.Fatal("expected engine to be paused at gate wait node")
	}

	if err := drainResumeCycles(ctx, log, loader, sink, client, state, graph, opts, eng); err != nil {
		t.Fatalf("drainResumeCycles: %v", err)
	}
	// Flush queued events to the fake server before asserting receipt.
	drainCtx, drainCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	client.Drain(drainCtx)
	drainCancel()

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
