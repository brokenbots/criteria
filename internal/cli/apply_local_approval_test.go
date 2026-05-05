package cli

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// All tests in this file exercise local-mode approval and signal-wait via
// runApply with CRITERIA_LOCAL_APPROVAL set. The noop adapter binary must be
// built once per test binary via buildNoopPluginBinary.

func TestApplyLocal_AutoApprove_ApprovalNode(t *testing.T) {
	pluginDir := filepath.Dir(buildNoopPluginBinary(t))
	stateDir := t.TempDir()
	t.Setenv("CRITERIA_PLUGINS", pluginDir)
	t.Setenv("CRITERIA_STATE_DIR", stateDir)
	t.Setenv("CRITERIA_LOCAL_APPROVAL", "auto-approve")

	var logBuf bytes.Buffer
	captLog := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	wf := filepath.Join("testdata", "local_approval_simple")
	err := runApply(context.Background(), applyOptions{workflowPath: wf, log: captLog})
	if err != nil {
		t.Fatalf("expected successful auto-approve run, got: %v", err)
	}
	if !strings.Contains(logBuf.String(), "auto-approving approval node") {
		t.Errorf("expected auto-approve warning log, got: %s", logBuf.String())
	}
	if !strings.Contains(logBuf.String(), "do not use in production") {
		t.Errorf("expected 'do not use in production' in warning log, got: %s", logBuf.String())
	}
}

func TestApplyLocal_AutoApprove_SignalWait(t *testing.T) {
	pluginDir := filepath.Dir(buildNoopPluginBinary(t))
	stateDir := t.TempDir()
	t.Setenv("CRITERIA_PLUGINS", pluginDir)
	t.Setenv("CRITERIA_STATE_DIR", stateDir)
	t.Setenv("CRITERIA_LOCAL_APPROVAL", "auto-approve")

	var logBuf bytes.Buffer
	captLog := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	wf := filepath.Join("testdata", "local_signal_wait")
	err := runApply(context.Background(), applyOptions{workflowPath: wf, log: captLog})
	if err != nil {
		t.Fatalf("expected successful auto-approve signal run, got: %v", err)
	}
	if !strings.Contains(logBuf.String(), "auto-approving signal wait node") {
		t.Errorf("expected auto-approve signal warning log, got: %s", logBuf.String())
	}
	if !strings.Contains(logBuf.String(), "do not use in production") {
		t.Errorf("expected 'do not use in production' in warning log, got: %s", logBuf.String())
	}
}

func TestApplyLocal_EnvMode_ApprovalApproved(t *testing.T) {
	pluginDir := filepath.Dir(buildNoopPluginBinary(t))
	stateDir := t.TempDir()
	t.Setenv("CRITERIA_PLUGINS", pluginDir)
	t.Setenv("CRITERIA_STATE_DIR", stateDir)
	t.Setenv("CRITERIA_LOCAL_APPROVAL", "env")
	t.Setenv("CRITERIA_APPROVAL_REVIEW", "approved")

	wf := filepath.Join("testdata", "local_approval_simple")
	err := runApply(context.Background(), applyOptions{workflowPath: wf})
	if err != nil {
		t.Fatalf("expected successful env-mode approved run, got: %v", err)
	}
}

func TestApplyLocal_EnvMode_ApprovalRejected(t *testing.T) {
	pluginDir := filepath.Dir(buildNoopPluginBinary(t))
	stateDir := t.TempDir()
	t.Setenv("CRITERIA_PLUGINS", pluginDir)
	t.Setenv("CRITERIA_STATE_DIR", stateDir)
	t.Setenv("CRITERIA_LOCAL_APPROVAL", "env")
	t.Setenv("CRITERIA_APPROVAL_REVIEW", "rejected")

	wf := filepath.Join("testdata", "local_approval_simple")
	// "rejected_state" is a valid terminal state (success=false); runApply
	// returns nil — workflow-level outcome is communicated via RunCompleted events,
	// not as a Go error.
	err := runApply(context.Background(), applyOptions{workflowPath: wf})
	if err != nil {
		t.Fatalf("rejected approval: run should complete cleanly, got: %v", err)
	}
}

