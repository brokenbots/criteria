package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	servertrans "github.com/brokenbots/criteria/internal/transport/server"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
	"github.com/brokenbots/criteria/workflow"
)

// fakeTransport is a test-only implementation of reattachTransport that lets
// tests configure per-call responses and inspect recorded calls.
type fakeTransport struct {
	// reattachResp/reattachErr control ReattachRun return values.
	reattachResp *pb.ReattachRunResponse
	reattachErr  error

	// startStreamsErr controls StartStreams return value.
	startStreamsErr error

	// resumeCh is returned by ResumeCh().
	resumeCh chan *pb.ResumeRun

	// published accumulates envelopes passed to Publish.
	published []*pb.Envelope
}

func (f *fakeTransport) ReattachRun(_ context.Context, _, _ string) (*pb.ReattachRunResponse, error) {
	return f.reattachResp, f.reattachErr
}

func (f *fakeTransport) StartStreams(_ context.Context, _ string) error {
	return f.startStreamsErr
}

func (f *fakeTransport) Drain(_ context.Context) {}

func (f *fakeTransport) ResumeCh() <-chan *pb.ResumeRun {
	if f.resumeCh == nil {
		f.resumeCh = make(chan *pb.ResumeRun)
	}
	return f.resumeCh
}

func (f *fakeTransport) Publish(_ context.Context, env *pb.Envelope) {
	f.published = append(f.published, env)
}

// discardLogger returns a logger that silently discards all output.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newOfflineClient creates a *servertrans.Client with a dead-but-valid URL.
// No network connections are made at construction time.
func newOfflineClient(t *testing.T) *servertrans.Client {
	t.Helper()
	c, err := servertrans.NewClient("http://localhost:1", discardLogger())
	if err != nil {
		t.Fatalf("newOfflineClient: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// writeCheckpointDirect writes a checkpoint JSON file directly (bypassing the
// workflow-path accessibility check in WriteStepCheckpoint).
func writeCheckpointDirect(t *testing.T, stateDir string, cp *StepCheckpoint) {
	t.Helper()
	t.Setenv("CRITERIA_STATE_DIR", stateDir)
	dir := filepath.Join(stateDir, "runs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir runs: %v", err)
	}
	b, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		t.Fatalf("marshal checkpoint: %v", err)
	}
	p := filepath.Join(dir, cp.RunID+".json")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatalf("write checkpoint: %v", err)
	}
}

// minimalWorkflow is a no-step single-terminal-state workflow for tests that
// only need a compilable workflow without running any adapter.
const minimalWorkflow = `
workflow "minimal" {
  version       = "0.1"
  initial_state = "done"
  target_state  = "done"

  state "done" {
    terminal = true
    success  = true
  }
}
`

// twoStepShellWorkflow is used for resume tests that need an executable workflow.
const twoStepShellWorkflow = `
workflow "shell_resume" {
  version       = "0.1"
  initial_state = "greet"
  target_state  = "done"

  adapter "shell" "default" {}

  step "greet" {
    target = adapter.shell.default
    input {
      command = "echo hello"
    }
    outcome "success" { transition_to = "done" }
    outcome "failure" { transition_to = "failed" }
  }

  state "done" {
    terminal = true
    success  = true
  }
  state "failed" {
    terminal = true
    success  = false
  }
}
`

// maxRetryWorkflow has max_step_retries = 0 to trigger retry-exceeded paths.
const maxRetryWorkflow = `
workflow "max_retry" {
  version       = "0.1"
  initial_state = "greet"
  target_state  = "done"

  adapter "shell" "default" {}

  policy {
    max_step_retries = 0
  }

  step "greet" {
    target = adapter.shell.default
    input {
      command = "echo hi"
    }
    outcome "success" { transition_to = "done" }
    outcome "failure" { transition_to = "failed" }
  }

  state "done" {
    terminal = true
    success  = true
  }
  state "failed" {
    terminal = true
    success  = false
  }
}
`

// TestApplyClientOptions verifies all TLS mode and codec combinations are
// mapped through to the Options struct without mutation.
func TestApplyClientOptions(t *testing.T) {
	cases := []struct {
		name     string
		opts     applyOptions
		wantMode servertrans.TLSMode
		wantCA   string
	}{
		{
			name:     "empty defaults",
			opts:     applyOptions{},
			wantMode: "",
		},
		{
			name:     "tls mode with CA",
			opts:     applyOptions{tlsMode: "tls", tlsCA: "/ca.pem"},
			wantMode: servertrans.TLSEnable,
			wantCA:   "/ca.pem",
		},
		{
			name:     "mtls mode",
			opts:     applyOptions{tlsMode: "mtls", tlsCert: "/c.pem", tlsKey: "/k.pem"},
			wantMode: servertrans.TLSMutual,
		},
		{
			name:     "disable mode",
			opts:     applyOptions{tlsMode: "disable"},
			wantMode: servertrans.TLSDisable,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := applyClientOptions(tc.opts)
			if got.TLSMode != tc.wantMode {
				t.Errorf("TLSMode: got %q want %q", got.TLSMode, tc.wantMode)
			}
			if tc.wantCA != "" && got.CAFile != tc.wantCA {
				t.Errorf("CAFile: got %q want %q", got.CAFile, tc.wantCA)
			}
		})
	}
}

