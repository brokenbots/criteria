package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/goleak"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/internal/cli/applytest"
	"github.com/brokenbots/criteria/internal/engine"
	"github.com/brokenbots/criteria/internal/run"
	servertrans "github.com/brokenbots/criteria/internal/transport/server"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
	"github.com/brokenbots/criteria/workflow"
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
workflow {
  name = "two_step"
  version       = "0.1"
  initial_state = "step_one"
  target_state  = "done"
}

adapter "noop" "default" {}

step "step_one" {
  target = adapter.noop.default
  outcome "success" { next = step.step_two }
  outcome "failure" { next = step.done }
}

step "step_two" {
  target = adapter.noop.default
  outcome "success" { next = step.done }
  outcome "failure" { next = step.done }
}

state "done" {
  terminal = true
  success  = true
}
`

// cancelWorkflow has a slow step_two (noop with a long delay) so a RunCancel can
// arrive before it completes. step_two intentionally has no "failure" outcome so
// context.Canceled propagates as an error instead of being silently routed
// through the failure transition.
const cancelWorkflow = `
workflow {
  name = "cancel_test"
  version       = "0.1"
  initial_state = "step_one"
  target_state  = "done"
}

adapter "noop" "default" {}

step "step_one" {
  target = adapter.noop.default
  outcome "success" { next = step.step_two }
  outcome "failure" { next = step.done }
}

step "step_two" {
  target = adapter.noop.default
  input { delay_ms = "30000" }
  outcome "success" { next = step.done }
}

state "done" {
  terminal = true
  success  = true
}
`

// pauseResumeWorkflow has a wait/signal node between step_one and step_three.
const pauseResumeWorkflow = `
workflow {
  name = "pause_resume"
  version       = "0.1"
  initial_state = "step_one"
  target_state  = "done"
}

adapter "noop" "default" {}

step "step_one" {
  target = adapter.noop.default
  outcome "success" { next = step.gate }
  outcome "failure" { next = step.done }
}

wait "gate" {
  signal = "resume"
  outcome "received" { next = step.step_three }
}

step "step_three" {
  target = adapter.noop.default
  outcome "success" { next = step.done }
  outcome "failure" { next = step.done }
}