func TestApplyLocal_EnvMode_SignalWait(t *testing.T) {
	pluginDir := filepath.Dir(buildNoopPluginBinary(t))
	stateDir := t.TempDir()
	t.Setenv("CRITERIA_PLUGINS", pluginDir)
	t.Setenv("CRITERIA_STATE_DIR", stateDir)
	t.Setenv("CRITERIA_LOCAL_APPROVAL", "env")
	t.Setenv("CRITERIA_SIGNAL_GATE", "success") // matches the "success" outcome in local_signal_wait.hcl

	wf := filepath.Join("testdata", "local_signal_wait")
	err := runApply(context.Background(), applyOptions{workflowPath: wf})
	if err != nil {
		t.Fatalf("expected successful env-mode signal run, got: %v", err)
	}
}

func TestApplyLocal_FileMode_ApprovalApproved(t *testing.T) {
	pluginDir := filepath.Dir(buildNoopPluginBinary(t))
	stateDir := t.TempDir()
	t.Setenv("CRITERIA_PLUGINS", pluginDir)
	t.Setenv("CRITERIA_STATE_DIR", stateDir)
	t.Setenv("CRITERIA_LOCAL_APPROVAL", "file")
	// Short timeout so the test fails fast if the goroutine doesn't deliver.
	t.Setenv("CRITERIA_LOCAL_APPROVAL_FILE_TIMEOUT", "10s")

	// Goroutine watches for the run directory, then writes the decision file.
	done := make(chan struct{})
	go func() {
		defer close(done)
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			runsDir := filepath.Join(stateDir, "runs")
			entries, err := os.ReadDir(runsDir)
			if err != nil || len(entries) == 0 {
				time.Sleep(30 * time.Millisecond)
				continue
			}
			runID := entries[0].Name()
			reqPath := filepath.Join(runsDir, runID, "approval-review.json")
			if err := os.WriteFile(reqPath, []byte(`{"decision":"approved"}`), 0o600); err != nil {
				// Dir may not exist yet; retry.
				time.Sleep(30 * time.Millisecond)
				continue
			}
			return
		}
	}()
	defer func() { <-done }()

	wf := filepath.Join("testdata", "local_approval_simple")
	err := runApply(context.Background(), applyOptions{workflowPath: wf})
	if err != nil {
		t.Fatalf("expected successful file-mode approved run, got: %v", err)
	}
}

func TestApplyLocal_LocalApprovalDisabled_ApprovalNodeRejected(t *testing.T) {
	// Without CRITERIA_LOCAL_APPROVAL, approval nodes must be rejected.
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())

	wf := filepath.Join("testdata", "local_approval_simple")
	err := runApply(context.Background(), applyOptions{workflowPath: wf})
	if err == nil {
		t.Fatal("expected error for approval node without CRITERIA_LOCAL_APPROVAL")
	}
	if !strings.Contains(err.Error(), "approval nodes require an orchestrator") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestApplyLocal_LocalApprovalDisabled_SignalWaitRejected(t *testing.T) {
	// Without CRITERIA_LOCAL_APPROVAL, wait {signal} nodes must be rejected.
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())

	wf := filepath.Join("testdata", "local_signal_wait")
	err := runApply(context.Background(), applyOptions{workflowPath: wf})
	if err == nil {
		t.Fatal("expected error for signal wait without CRITERIA_LOCAL_APPROVAL")
	}
	if !strings.Contains(err.Error(), "signal waits require an orchestrator") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestApplyLocal_StdinMode_Approved(t *testing.T) {
	pluginDir := filepath.Dir(buildNoopPluginBinary(t))
	stateDir := t.TempDir()
	t.Setenv("CRITERIA_PLUGINS", pluginDir)
	t.Setenv("CRITERIA_STATE_DIR", stateDir)
	t.Setenv("CRITERIA_LOCAL_APPROVAL", "stdin")

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	if _, err := w.WriteString("y\n"); err != nil {
		t.Fatalf("write stdin: %v", err)
	}
	w.Close()

	wf := filepath.Join("testdata", "local_approval_simple")
	if err := runApply(context.Background(), applyOptions{workflowPath: wf, stdin: r}); err != nil {
		t.Fatalf("stdin approved run: %v", err)
	}
}