// TestNewLocalRunState verifies the constructor captures PID and fields.
func TestNewLocalRunState(t *testing.T) {
	st := newLocalRunState("run-1", "my-wf", "http://srv")
	if st.RunID != "run-1" {
		t.Errorf("RunID: got %q", st.RunID)
	}
	if st.Workflow != "my-wf" {
		t.Errorf("Workflow: got %q", st.Workflow)
	}
	if st.ServerURL != "http://srv" {
		t.Errorf("ServerURL: got %q", st.ServerURL)
	}
	if st.PID != os.Getpid() {
		t.Errorf("PID: got %d want %d", st.PID, os.Getpid())
	}
}

// TestAbandonCheckpoint verifies the checkpoint file is removed and the
// log line is emitted.
func TestAbandonCheckpoint(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", stateDir)

	wfFile := writeWorkflowFile(t, minimalWorkflow)
	cp := &StepCheckpoint{
		RunID:        "cp-abandon",
		Workflow:     "minimal",
		WorkflowPath: wfFile,
		CurrentStep:  "done",
	}
	if err := WriteStepCheckpoint(cp); err != nil {
		t.Fatalf("WriteStepCheckpoint: %v", err)
	}

	var logBuf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	abandonCheckpoint(log, cp, "test abandon reason", nil)

	// Checkpoint file should be gone.
	list, _ := ListStepCheckpoints()
	for _, item := range list {
		if item.RunID == "cp-abandon" {
			t.Error("checkpoint still present after abandonCheckpoint")
		}
	}
	if !strings.Contains(logBuf.String(), "test abandon reason") {
		t.Errorf("expected log message in output, got: %s", logBuf.String())
	}
}

// TestAbandonCheckpoint_WithError verifies the error is logged when provided.
func TestAbandonCheckpoint_WithError(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", stateDir)

	wfFile := writeWorkflowFile(t, minimalWorkflow)
	cp := &StepCheckpoint{RunID: "cp-err", Workflow: "minimal", WorkflowPath: wfFile}
	if err := WriteStepCheckpoint(cp); err != nil {
		t.Fatalf("WriteStepCheckpoint: %v", err)
	}

	var logBuf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logBuf, nil))
	abandonCheckpoint(log, cp, "with error", &stringErrCLI{msg: "underlying failure"})

	if !strings.Contains(logBuf.String(), "underlying failure") {
		t.Errorf("error not logged; log output: %s", logBuf.String())
	}
}

// TestBuildRecoveryClient_MissingCredentials verifies that a checkpoint
// without CriteriaID/Token is abandoned and an error is returned.
func TestBuildRecoveryClient_MissingCredentials(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", stateDir)

	wfFile := writeWorkflowFile(t, minimalWorkflow)
	cp := &StepCheckpoint{
		RunID:        "cp-nocreds",
		Workflow:     "minimal",
		WorkflowPath: wfFile,
		CriteriaID:   "", // missing
		Token:        "", // missing
		ServerURL:    "http://localhost:9",
	}
	if err := WriteStepCheckpoint(cp); err != nil {
		t.Fatalf("WriteStepCheckpoint: %v", err)
	}

	rc, err := buildRecoveryClient(discardLogger(), cp, &servertrans.Options{})
	if err == nil {
		if rc != nil {
			rc.Close()
		}
		t.Fatal("expected error for missing credentials")
	}
	// Checkpoint must have been removed.
	list, _ := ListStepCheckpoints()
	for _, item := range list {
		if item.RunID == "cp-nocreds" {
			t.Error("checkpoint not removed after missing-credentials failure")
		}
	}
}

// TestBuildRecoveryClient_BadServerURL verifies that an invalid server URL
// causes buildRecoveryClient to abandon the checkpoint.
func TestBuildRecoveryClient_BadServerURL(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", stateDir)

	wfFile := writeWorkflowFile(t, minimalWorkflow)
	cp := &StepCheckpoint{
		RunID:        "cp-badurl",
		Workflow:     "minimal",
		WorkflowPath: wfFile,
		CriteriaID:   "crt-1",
		Token:        "tok-1",
		ServerURL:    "ftp://not-http-or-https", // triggers NewClient error
	}
	if err := WriteStepCheckpoint(cp); err != nil {
		t.Fatalf("WriteStepCheckpoint: %v", err)
	}

	rc, err := buildRecoveryClient(discardLogger(), cp, &servertrans.Options{})
	if err == nil {
		if rc != nil {
			rc.Close()
		}
		t.Fatal("expected error for invalid server URL scheme")
	}
	// Checkpoint must have been removed.
	list, _ := ListStepCheckpoints()
	for _, item := range list {
		if item.RunID == "cp-badurl" {
			t.Error("checkpoint not removed after bad-URL failure")
		}
	}
}

// TestLoadCheckpointWorkflow_MissingFile verifies that a missing workflow
// path triggers checkpoint abandonment.
func TestLoadCheckpointWorkflow_MissingFile(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", stateDir)

	cp := &StepCheckpoint{
		RunID:        "cp-missing",
		WorkflowPath: filepath.Join(t.TempDir(), "no-such-file.hcl"),
	}
	writeCheckpointDirect(t, stateDir, cp)

	graph, err := loadCheckpointWorkflow(context.Background(), discardLogger(), cp)
	if err == nil {
		t.Fatal("expected error for missing workflow file")
	}
	_ = graph
}