state "done" {
  terminal = true
  success  = true
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

// TestRunApplyServer_TerminalFailure_ExitsNonZero verifies that a server-mode
// run ending in a terminal state with success=false causes runApplyServer to
// return a non-nil error, which propagates to a non-zero CLI exit code.
func TestRunApplyServer_TerminalFailure_ExitsNonZero(t *testing.T) {
	requireNoGoroutineLeak(t)
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())
	fake := applytest.New(t)

	wfPath := writeWorkflowFile(t, `
workflow {
  name = "server_terminal_failure"
  version       = "0.1"
  initial_state = "failed"
  target_state  = "failed"
}

state "failed" {
  terminal = true
  success  = false
}
`)
	opts := applyOptions{
		workflowPath: wfPath,
		serverURL:    fake.URL(),
		name:         "test-agent",
	}
	err := runApplyServer(context.Background(), opts)
	if err == nil {
		t.Fatal("expected non-nil error for terminal failed server run")
	}
	if !strings.Contains(err.Error(), "success=false") {
		t.Fatalf("error should report success=false, got: %v", err)
	}

	evts := fake.Events()
	var completed *pb.RunCompleted
	for _, e := range evts {
		if rc := e.GetRunCompleted(); rc != nil {
			completed = rc
			break
		}
	}
	if completed == nil {
		t.Fatal("expected RunCompleted event")
	}
	if completed.GetFinalState() != "failed" {
		t.Fatalf("RunCompleted.finalState = %q, want failed", completed.GetFinalState())
	}
	if completed.GetSuccess() {
		t.Fatal("RunCompleted.success = true, want false")
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
	fake.Execution = applytest.ApplyExecution{CancelAt: "step_two", CancelAfter: 500 * time.Millisecond}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	log := newApplyLogger()
	wfPath := writeWorkflowFile(t, cancelWorkflow)
	src, graph, loader, err := compileForExecution(ctx, wfPath, log, false, false)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	defer func() { _ = loader.Shutdown(context.WithoutCancel(ctx)) }()

	copts := servertrans.Options{TLSMode: servertrans.TLSDisable}
	client, runID, resumed, _, err := setupServerRun(ctx, log, graph, src, fake.URL(), "test", &copts, cancel, nil, "")
	if err != nil {
		t.Fatalf("setupServerRun: %v", err)
	}
	if resumed {
		t.Fatal("setupServerRun unexpectedly resumed a matching checkpoint")
	}
	defer client.Close()

	state := newLocalRunState(runID, graph.Name, fake.URL())
	opts := applyOptions{workflowPath: wfPath, serverURL: fake.URL()}

	// Run executeServerRun in a goroutine so we can observe the checkpoint
	// written before the cancel propagates (the defer inside executeServerRun
	// removes it on return, so we must read it while the function is still
	// executing).
	runErr := make(chan error, 1)
	go func() { runErr <- executeServerRun(ctx, log, loader, client, state, graph, opts, nil) }()

	// CancelAfter delays the fake's RunCancel by 500ms, so the step_two
	// checkpoint (written on OnStepEntered before the adapter's 30s sleep is
	// killed) stays on disk for at least that long. Polling at 20ms then
	// observes it deterministically; a tight 1ms poll raced with the
	// sub-millisecond write/remove window and failed intermittently.
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
		time.Sleep(20 * time.Millisecond)
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
	src, graph, loader, err := compileForExecution(bgCtx, wfPath, log, false, false)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	defer func() { _ = loader.Shutdown(context.WithoutCancel(bgCtx)) }()

	copts := servertrans.Options{TLSMode: servertrans.TLSDisable}
	client, runID, resumed, _, err := setupServerRun(bgCtx, log, graph, src, fake.URL(), "test", &copts, cancel, nil, "")
	if err != nil {
		t.Fatalf("setupServerRun: %v", err)
	}
	if resumed {
		t.Fatal("setupServerRun unexpectedly resumed a matching checkpoint")
	}
	defer client.Close()

	// Only executeServerRun uses the short-lived timeout context.
	timeoutCtx, timeoutCancel := context.WithTimeout(bgCtx, 500*time.Millisecond)
	defer timeoutCancel()

	state := newLocalRunState(runID, graph.Name, fake.URL())
	opts := applyOptions{workflowPath: wfPath, serverURL: fake.URL()}
	err = executeServerRun(timeoutCtx, log, loader, client, state, graph, opts, nil)
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
	src, graph, loader, err := compileForExecution(ctx, wfPath, log, false, false)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	defer func() { _ = loader.Shutdown(context.WithoutCancel(ctx)) }()

	copts := servertrans.Options{TLSMode: servertrans.TLSDisable}
	client, runID, resumed, _, err := setupServerRun(ctx, log, graph, src, fake.URL(), "test", &copts, cancel, nil, "")
	if err != nil {
		t.Fatalf("setupServerRun: %v", err)
	}
	if resumed {
		t.Fatal("setupServerRun unexpectedly resumed a matching checkpoint")
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
	src, graph, loader, err := compileForExecution(ctx, wfPath, log, false, false)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	defer func() { _ = loader.Shutdown(context.WithoutCancel(ctx)) }()

	copts := servertrans.Options{TLSMode: servertrans.TLSEnable, CAFile: caFile}
	client, runID, resumed, _, err := setupServerRun(ctx, log, graph, src, fake.URL(), "test", &copts, cancel, nil, "")
	if err != nil {
		t.Fatalf("setupServerRun with TLS: %v", err)
	}
	if resumed {
		t.Fatal("setupServerRun unexpectedly resumed a matching checkpoint")
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
	src, graph, loader, err := compileForExecution(ctx, wfPath, log, false, false)
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
	client, runID, resumed, _, err := setupServerRun(ctx, log, graph, src, fake.URL(), "test", &copts, cancel, nil, "")
	if err != nil {
		t.Fatalf("setupServerRun with mTLS: %v", err)
	}
	if resumed {
		t.Fatal("setupServerRun unexpectedly resumed a matching checkpoint")
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
	_, _, _, _, err := setupServerRun(context.Background(), log, nil, nil, "https://localhost:9999", "test", &copts, nil, nil, "")
	if err == nil {
		t.Fatal("expected error for mtls without cert")
	}
	if !strings.Contains(err.Error(), "mtls requires --tls-cert and --tls-key") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestSetupServerRun_MTLSRejectsCACert proves that the rejectCACertClient
// VerifyPeerCertificate hook is wired correctly: using the CA certificate and
// key (instead of the generated leaf client cert) must be rejected by the
// mTLS fake server. This is the regression test for the fix that prevents the
// CA cert from accidentally authenticating as a client credential.
func TestSetupServerRun_MTLSRejectsCACert(t *testing.T) {
	requireNoGoroutineLeak(t)
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())
	fake := applytest.NewMTLS(t)

	tmpDir := t.TempDir()
	caFile := filepath.Join(tmpDir, "ca.pem")
	// Use the CA cert+key as client credentials — the server must reject this.
	certFile := filepath.Join(tmpDir, "client.pem")
	keyFile := filepath.Join(tmpDir, "client.key")
	if err := os.WriteFile(caFile, fake.CACertPEM(), 0o600); err != nil {
		t.Fatalf("write CA file: %v", err)
	}
	if err := os.WriteFile(certFile, fake.CACertPEM(), 0o600); err != nil {
		t.Fatalf("write cert file: %v", err)
	}
	if err := os.WriteFile(keyFile, fake.CAKeyPEM(), 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	log := newApplyLogger()
	wfPath := writeWorkflowFile(t, twoStepWorkflow)
	src, graph, loader, err := compileForExecution(ctx, wfPath, log, false, false)
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
	_, _, _, _, err = setupServerRun(ctx, log, graph, src, fake.URL(), "test", &copts, cancel, nil, "")
	if err == nil {
		t.Fatal("expected setupServerRun to fail: CA cert must be rejected as a client credential")
	}
	// err != nil is sufficient: the fake server is healthy and the only variable
	// is the client credentials. Under -race load the mTLS rejection surfaces at
	// different layers ("bad certificate", "broken pipe", "connection reset",
	// "client conn could not be established", etc.) so no string match is stable.
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
	src, graph, loader, err := compileForExecution(ctx, wfPath, log, false, false)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	defer func() { _ = loader.Shutdown(context.WithoutCancel(ctx)) }()

	copts := servertrans.Options{TLSMode: servertrans.TLSDisable}
	client, runID, resumed, _, err := setupServerRun(ctx, log, graph, src, fake.URL(), "test", &copts, cancel, nil, "")
	if err != nil {
		t.Fatalf("setupServerRun: %v", err)
	}
	if resumed {
		t.Fatal("setupServerRun unexpectedly resumed a matching checkpoint")
	}
	defer client.Close()

	state := newLocalRunState(runID, graph.Name, fake.URL())

	// CRI-202: the engine loader is an in-process stateful handle so the
	// checkpoint surface wired by buildServerRunEngine/drainResumeCycles is
	// observable: adapter state persists as snapshot files at each step
	// boundary and the resumed engine replays it through the handle's Restore.
	handle := &statefulHandle{name: "noop"}
	ckLoader := &staticHandleLoader{handle: handle}

	// Build the sink and engine exactly as executeServerRun would, but without
	// the deferred checkpoint cleanup so we can assert its state between cycles.
	var eng *engine.Engine
	sink := buildServerSink(ctx, client, client, runID, graph, wfPath, fake.URL(), "", log,
		func() map[string]int {
			if eng != nil {
				return eng.VisitCounts()
			}
			return nil
		})

	eng, err = buildServerRunEngine(graph, ckLoader, sink, state, applyOptions{workflowPath: wfPath}, "", nil, "")
	if err != nil {
		t.Fatalf("buildServerRunEngine: %v", err)
	}
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

	// CRI-202: the first engine's checkpoint surface must have persisted the
	// step_one boundary adapter state before the pause.
	snapDir := filepath.Join(stateDir, "runs", runID, "snapshots", "noop.default")
	prePairs := snapshotPairs(t, snapDir)
	if len(prePairs) != 1 {
		t.Fatalf("expected 1 pre-pause snapshot pair under %s, got %d", snapDir, len(prePairs))
	}
	if prePairs[0] != "0000000001" {
		t.Errorf("first snapshot seq = %q, want 0000000001", prePairs[0])
	}
	if len(handle.restores()) != 0 {
		t.Errorf("expected no Restore replay before resume, got %d", len(handle.restores()))
	}

	// Read the pre-pause blob now, while the paused (non-terminal) run still
	// owns its snapshots: a completed run's retention janitor deletes them, so
	// later assertions compare against this captured value.
	preBlob, err := os.ReadFile(filepath.Join(snapDir, prePairs[0]+".bin"))
	if err != nil {
		t.Fatalf("read pre-pause snapshot: %v", err)
	}

	// Capture the resumed run's snapshot surface at each step boundary. The
	// resumed run completes, and CRI-202 retention deletes a completed run's
	// snapshots, so post-run file reads would observe an empty directory.
	var resumedCaptures []snapshotCapture
	resumedSink := &stepHookSink{Sink: sink, onStepOutcome: func(step string) {
		c := captureLatestSnapshot(t, snapDir)
		c.step = step
		resumedCaptures = append(resumedCaptures, c)
	}}

	// Call drainResumeCycles directly — it blocks until the fake sends ResumeRun.
	// Pass the hook-wrapped sink as the runSink so the resumed engine's
	// checkpoint surface is observable mid-run; the control-plane sink stays
	// unwrapped for IsPaused/PausedAt. The engine loader is the in-process
	// stateful loader so the resumed engine replays state through the handle.
	if err := drainResumeCycles(ctx, log, ckLoader, sink, resumedSink, client.ResumeCh(), nil, "", state, graph, filepath.Dir(wfPath), eng, ""); err != nil {
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

	// CRI-202: the resumed engine must have replayed the pre-pause adapter
	// state (one Restore carrying the pre-pause blob) and persisted new state
	// for the post-resume step_three boundary. A missing snapshot base or run
	// id in drainResumeCycles' resumed engine leaves both assertions failing.
	restores := handle.restores()
	if len(restores) != 1 {
		t.Fatalf("expected 1 Restore replay in the resumed engine, got %d", len(restores))
	}
	if !bytes.Equal(restores[0].blob, preBlob) {
		t.Errorf("restored blob = %q, want the pre-pause state %q", restores[0].blob, preBlob)
	}
	if restores[0].schemaVersion != 1 {
		t.Errorf("restored schema version = %d, want 1", restores[0].schemaVersion)
	}

	// The step_three boundary capture (taken while the resumed run was still
	// in flight, before the terminal retention janitor) must show the durable
	// post-resume state: a second snapshot pair carrying step_three's state
	// with the declared schema tag.
	var stepThree *snapshotCapture
	for i := range resumedCaptures {
		if resumedCaptures[i].step == "step_three" {
			stepThree = &resumedCaptures[i]
		}
	}
	if stepThree == nil {
		t.Fatalf("expected a step-boundary capture for step_three in the resumed run, got captures: %v", resumedCaptures)
	}
	if len(stepThree.pairs) != 2 {
		t.Fatalf("expected 2 snapshot pairs at the step_three boundary (step_one + step_three), got %d", len(stepThree.pairs))
	}
	if stepThree.pairs[1] != "0000000002" {
		t.Errorf("second snapshot seq = %q, want 0000000002", stepThree.pairs[1])
	}
	if string(stepThree.latestBlob) != "state-after-call-2" {
		t.Errorf("post-resume snapshot blob = %q, want %q (state persisted by the resumed step)", stepThree.latestBlob, "state-after-call-2")
	}
	if stepThree.stateSchema != "stateful.v1" {
		t.Errorf("post-resume snapshot state_schema = %q, want %q", stepThree.stateSchema, "stateful.v1")
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
	src, graph, loader, err := compileForExecution(ctx, wfPath, log, false, false)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	defer func() { _ = loader.Shutdown(context.WithoutCancel(ctx)) }()

	copts := servertrans.Options{TLSMode: servertrans.TLSDisable}
	client, runID, resumed, _, err := setupServerRun(ctx, log, graph, src, fake.URL(), "test", &copts, cancel, nil, "")
	if err != nil {
		t.Fatalf("setupServerRun: %v", err)
	}
	if resumed {
		t.Fatal("setupServerRun unexpectedly resumed a matching checkpoint")
	}
	defer client.Close()

	state := newLocalRunState(runID, graph.Name, fake.URL())

	// Run the engine to the pause point, then call drainResumeCycles directly.
	var eng *engine.Engine
	sink := buildServerSink(ctx, client, client, runID, graph, wfPath, fake.URL(), "", log,
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

	// Pass sink as the runSink because this test builds the server sink directly
	// rather than through executeServerRun.
	if err := drainResumeCycles(ctx, log, loader, sink, sink, client.ResumeCh(), nil, "", state, graph, filepath.Dir(wfPath), eng, ""); err != nil {
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

// restoreRecord captures one Restore RPC the in-process handle received.
type restoreRecord struct {
	blob          []byte
	schemaVersion uint32
}

// statefulHandle is an in-process adapter handle declaring blob/per-step
// checkpoint state (schema "stateful.v1"). It records Restore RPCs and mutates
// its state through Execute/Snapshot/Restore like a real adapter would, so
// CLI-level tests can observe that an engine was constructed with the CRI-202
// checkpoint surface: snapshots appear on disk and resumes replay state.
type statefulHandle struct {
	name string

	mu           sync.Mutex
	state        []byte
	restoreCalls []restoreRecord
	executeCalls int
}

func (h *statefulHandle) Info(context.Context) (adapterhost.Info, error) {
	return adapterhost.Info{
		Name: h.name,
		AdapterInfo: workflow.AdapterInfo{
			State: &workflow.StateDeclaration{
				Mode:        workflow.StateModeBlob,
				Schema:      "stateful.v1",
				Granularity: workflow.StateGranularityPerStep,
			},
		},
	}, nil
}

func (h *statefulHandle) OpenSession(context.Context, string, map[string]string, map[string]string) error {
	return nil
}

func (h *statefulHandle) Execute(context.Context, string, *workflow.StepNode, adapter.EventSink) (adapter.Result, error) {
	h.mu.Lock()
	h.executeCalls++
	n := h.executeCalls
	h.state = []byte(fmt.Sprintf("state-after-call-%d", n))
	h.mu.Unlock()
	return adapter.Result{Outcome: "success"}, nil
}

func (h *statefulHandle) Permit(context.Context, string, string, bool, string) error { return nil }
func (h *statefulHandle) CloseSession(context.Context, string) error                 { return nil }
func (h *statefulHandle) Kill()                                                      {}
func (h *statefulHandle) Pause(context.Context, string) error                        { return nil }
func (h *statefulHandle) Resume(context.Context, string) error                       { return nil }
func (h *statefulHandle) Inspect(context.Context, string) (*v2.InspectResponse, error) {
	return &v2.InspectResponse{}, nil
}

func (h *statefulHandle) Snapshot(context.Context, string) (*v2.SnapshotResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return &v2.SnapshotResponse{State: append([]byte(nil), h.state...)}, nil
}

func (h *statefulHandle) Restore(_ context.Context, _ string, blob []byte, schemaVersion uint32) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.restoreCalls = append(h.restoreCalls, restoreRecord{blob: append([]byte(nil), blob...), schemaVersion: schemaVersion})
	h.state = append([]byte(nil), blob...)
	return nil
}

func (h *statefulHandle) restores() []restoreRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]restoreRecord(nil), h.restoreCalls...)
}

// staticHandleLoader resolves every adapter to the same in-process handle.
type staticHandleLoader struct{ handle adapterhost.Handle }

func (l *staticHandleLoader) Resolve(context.Context, string) (adapterhost.Handle, error) {
	return l.handle, nil
}
func (l *staticHandleLoader) Shutdown(context.Context) error { return nil }

// snapshotPairs returns the sorted snapshot sequence numbers (.bin blobs) for
// one session's snapshot directory.
func snapshotPairs(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read snapshot dir %s: %v", dir, err)
	}
	var seqs []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".bin") {
			seqs = append(seqs, strings.TrimSuffix(e.Name(), ".bin"))
		}
	}
	sort.Strings(seqs)
	return seqs
}

// snapshotCapture records the snapshot surface for one session directory as
// observed at a single moment in time.
type snapshotCapture struct {
	step        string
	pairs       []string
	latestBlob  []byte
	stateSchema string
}

// captureLatestSnapshot reads the current snapshot surface: the sorted
// sequence numbers, the latest blob, and the latest sidecar's declared state
// schema. The terminal-run retention janitor deletes a completed run's
// snapshots (CRI-202), so on-disk assertions must be captured while the run is
// still in flight — the same pattern the engine tests use with a hook sink.
func captureLatestSnapshot(t *testing.T, dir string) snapshotCapture {
	t.Helper()
	c := snapshotCapture{pairs: snapshotPairs(t, dir)}
	if len(c.pairs) == 0 {
		return c
	}
	latest := c.pairs[len(c.pairs)-1]
	blob, err := os.ReadFile(filepath.Join(dir, latest+".bin"))
	if err != nil {
		t.Fatalf("read snapshot blob %s: %v", latest, err)
	}
	c.latestBlob = blob
	meta, err := os.ReadFile(filepath.Join(dir, latest+".json"))
	if err != nil {
		t.Fatalf("read snapshot sidecar %s: %v", latest, err)
	}
	var side struct {
		StateSchema string `json:"state_schema"`
	}
	if err := json.Unmarshal(meta, &side); err != nil {
		t.Fatalf("decode snapshot sidecar %s: %v", latest, err)
	}
	c.stateSchema = side.StateSchema
	return c
}

// stepHookSink observes OnStepOutcome events while delegating the rest of the
// sink surface to the wrapped *run.Sink, so tests can read the checkpoint
// surface at step boundaries while the run is still in flight.
type stepHookSink struct {
	*run.Sink
	onStepOutcome func(step string)
}

func (s *stepHookSink) OnStepOutcome(step, outcome string, duration time.Duration, err error) {
	if s.onStepOutcome != nil {
		s.onStepOutcome(step)
	}
	s.Sink.OnStepOutcome(step, outcome, duration, err)
}

// TestServerModeResume_CheckpointSurfaceWired is the CRI-202 regression guard
// for buildServerRunEngine: the fresh server-mode engine must be constructed
// with the checkpoint surface (WithSnapshotBase + WithRunID), so adapter state
// is durably persisted at every step boundary under
// <home>/runs/<runID>/snapshots/<session>/. If serverRunEngineOptions drops
// either option, wireCheckpointStore silently no-ops and this test fails on
// the missing snapshots. The surface is observed at step boundaries via a
// hook sink because a completed run's retention janitor deletes its
// snapshots before Run returns.
func TestServerModeResume_CheckpointSurfaceWired(t *testing.T) {
	requireNoGoroutineLeak(t)
	stateDir := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", stateDir)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	log := newApplyLogger()
	wfPath := writeWorkflowFile(t, twoStepWorkflow)
	_, graph, loader, err := compileForExecution(ctx, wfPath, log, false, false)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	defer func() { _ = loader.Shutdown(context.WithoutCancel(ctx)) }()

	handle := &statefulHandle{name: "noop"}
	ckLoader := &staticHandleLoader{handle: handle}

	runID := uuid.NewString()
	ft := &fakeTransport{}
	sink := buildServerSink(ctx, ft, nil, runID, graph, wfPath, "http://fake.test", "", log, nil)
	state := newLocalRunState(runID, graph.Name, "http://fake.test")
	snapDir := filepath.Join(stateDir, "runs", runID, "snapshots", "noop.default")

	var captures []snapshotCapture
	hooked := &stepHookSink{Sink: sink, onStepOutcome: func(step string) {
		c := captureLatestSnapshot(t, snapDir)
		c.step = step
		captures = append(captures, c)
	}}

	eng, err := buildServerRunEngine(graph, ckLoader, hooked, state, applyOptions{workflowPath: wfPath}, "", nil, "")
	if err != nil {
		t.Fatalf("buildServerRunEngine: %v", err)
	}
	if err := eng.Run(ctx); err != nil {
		t.Fatalf("engine run: %v", err)
	}

	// One capture per executed step boundary proves CheckpointSave ran and the
	// state blob landed on disk before the step was reported done.
	if len(captures) != 2 {
		t.Fatalf("expected 2 step-boundary captures (step_one, step_two), got %d", len(captures))
	}
	for i, want := range []string{"step_one", "step_two"} {
		c := captures[i]
		if c.step != want {
			t.Errorf("capture %d step = %q, want %q", i, c.step, want)
		}
		if len(c.pairs) != i+1 {
			t.Fatalf("capture %q saw %d snapshot pairs under %s, want %d", c.step, len(c.pairs), snapDir, i+1)
		}
		if c.pairs[i] != fmt.Sprintf("%010d", i+1) {
			t.Errorf("capture %q latest seq = %q, want %010d", c.step, c.pairs[i], i+1)
		}
		if wantBlob := fmt.Sprintf("state-after-call-%d", i+1); string(c.latestBlob) != wantBlob {
			t.Errorf("capture %q blob = %q, want %q", c.step, c.latestBlob, wantBlob)
		}
		if c.stateSchema != "stateful.v1" {
			t.Errorf("capture %q state_schema = %q, want %q (declared schema tag stamped on the sidecar)", c.step, c.stateSchema, "stateful.v1")
		}
	}
	if len(handle.restores()) != 0 {
		t.Errorf("expected no Restore replay in a fresh run, got %d", len(handle.restores()))
	}
	completed := false
	for _, env := range ft.published {
		if env.GetRunCompleted() != nil {
			completed = true
		}
	}
	if !completed {
		t.Error("expected RunCompleted event from the engine run")
	}

	// CRI-203: every durable save also published its advisory checkpoint
	// pointer into the stream the server persists for ListRunEvents
	// consumers. Each pointer must mirror the persisted state row it refers
	// to (castle keeps only pointers; the blob stays engine-local), keyed by
	// the graph adapter identity and the save's sequence number.
	var ptrs []*pb.CheckpointPointer
	for _, env := range ft.published {
		if p := env.GetCheckpointPointer(); p != nil {
			ptrs = append(ptrs, p)
		}
	}
	if len(ptrs) != len(captures) {
		t.Fatalf("published %d checkpoint pointers, want one per durable save (%d)", len(ptrs), len(captures))
	}
	for i, p := range ptrs {
		wantStateID := fmt.Sprintf("noop.default/%010d", i+1)
		if p.GetStateId() != wantStateID {
			t.Errorf("pointer %d state_id = %q, want %q", i, p.GetStateId(), wantStateID)
		}
		if p.GetSessionId() != "noop.default" {
			t.Errorf("pointer %d session_id = %q, want %q", i, p.GetSessionId(), "noop.default")
		}
		if p.GetAdapterKind() != "noop" {
			t.Errorf("pointer %d adapter_kind = %q, want %q", i, p.GetAdapterKind(), "noop")
		}
		if p.GetStateSchema() != "stateful.v1" {
			t.Errorf("pointer %d state_schema = %q, want %q", i, p.GetStateSchema(), "stateful.v1")
		}
		if wantDigest := adapterhost.ComputeStateDigest(captures[i].latestBlob); p.GetStateDigest() != wantDigest {
			t.Errorf("pointer %d state_digest = %q, want %q (the persisted blob's digest)", i, p.GetStateDigest(), wantDigest)
		}
		if wantSize := int64(len(captures[i].latestBlob)); p.GetStateSize() != wantSize {
			t.Errorf("pointer %d state_size = %d, want %d", i, p.GetStateSize(), wantSize)
		}
		if p.GetGranularity() != string(workflow.StateGranularityPerStep) {
			t.Errorf("pointer %d granularity = %q, want %q", i, p.GetGranularity(), string(workflow.StateGranularityPerStep))
		}
	}
}