func TestApplyLocal_StdinMode_Rejected(t *testing.T) {
	pluginDir := filepath.Dir(buildNoopPluginBinary(t))
	stateDir := t.TempDir()
	t.Setenv("CRITERIA_PLUGINS", pluginDir)
	t.Setenv("CRITERIA_STATE_DIR", stateDir)
	t.Setenv("CRITERIA_LOCAL_APPROVAL", "stdin")

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	if _, err := w.WriteString("n\n"); err != nil {
		t.Fatalf("write stdin: %v", err)
	}
	w.Close()

	wf := filepath.Join("testdata", "local_approval_simple")
	// "rejected_state" is a valid terminal state (success=false); engine returns nil.
	if err := runApply(context.Background(), applyOptions{workflowPath: wf, stdin: r}); err != nil {
		t.Fatalf("stdin rejected run should complete cleanly, got: %v", err)
	}
}

func TestApplyLocal_FileMode_SignalWait(t *testing.T) {
	pluginDir := filepath.Dir(buildNoopPluginBinary(t))
	stateDir := t.TempDir()
	t.Setenv("CRITERIA_PLUGINS", pluginDir)
	t.Setenv("CRITERIA_STATE_DIR", stateDir)
	t.Setenv("CRITERIA_LOCAL_APPROVAL", "file")
	t.Setenv("CRITERIA_LOCAL_APPROVAL_FILE_TIMEOUT", "10s")

	done := make(chan struct{})
	go func() {
		defer close(done)
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			runsDir := filepath.Join(stateDir, "runs")
			entries, err := os.ReadDir(runsDir)
			if err != nil || len(entries) == 0 {
				time.Sleep(30 * time.Millisecond)
				continue
			}
			runID := entries[0].Name()
			reqPath := filepath.Join(runsDir, runID, "approval-gate.json")
			if err := os.WriteFile(reqPath, []byte(`{"outcome":"success"}`), 0o600); err != nil {
				time.Sleep(30 * time.Millisecond)
				continue
			}
			return
		}
	}()
	defer func() { <-done }()

	wf := filepath.Join("testdata", "local_signal_wait")
	if err := runApply(context.Background(), applyOptions{workflowPath: wf}); err != nil {
		t.Fatalf("file-mode signal wait run: %v", err)
	}
}

func TestApplyLocal_FileMode_Timeout(t *testing.T) {
	pluginDir := filepath.Dir(buildNoopPluginBinary(t))
	stateDir := t.TempDir()
	t.Setenv("CRITERIA_PLUGINS", pluginDir)
	t.Setenv("CRITERIA_STATE_DIR", stateDir)
	t.Setenv("CRITERIA_LOCAL_APPROVAL", "file")
	// Very short timeout — no goroutine writes the file, so the run must fail.
	t.Setenv("CRITERIA_LOCAL_APPROVAL_FILE_TIMEOUT", "200ms")

	wf := filepath.Join("testdata", "local_approval_simple")
	err := runApply(context.Background(), applyOptions{workflowPath: wf})
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected 'timed out' in error, got: %v", err)
	}
}

func TestApplyLocal_MultiApproval_EnvMode(t *testing.T) {
	pluginDir := filepath.Dir(buildNoopPluginBinary(t))
	stateDir := t.TempDir()
	t.Setenv("CRITERIA_PLUGINS", pluginDir)
	t.Setenv("CRITERIA_STATE_DIR", stateDir)
	t.Setenv("CRITERIA_LOCAL_APPROVAL", "env")
	t.Setenv("CRITERIA_APPROVAL_FIRST_REVIEW", "approved")
	t.Setenv("CRITERIA_APPROVAL_SECOND_REVIEW", "approved")

	wf := filepath.Join("testdata", "local_approval_multi")
	if err := runApply(context.Background(), applyOptions{workflowPath: wf}); err != nil {
		t.Fatalf("multi-approval env-mode run: %v", err)
	}
}