// TestLoadCheckpointWorkflow_InvalidHCL verifies that unparseable HCL
// triggers checkpoint abandonment.
func TestLoadCheckpointWorkflow_InvalidHCL(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", stateDir)

	badHCL := filepath.Join(t.TempDir(), "bad.hcl")
	if err := os.WriteFile(badHCL, []byte(`{{{not hcl`), 0o600); err != nil {
		t.Fatal(err)
	}
	cp := &StepCheckpoint{RunID: "cp-badhcl", WorkflowPath: badHCL}
	writeCheckpointDirect(t, stateDir, cp)

	graph, err := loadCheckpointWorkflow(context.Background(), discardLogger(), cp)
	if err == nil {
		t.Fatal("expected error for invalid HCL")
	}
	_ = graph
}

// TestLoadCheckpointWorkflow_Valid verifies that a valid HCL workflow file
// returns a non-nil FSMGraph.
func TestLoadCheckpointWorkflow_Valid(t *testing.T) {
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())
	wfFile := writeWorkflowFile(t, minimalWorkflow)
	cp := &StepCheckpoint{RunID: "cp-valid", WorkflowPath: wfFile}
	graph, err := loadCheckpointWorkflow(context.Background(), discardLogger(), cp)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if graph == nil {
		t.Fatal("expected non-nil FSMGraph")
	}
}

// TestParseWorkflowFromPath exercises all failure and success paths.
func TestParseWorkflowFromPath(t *testing.T) {
	t.Run("empty path", func(t *testing.T) {
		_, err := parseWorkflowFromPath(context.Background(), "")
		if err == nil {
			t.Fatal("expected error for empty path")
		}
	})

	t.Run("missing file", func(t *testing.T) {
		_, err := parseWorkflowFromPath(context.Background(), filepath.Join(t.TempDir(), "nope.hcl"))
		if err == nil {
			t.Fatal("expected error for missing file")
		}
	})

	t.Run("invalid HCL", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "bad.hcl")
		_ = os.WriteFile(p, []byte(`not valid hcl {{{`), 0o600)
		_, err := parseWorkflowFromPath(context.Background(), p)
		if err == nil {
			t.Fatal("expected error for invalid HCL")
		}
	})

	t.Run("valid workflow", func(t *testing.T) {
		t.Setenv("CRITERIA_STATE_DIR", t.TempDir())
		wfFile := writeWorkflowFile(t, minimalWorkflow)
		graph, err := parseWorkflowFromPath(context.Background(), wfFile)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if graph == nil || graph.Name != "minimal" {
			t.Fatalf("unexpected graph: %+v", graph)
		}
	})
}

// TestBuildServerSink verifies that the CheckpointFn written by buildServerSink
// calls writeRunCheckpoint with the correct fields.
func TestBuildServerSink(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", stateDir)

	wfFile := writeWorkflowFile(t, minimalWorkflow)
	graph, err := parseWorkflowFromPath(context.Background(), wfFile)
	if err != nil {
		t.Fatalf("parseWorkflowFromPath: %v", err)
	}

	c := newOfflineClient(t)
	log := discardLogger()
	sink := buildServerSink(context.Background(), c, "run-srv-1", graph, wfFile, "http://srv", log, nil)
	if sink == nil {
		t.Fatal("expected non-nil Sink")
	}
	if sink.CheckpointFn == nil {
		t.Fatal("CheckpointFn must be set")
	}

	// Calling CheckpointFn writes a checkpoint with the expected fields.
	sink.CheckpointFn("step1", 1)
	checkpoints, err := ListStepCheckpoints()
	if err != nil {
		t.Fatalf("ListStepCheckpoints: %v", err)
	}
	found := false
	for _, cp := range checkpoints {
		if cp.RunID == "run-srv-1" {
			found = true
			if cp.CurrentStep != "step1" {
				t.Errorf("checkpoint step: got %q want %q", cp.CurrentStep, "step1")
			}
			if cp.Attempt != 1 {
				t.Errorf("checkpoint attempt: got %d want %d", cp.Attempt, 1)
			}
		}
	}
	if !found {
		t.Error("checkpoint for run-srv-1 not found")
	}
}

// TestBuildServerSink_VisitsPersisted verifies that when buildServerSink
// receives a non-nil getVisits callback, the visits returned by that callback
// are written into the StepCheckpoint. This ensures the server checkpoint
// write path cannot regress to dropping visit counts silently.
func TestBuildServerSink_VisitsPersisted(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", stateDir)

	wfFile := writeWorkflowFile(t, minimalWorkflow)
	graph, err := parseWorkflowFromPath(context.Background(), wfFile)
	if err != nil {
		t.Fatalf("parseWorkflowFromPath: %v", err)
	}

	wantVisits := map[string]int{"build": 2, "test": 1}
	c := newOfflineClient(t)
	sink := buildServerSink(context.Background(), c, "run-srv-visits", graph, wfFile, "http://srv", discardLogger(),
		func() map[string]int { return wantVisits })

	sink.CheckpointFn("build", 3)

	checkpoints, err := ListStepCheckpoints()
	if err != nil {
		t.Fatalf("ListStepCheckpoints: %v", err)
	}
	var found *StepCheckpoint
	for _, cp := range checkpoints {
		if cp.RunID == "run-srv-visits" {
			found = cp
			break
		}
	}
	if found == nil {
		t.Fatal("checkpoint for run-srv-visits not found")
	}
	for step, want := range wantVisits {
		if got := found.Visits[step]; got != want {
			t.Errorf("Visits[%q] = %d; want %d", step, got, want)
		}
	}
}