func TestApplyLocal_EnvMode_SignalWait_UnknownOutcome_Error(t *testing.T) {
	pluginDir := filepath.Dir(buildNoopPluginBinary(t))
	stateDir := t.TempDir()
	t.Setenv("CRITERIA_PLUGINS", pluginDir)
	t.Setenv("CRITERIA_STATE_DIR", stateDir)
	t.Setenv("CRITERIA_LOCAL_APPROVAL", "env")
	t.Setenv("CRITERIA_SIGNAL_GATE", "bogus") // "bogus" is not declared in local_signal_wait.hcl

	wf := filepath.Join("testdata", "local_signal_wait")
	err := runApply(context.Background(), applyOptions{workflowPath: wf})
	if err == nil {
		t.Fatal("expected error for unknown signal outcome, got nil")
	}
	if !strings.Contains(err.Error(), "bogus") {
		t.Errorf("error should mention the unknown outcome 'bogus', got: %v", err)
	}
	if !strings.Contains(err.Error(), "not declared") {
		t.Errorf("error should say 'not declared', got: %v", err)
	}
}

func TestApplyLocal_StdinMode_SignalWait_UnknownOutcome_Error(t *testing.T) {
	pluginDir := filepath.Dir(buildNoopPluginBinary(t))
	stateDir := t.TempDir()
	t.Setenv("CRITERIA_PLUGINS", pluginDir)
	t.Setenv("CRITERIA_STATE_DIR", stateDir)
	t.Setenv("CRITERIA_LOCAL_APPROVAL", "stdin")

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString(`{"outcome":"bogus"}` + "\n"); err != nil {
		t.Fatalf("write stdin: %v", err)
	}
	w.Close()

	wf := filepath.Join("testdata", "local_signal_wait")
	runErr := runApply(context.Background(), applyOptions{workflowPath: wf, stdin: r})
	if runErr == nil {
		t.Fatal("expected error for unknown signal outcome, got nil")
	}
	if !strings.Contains(runErr.Error(), "bogus") {
		t.Errorf("error should mention the unknown outcome 'bogus', got: %v", runErr)
	}
}

func TestApplyLocal_FileMode_SignalWait_UnknownOutcome_Error(t *testing.T) {
	pluginDir := filepath.Dir(buildNoopPluginBinary(t))
	stateDir := t.TempDir()
	t.Setenv("CRITERIA_PLUGINS", pluginDir)
	t.Setenv("CRITERIA_STATE_DIR", stateDir)
	t.Setenv("CRITERIA_LOCAL_APPROVAL", "file")
	t.Setenv("CRITERIA_LOCAL_APPROVAL_FILE_TIMEOUT", "10s")

	done := make(chan struct{})
	go func() {
		defer close(done)
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			runsDir := filepath.Join(stateDir, "runs")
			entries, err := os.ReadDir(runsDir)
			if err != nil || len(entries) == 0 {
				time.Sleep(30 * time.Millisecond)
				continue
			}
			runID := entries[0].Name()
			reqPath := filepath.Join(runsDir, runID, "approval-gate.json")
			if err := os.WriteFile(reqPath, []byte(`{"outcome":"bogus"}`), 0o600); err != nil {
				time.Sleep(30 * time.Millisecond)
				continue
			}
			return
		}
	}()
	defer func() { <-done }()

	wf := filepath.Join("testdata", "local_signal_wait")
	runErr := runApply(context.Background(), applyOptions{workflowPath: wf})
	if runErr == nil {
		t.Fatal("expected error for unknown file signal outcome, got nil")
	}
	if !strings.Contains(runErr.Error(), "bogus") {
		t.Errorf("error should mention the unknown outcome 'bogus', got: %v", runErr)
	}
}