// TestBuildLocalCheckpointFn_VisitsPersisted verifies that buildLocalCheckpointFn
// writes the visits returned by getVisits into the StepCheckpoint. Mirrors
// TestBuildServerSink_VisitsPersisted for the initial-run local path. This would
// fail if buildLocalCheckpointFn stopped calling getVisits() or stopped assigning
// its result to cp.Visits.
func TestBuildLocalCheckpointFn_VisitsPersisted(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", stateDir)

	wfFile := writeWorkflowFile(t, maxVisitsWorkflow)
	wantVisits := map[string]int{"work": 2, "review": 1}

	fn := buildLocalCheckpointFn(discardLogger(), "local-fn-visits", "max_visits_test", wfFile,
		func() map[string]int { return wantVisits })
	fn("work", 1)

	checkpoints, err := ListStepCheckpoints()
	if err != nil {
		t.Fatalf("ListStepCheckpoints: %v", err)
	}
	var found *StepCheckpoint
	for _, cp := range checkpoints {
		if cp.RunID == "local-fn-visits" {
			found = cp
			break
		}
	}
	if found == nil {
		t.Fatal("checkpoint not found; buildLocalCheckpointFn may not have written it")
	}
	for step, want := range wantVisits {
		if got := found.Visits[step]; got != want {
			t.Errorf("Visits[%q] = %d; want %d", step, got, want)
		}
	}
}

// TestBuildReattachTrackerAndEngine_VisitsPersisted proves that the local
// checkpoint write path records live visit counts from the engine. It calls
// buildReattachTrackerAndEngine directly, runs the returned engine, and asserts
// that the checkpoint written to disk during OnStepEntered contains the
// incremented visit count from eng.VisitCounts(). This would fail if the
// checkpointFn closure removed the `cp.Visits = eng.VisitCounts()` assignment,
// leaving the persisted checkpoint with nil/empty visits — which would cause
// subsequent crash-recovery resumes to ignore prior visit history.
func TestBuildReattachTrackerAndEngine_VisitsPersisted(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", stateDir)

	// maxVisitsWorkflow: step "work" with max_visits=1 using shell "echo hi".
	// With no prior visits (Visits=nil), the first attempt succeeds:
	//   incrementVisit: nil → {"work":1} (0 < 1, gate passes)
	//   OnStepEntered  → checkpointFn writes checkpoint with eng.VisitCounts()={"work":1}
	//   shell succeeds → done.
	wfFile := writeWorkflowFile(t, maxVisitsWorkflow)
	cp := &StepCheckpoint{
		RunID:        "brate-visits-written",
		WorkflowPath: wfFile,
		CurrentStep:  "work",
		Attempt:      0,
		// Visits deliberately nil: proves the closure reads from the live engine,
		// not from the seed checkpoint.
	}
	if err := WriteStepCheckpoint(cp); err != nil {
		t.Fatalf("WriteStepCheckpoint: %v", err)
	}

	graph, loader, _, ok := prepareReattach(context.Background(), discardLogger(), cp)
	if !ok {
		t.Fatal("prepareReattach failed")
	}
	defer loader.Shutdown(context.Background())

	var out bytes.Buffer
	_, _, eng := buildReattachTrackerAndEngine(cp, discardLogger(), graph, loader, &out, outputModeJSON, 1)

	// Run the engine. checkpointFn fires from OnStepEntered with liveRunState.Visits={"work":1}.
	if runErr := eng.RunFrom(context.Background(), "work", 1); runErr != nil {
		t.Fatalf("unexpected RunFrom error: %v", runErr)
	}

	// buildReattachTrackerAndEngine writes checkpoints but never removes them;
	// the file on disk must reflect the live visit count from eng.VisitCounts().
	checkpoints, err := ListStepCheckpoints()
	if err != nil {
		t.Fatalf("ListStepCheckpoints: %v", err)
	}
	var found *StepCheckpoint
	for _, item := range checkpoints {
		if item.RunID == "brate-visits-written" {
			found = item
			break
		}
	}
	if found == nil {
		t.Fatal("checkpoint not found after run; checkpointFn may not have fired during OnStepEntered")
	}
	if got := found.Visits["work"]; got != 1 {
		t.Errorf("checkpoint Visits[%q] = %d; want 1 — checkpointFn must assign eng.VisitCounts()", "work", got)
	}
}

// TestResumeOneLocalRun_HappyPath verifies that a local checkpoint is resumed
// and the checkpoint file is cleaned up after successful completion.
func TestResumeOneLocalRun_HappyPath(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", stateDir)

	wfFile := writeWorkflowFile(t, twoStepShellWorkflow)
	cp := &StepCheckpoint{
		RunID:        "local-resume-1",
		Workflow:     "shell_resume",
		WorkflowPath: wfFile,
		CurrentStep:  "greet",
		Attempt:      0, // attempt 0 → nextAttempt=1 ≤ maxAttempts=1, so the engine path runs
	}
	if err := WriteStepCheckpoint(cp); err != nil {
		t.Fatalf("WriteStepCheckpoint: %v", err)
	}

	var out bytes.Buffer
	resumeOneLocalRun(context.Background(), discardLogger(), cp, &out, outputModeJSON)

	// Checkpoint must be cleaned up after successful resume.
	checkpoints, _ := ListStepCheckpoints()
	for _, item := range checkpoints {
		if item.RunID == "local-resume-1" {
			t.Error("checkpoint not removed after successful local resume")
		}
	}
}

// TestResumeOneLocalRun_MissingWorkflow verifies that a checkpoint with a
// missing workflow file is abandoned cleanly.
func TestResumeOneLocalRun_MissingWorkflow(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", stateDir)

	missingPath := filepath.Join(t.TempDir(), "no-such-file.hcl")
	cp := &StepCheckpoint{
		RunID:        "local-missing-wf",
		Workflow:     "ghost",
		WorkflowPath: missingPath,
		CurrentStep:  "step1",
	}
	// Write directly, bypassing the path-accessibility check.
	writeCheckpointDirect(t, stateDir, cp)

	var out bytes.Buffer
	resumeOneLocalRun(context.Background(), discardLogger(), cp, &out, outputModeJSON)

	// Checkpoint must be removed (abandoned).
	checkpoints, _ := ListStepCheckpoints()
	for _, item := range checkpoints {
		if item.RunID == "local-missing-wf" {
			t.Error("checkpoint not removed after missing-workflow abandonment")
		}
	}
}

// TestResumeOneLocalRun_ExceedsMaxRetries verifies that a checkpoint exceeding
// max_step_retries emits a RunFailed event and removes the checkpoint.
func TestResumeOneLocalRun_ExceedsMaxRetries(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", stateDir)

	wfFile := writeWorkflowFile(t, maxRetryWorkflow)
	// Attempt 2 with max_step_retries=0 means maxAttempts=1; nextAttempt=3 > 1.
	cp := &StepCheckpoint{
		RunID:        "max-retry-run",
		Workflow:     "max_retry",
		WorkflowPath: wfFile,
		CurrentStep:  "greet",
		Attempt:      2,
	}
	if err := WriteStepCheckpoint(cp); err != nil {
		t.Fatalf("WriteStepCheckpoint: %v", err)
	}

	var out bytes.Buffer
	resumeOneLocalRun(context.Background(), discardLogger(), cp, &out, outputModeJSON)

	// Checkpoint removed regardless.
	checkpoints, _ := ListStepCheckpoints()
	for _, item := range checkpoints {
		if item.RunID == "max-retry-run" {
			t.Error("checkpoint not removed after max-retry failure")
		}
	}
	// A RunFailed event must have been written to the output buffer.
	if !strings.Contains(out.String(), "RunFailed") {
		t.Errorf("expected RunFailed in output, got: %s", out.String())
	}
}

// TestResumeOneLocalRun_VisitsRestored verifies that visit counts persisted in a
// local StepCheckpoint are seeded into the resumed engine and that max_visits is
// enforced against the restored count. With Visits={"work":1} and max_visits=1,
// the first incrementVisit call in the resumed engine must fail immediately,
// proving the local buildReattachTrackerAndEngine → WithResumedVisits path works
// end-to-end. This would fail if the visits map were dropped before engine
// construction or never written into the resumed engine options.
func TestResumeOneLocalRun_VisitsRestored(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", stateDir)

	wfFile := writeWorkflowFile(t, maxVisitsWorkflow)
	cp := &StepCheckpoint{
		RunID:        "local-visits-restored",
		Workflow:     "max_visits_test",
		WorkflowPath: wfFile,
		CurrentStep:  "work",
		Attempt:      0,                         // nextAttempt=1 ≤ maxAttempts=1
		Visits:       map[string]int{"work": 1}, // already at the max_visits=1 limit
	}
	if err := WriteStepCheckpoint(cp); err != nil {
		t.Fatalf("WriteStepCheckpoint: %v", err)
	}

	var out bytes.Buffer
	resumeOneLocalRun(context.Background(), discardLogger(), cp, &out, outputModeJSON)

	// Checkpoint must be cleaned up regardless of failure.
	checkpoints, _ := ListStepCheckpoints()
	for _, item := range checkpoints {
		if item.RunID == "local-visits-restored" {
			t.Error("checkpoint not removed after max_visits failure on local resume")
		}
	}
	// Output must contain a RunFailed event that mentions exceeded max_visits.
	if !strings.Contains(out.String(), "RunFailed") {
		t.Errorf("expected RunFailed in output, got: %s", out.String())
	}
	if !strings.Contains(out.String(), "exceeded max_visits") {
		t.Errorf("expected 'exceeded max_visits' in output, got: %s", out.String())
	}
}

// stringErrCLI is a minimal error type for CLI-package tests.
type stringErrCLI struct{ msg string }

func (e *stringErrCLI) Error() string { return e.msg }

// newCheckpointWithWorkflow is a test helper that writes a workflow to a temp
// file and returns a StepCheckpoint pointing at it.
func newCheckpointWithWorkflow(t *testing.T, stateDir, runID, hcl string) *StepCheckpoint {
	t.Helper()
	t.Setenv("CRITERIA_STATE_DIR", stateDir)
	wfFile := writeWorkflowFile(t, hcl)
	cp := &StepCheckpoint{
		RunID:        runID,
		WorkflowPath: wfFile,
		CriteriaID:   "crt-test",
		Token:        "tok-test",
		ServerURL:    "http://localhost:9",
	}
	return cp
}

// TestAttemptReattach_RPCError verifies that an RPC failure abandons the
// checkpoint and returns a non-nil error.
func TestAttemptReattach_RPCError(t *testing.T) {
	stateDir := t.TempDir()
	cp := newCheckpointWithWorkflow(t, stateDir, "ra-rpc-err", minimalWorkflow)
	writeCheckpointDirect(t, stateDir, cp)

	ft := &fakeTransport{reattachErr: errors.New("connection refused")}
	resp, err := attemptReattach(context.Background(), discardLogger(), ft, cp)
	if err == nil {
		t.Fatal("expected error for RPC failure")
	}
	if resp != nil {
		t.Errorf("expected nil response, got %v", resp)
	}
	// Checkpoint must be removed.
	list, _ := ListStepCheckpoints()
	for _, item := range list {
		if item.RunID == cp.RunID {
			t.Error("checkpoint not removed after RPC error")
		}
	}
}