func TestApplyLocal_Reattach_ReusePersistedDecision(t *testing.T) {
	// Verify that when a decision is already persisted on disk, resumeOneLocalRun
	// reuses it without re-prompting. We use stdin mode with NO piped input so the
	// test would hang if the persisted decision were not found.
	pluginDir := filepath.Dir(buildNoopPluginBinary(t))
	stateDir := t.TempDir()
	t.Setenv("CRITERIA_PLUGINS", pluginDir)
	t.Setenv("CRITERIA_STATE_DIR", stateDir)
	t.Setenv("CRITERIA_LOCAL_APPROVAL", "stdin")

	wf := filepath.Join("testdata", "local_approval_simple")
	runID := "reattach-test-run-id"

	// Write a checkpoint as if the run crashed while paused at the approval node.
	cp := &StepCheckpoint{
		RunID:        runID,
		Workflow:     "local_approval_simple",
		WorkflowPath: wf,
		CurrentStep:  "review",
		Attempt:      0,
		StartedAt:    time.Now().UTC(),
	}
	if err := WriteStepCheckpoint(cp); err != nil {
		t.Fatalf("WriteStepCheckpoint: %v", err)
	}

	// Pre-write a persisted decision at the expected path.
	decisionDir := filepath.Join(stateDir, "runs", runID, "approvals")
	if err := os.MkdirAll(decisionDir, 0o700); err != nil {
		t.Fatalf("MkdirAll decision dir: %v", err)
	}
	decisionFile := filepath.Join(decisionDir, "review.json")
	decision := `{"decision":"approved","decided_at":"2024-01-01T00:00:00Z"}`
	if err := os.WriteFile(decisionFile, []byte(decision), 0o600); err != nil {
		t.Fatalf("WriteFile decision: %v", err)
	}

	// Use a timeout context: if the persisted decision is NOT found and the resumer
	// blocks on stdin, the context expires and the test fails with a clear message.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var logBuf bytes.Buffer
	captLog := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	resumeOneLocalRun(ctx, captLog, cp, io.Discard, outputModeJSON)

	logOutput := logBuf.String()
	if strings.Contains(logOutput, "resumed local run failed") {
		t.Fatalf("reattach run failed; log: %s", logOutput)
	}
	if !strings.Contains(logOutput, "resumed local run completed") {
		t.Fatalf("expected 'resumed local run completed' in logs; got: %s", logOutput)
	}
}

func TestApplyLocal_Reattach_InvalidPersistedSignalOutcome_Error(t *testing.T) {
	// Verify that a persisted signal outcome that is not declared in the workflow
	// is rejected on reattach rather than silently selecting a fallback branch.
	pluginDir := filepath.Dir(buildNoopPluginBinary(t))
	stateDir := t.TempDir()
	t.Setenv("CRITERIA_PLUGINS", pluginDir)
	t.Setenv("CRITERIA_STATE_DIR", stateDir)
	t.Setenv("CRITERIA_LOCAL_APPROVAL", "stdin") // would block if reattach doesn't fail first

	wf := filepath.Join("testdata", "local_signal_wait") // declares outcome "success" only
	runID := "reattach-bad-signal-run"

	cp := &StepCheckpoint{
		RunID:        runID,
		Workflow:     "local_signal_wait",
		WorkflowPath: wf,
		CurrentStep:  "gate",
		Attempt:      0,
		StartedAt:    time.Now().UTC(),
	}
	if err := WriteStepCheckpoint(cp); err != nil {
		t.Fatalf("WriteStepCheckpoint: %v", err)
	}

	// Pre-write a persisted signal decision with an undeclared outcome.
	decisionDir := filepath.Join(stateDir, "runs", runID, "approvals")
	if err := os.MkdirAll(decisionDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(decisionDir, "gate.json"),
		[]byte(`{"outcome":"bogus","decided_at":"2024-01-01T00:00:00Z"}`),
		0o600,
	); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var logBuf bytes.Buffer
	captLog := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	resumeOneLocalRun(ctx, captLog, cp, io.Discard, outputModeJSON)

	logOutput := logBuf.String()
	if !strings.Contains(logOutput, "resumed local run failed") {
		t.Errorf("expected 'resumed local run failed' in logs; got: %s", logOutput)
	}
	if strings.Contains(logOutput, "resumed local run completed") {
		t.Errorf("run must not complete with invalid persisted signal outcome; log: %s", logOutput)
	}
	if !strings.Contains(logOutput, "bogus") {
		t.Errorf("error log should mention the invalid outcome 'bogus'; got: %s", logOutput)
	}
}