// TestAttemptReattach_NotResumable verifies that CanResume=false causes the
// checkpoint to be removed and (nil, nil) to be returned.
func TestAttemptReattach_NotResumable(t *testing.T) {
	stateDir := t.TempDir()
	cp := newCheckpointWithWorkflow(t, stateDir, "ra-not-resumable", minimalWorkflow)
	writeCheckpointDirect(t, stateDir, cp)

	ft := &fakeTransport{
		reattachResp: &pb.ReattachRunResponse{CanResume: false, Status: "failed"},
	}
	resp, err := attemptReattach(context.Background(), discardLogger(), ft, cp)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp != nil {
		t.Errorf("expected nil response for non-resumable run, got %v", resp)
	}
	// Checkpoint must be removed.
	list, _ := ListStepCheckpoints()
	for _, item := range list {
		if item.RunID == cp.RunID {
			t.Error("checkpoint not removed for non-resumable run")
		}
	}
}

// TestAttemptReattach_Success verifies that a resumable run returns the
// response unchanged.
func TestAttemptReattach_Success(t *testing.T) {
	stateDir := t.TempDir()
	cp := newCheckpointWithWorkflow(t, stateDir, "ra-success", minimalWorkflow)
	writeCheckpointDirect(t, stateDir, cp)

	want := &pb.ReattachRunResponse{
		CanResume:   true,
		Status:      "running",
		CurrentStep: "greet",
		Attempt:     1,
	}
	ft := &fakeTransport{reattachResp: want}
	resp, err := attemptReattach(context.Background(), discardLogger(), ft, cp)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}
	if resp.CurrentStep != want.CurrentStep || resp.Attempt != want.Attempt {
		t.Errorf("response mismatch: got %v want %v", resp, want)
	}
}

// TestResumeActiveRun_ExceedsMaxRetries verifies that when nextAttempt exceeds
// max_step_retries the run is failed and the checkpoint is removed.
func TestResumeActiveRun_ExceedsMaxRetries(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", stateDir)

	wfFile := writeWorkflowFile(t, maxRetryWorkflow)
	cp := &StepCheckpoint{RunID: "rar-exceeded", WorkflowPath: wfFile}
	writeCheckpointDirect(t, stateDir, cp)

	// Attempt=1 with MaxStepRetries=0 means maxAttempts=1; nextAttempt=2 > 1.
	resp := &pb.ReattachRunResponse{
		CanResume:   true,
		Status:      "running",
		CurrentStep: "greet",
		Attempt:     1,
	}
	graph, err := parseWorkflowFromPath(context.Background(), wfFile)
	if err != nil {
		t.Fatalf("parseWorkflowFromPath: %v", err)
	}

	ft := &fakeTransport{}
	resumeActiveRun(context.Background(), discardLogger(), ft, cp, graph, resp)

	// Checkpoint must be removed.
	list, _ := ListStepCheckpoints()
	for _, item := range list {
		if item.RunID == cp.RunID {
			t.Error("checkpoint not removed after retry-exceeded")
		}
	}
	// A RunFailed envelope must have been published.
	hasRunFailed := false
	for _, env := range ft.published {
		if env.GetRunFailed() != nil {
			hasRunFailed = true
		}
	}
	if !hasRunFailed {
		t.Errorf("expected RunFailed event to be published, got %d envelopes", len(ft.published))
	}
}

// TestResumeActiveRun_HappyPath verifies that a valid resume starts streams,
// emits OnStepResumed, runs the engine, and cleans up the checkpoint.
func TestResumeActiveRun_HappyPath(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", stateDir)

	// Use the minimal workflow: initial_state = target_state = "done".
	// RunFrom("done", 1) terminates immediately via ErrTerminal → OnRunCompleted.
	wfFile := writeWorkflowFile(t, minimalWorkflow)
	cp := &StepCheckpoint{RunID: "rar-happy", WorkflowPath: wfFile}
	writeCheckpointDirect(t, stateDir, cp)

	resp := &pb.ReattachRunResponse{
		CanResume:   true,
		Status:      "running",
		CurrentStep: "done", // terminal state → engine finishes immediately
		Attempt:     0,      // nextAttempt=1 ≤ maxAttempts=1 (MaxStepRetries=0 default)
	}
	graph, err := parseWorkflowFromPath(context.Background(), wfFile)
	if err != nil {
		t.Fatalf("parseWorkflowFromPath: %v", err)
	}

	ft := &fakeTransport{}
	resumeActiveRun(context.Background(), discardLogger(), ft, cp, graph, resp)

	// Checkpoint must be removed after the run completes.
	list, _ := ListStepCheckpoints()
	for _, item := range list {
		if item.RunID == cp.RunID {
			t.Error("checkpoint not removed after active resume")
		}
	}
	// OnStepResumed must have been published (first publish call).
	if len(ft.published) == 0 {
		t.Fatal("expected at least one published envelope (OnStepResumed)")
	}
	// Verify OnRunCompleted was also published (terminal state reached).
	hasCompleted := false
	for _, env := range ft.published {
		if env.GetRunCompleted() != nil {
			hasCompleted = true
		}
	}
	if !hasCompleted {
		t.Errorf("expected RunCompleted event; published envelopes: %d", len(ft.published))
	}
}

// maxVisitsWorkflow has max_visits = 1 on step "work" for testing visit-count
// persistence across reattach.
const maxVisitsWorkflow = `
workflow "max_visits_test" {
  version       = "0.1"
  initial_state = "work"
  target_state  = "done"

  adapter "shell" "default" {}

  step "work" {
    target = adapter.shell.default
    max_visits = 1
    input {
      command = "echo hi"
    }
    outcome "success" { transition_to = "done" }
    outcome "failure" { transition_to = "failed" }
  }

  state "done" {
    terminal = true
    success  = true
  }
  state "failed" {
    terminal = true
    success  = false
  }
}
`

// TestResumeActiveRun_VisitsRestored verifies that visit counts persisted in a
// StepCheckpoint are seeded into the resumed engine and that max_visits is
// enforced against the restored count. With Visits={"work":1} and
// max_visits=1, the first incrementVisit call in the resumed engine must fail.
func TestResumeActiveRun_VisitsRestored(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", stateDir)

	wfFile := writeWorkflowFile(t, maxVisitsWorkflow)
	cp := &StepCheckpoint{
		RunID:        "rar-visits",
		WorkflowPath: wfFile,
		Visits:       map[string]int{"work": 1}, // already at the limit
	}
	writeCheckpointDirect(t, stateDir, cp)

	resp := &pb.ReattachRunResponse{
		CanResume:   true,
		Status:      "running",
		CurrentStep: "work",
		Attempt:     0, // nextAttempt=1 ≤ maxAttempts=1
	}
	graph, err := parseWorkflowFromPath(context.Background(), wfFile)
	if err != nil {
		t.Fatalf("parseWorkflowFromPath: %v", err)
	}

	ft := &fakeTransport{}
	resumeActiveRun(context.Background(), discardLogger(), ft, cp, graph, resp)

	// The engine must emit RunFailed because visits["work"]=1 >= max_visits=1.
	var gotFailed bool
	for _, env := range ft.published {
		if rf := env.GetRunFailed(); rf != nil {
			gotFailed = true
			if !strings.Contains(rf.GetReason(), "exceeded max_visits") {
				t.Errorf("RunFailed reason %q should mention exceeded max_visits", rf.GetReason())
			}
		}
	}
	if !gotFailed {
		t.Error("expected RunFailed event when checkpoint visits match max_visits limit")
	}
}

// TestResumePausedRun_StartsStreamsAndRunsEngine verifies that resumePausedRun
// starts streams, restores state, and drives the engine for a paused run that
// contains only a terminal state as the start node (immediate completion).
func TestResumePausedRun_StartsStreamsAndRunsEngine(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", stateDir)

	// minimal workflow: starting from "done" → engine exits immediately.
	wfFile := writeWorkflowFile(t, minimalWorkflow)
	cp := &StepCheckpoint{RunID: "rpr-happy", WorkflowPath: wfFile}
	writeCheckpointDirect(t, stateDir, cp)

	resp := &pb.ReattachRunResponse{
		CanResume:     true,
		Status:        "paused",
		CurrentStep:   "done", // terminal → engine completes immediately
		PendingSignal: "start",
	}
	graph, err := parseWorkflowFromPath(context.Background(), wfFile)
	if err != nil {
		t.Fatalf("parseWorkflowFromPath: %v", err)
	}

	ft := &fakeTransport{}
	resumePausedRun(context.Background(), discardLogger(), ft, cp, graph, resp)

	// Checkpoint must be removed.
	list, _ := ListStepCheckpoints()
	for _, item := range list {
		if item.RunID == cp.RunID {
			t.Error("checkpoint not removed after paused run completion")
		}
	}
	// At least one envelope must have been published, and the terminal
	// envelope must be RunCompleted (not just "something was published").
	if len(ft.published) == 0 {
		t.Fatal("expected at least one published envelope")
	}
	hasRunCompleted := false
	for _, env := range ft.published {
		if env.GetRunCompleted() != nil {
			hasRunCompleted = true
			break
		}
	}
	if !hasRunCompleted {
		t.Errorf("expected RunCompleted envelope; published envelopes: %d", len(ft.published))
	}
}

// TestResumePausedRun_StartStreamsError verifies that a StartStreams failure
// abandons the checkpoint without running the engine.
func TestResumePausedRun_StartStreamsError(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", stateDir)

	wfFile := writeWorkflowFile(t, minimalWorkflow)
	cp := &StepCheckpoint{RunID: "rpr-streams-err", WorkflowPath: wfFile}
	writeCheckpointDirect(t, stateDir, cp)

	resp := &pb.ReattachRunResponse{Status: "paused", CurrentStep: "done"}
	graph, err := parseWorkflowFromPath(context.Background(), wfFile)
	if err != nil {
		t.Fatalf("parseWorkflowFromPath: %v", err)
	}

	ft := &fakeTransport{startStreamsErr: fmt.Errorf("connection refused")}
	resumePausedRun(context.Background(), discardLogger(), ft, cp, graph, resp)

	// Checkpoint must be removed (abandoned on stream error).
	list, _ := ListStepCheckpoints()
	for _, item := range list {
		if item.RunID == cp.RunID {
			t.Error("checkpoint not removed after StartStreams failure")
		}
	}
	// No engine events should have been published.
	if len(ft.published) != 0 {
		t.Errorf("expected no published envelopes, got %d", len(ft.published))
	}
}

// TestResumeOneLocalRun_ServerNodeRejected verifies that a checkpoint for a
// workflow containing a server-only node (approval) is abandoned cleanly.
func TestResumeOneLocalRun_ServerNodeRejected(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", stateDir)

	wfFile := writeWorkflowFile(t, `
workflow "needs_approval" {
  version       = "0.1"
  initial_state = "review"
  target_state  = "done"

  approval "review" {
    approvers = ["alice"]
    reason    = "ship it?"
    outcome "approved" { transition_to = "done" }
    outcome "rejected" { transition_to = "done" }
  }

  state "done" {
    terminal = true
    success  = true
  }
}
`)
	cp := &StepCheckpoint{
		RunID:        "server-node-run",
		Workflow:     "needs_approval",
		WorkflowPath: wfFile,
		CurrentStep:  "review",
		Attempt:      0,
	}
	if err := WriteStepCheckpoint(cp); err != nil {
		t.Fatalf("WriteStepCheckpoint: %v", err)
	}

	var out bytes.Buffer
	resumeOneLocalRun(context.Background(), discardLogger(), cp, &out, outputModeJSON)

	// Checkpoint must be cleared (unsupported in local mode).
	checkpoints, _ := ListStepCheckpoints()
	for _, item := range checkpoints {
		if item.RunID == "server-node-run" {
			t.Error("checkpoint not removed for server-node workflow")
		}
	}
}

// TestCheckIterationCursorValidity_CurrentMissingFromBody verified that
// checkIterationCursorValidity errored when a body step was missing from an
// inline-workflow step. Inline workflow bodies were removed in W13; body step
// validation no longer exists. The test is preserved as a permanent skip so
// the intent is documented for any future subworkflow-cursor validation work.
func TestCheckIterationCursorValidity_CurrentMissingFromBody(t *testing.T) {
	t.Skip("test uses removed inline workflow body feature (W13/W14); subworkflow cursor validation is a separate future workstream")
}

// TestIter_ResumeRejectsModifiedBody is the package-level mirror of
// TestCheckIterationCursorValidity_CurrentMissingFromBody (see above).
func TestIter_ResumeRejectsModifiedBody(t *testing.T) {
	t.Skip("test uses removed inline workflow body feature (W13/W14); subworkflow cursor validation is a separate future workstream")
}

const iterCursorWorkflow = `
workflow "iter_cursor" {
  version       = "0.1"
  initial_state = "execute"
  target_state  = "done"

  adapter "noop" "default" {}

  step "execute" {
    target = adapter.noop.default
    for_each  = ["a", "b"]
    outcome "all_succeeded" { transition_to = "done" }
    outcome "any_failed"    { transition_to = "done" }
  }

  state "done" {
    terminal = true
    success  = true
  }
}
`

func compileIterCursorWorkflow(t *testing.T) *workflow.FSMGraph {
	t.Helper()
	spec, diags := workflow.Parse("iter_cursor.hcl", []byte(iterCursorWorkflow))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := workflow.Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	return g
}

// iterCursorScopeForStep returns a serialised variableScope JSON with an
// in-progress iter cursor for the given step name. Used by the
// checkIterationCursorValidity tests to simulate a checkpoint taken during an
// active iteration.
func iterCursorScopeForStep(t *testing.T, g *workflow.FSMGraph, stepName string) string {
	t.Helper()
	vars := workflow.SeedVarsFromGraph(g)
	scope, err := workflow.SerializeVarScope(vars, []workflow.IterCursor{{
		StepName:   stepName,
		InProgress: true,
	}})
	if err != nil {
		t.Fatalf("SerializeVarScope: %v", err)
	}
	return scope
}

// TestCheckIterationCursorValidity_StepRemoved verifies that
// checkIterationCursorValidity returns an error when the checkpoint cursor
// names a step that no longer exists in the compiled workflow graph. This
// simulates a workflow edit that deletes the iterating step between crash and resume.
func TestCheckIterationCursorValidity_StepRemoved(t *testing.T) {
	g := compileIterCursorWorkflow(t)
	scope := iterCursorScopeForStep(t, g, "execute")

	// Confirm the baseline: scope with "execute" still in the graph returns nil.
	if err := checkIterationCursorValidity(g, scope, "execute"); err != nil {
		t.Fatalf("baseline check failed unexpectedly: %v", err)
	}

	// Simulate the workflow being edited: remove "execute" from the graph.
	delete(g.Steps, "execute")

	err := checkIterationCursorValidity(g, scope, "execute")
	if err == nil {
		t.Fatal("expected error when iterating step no longer exists, got nil")
	}
	if !strings.Contains(err.Error(), "no longer exists") {
		t.Errorf("expected 'no longer exists' in error; got: %v", err)
	}
}

// TestCheckIterationCursorValidity_NoActiveIteration verifies that
// checkIterationCursorValidity returns nil when there is no in-progress
// iteration cursor in the variable scope (i.e. the run was not iterating
// when the checkpoint was taken).
func TestCheckIterationCursorValidity_NoActiveIteration(t *testing.T) {
	g := compileIterCursorWorkflow(t)
	vars := workflow.SeedVarsFromGraph(g)
	// Scope with no iter cursor — simulates a step outside any iteration.
	scope, err := workflow.SerializeVarScope(vars)
	if err != nil {
		t.Fatalf("SerializeVarScope: %v", err)
	}

	if err := checkIterationCursorValidity(g, scope, "execute"); err != nil {
		t.Errorf("expected nil for non-iteration step; got: %v", err)
	}
}
