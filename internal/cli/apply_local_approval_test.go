package cli

import (
	"context"
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

	wf := filepath.Join("testdata", "local_approval_simple.hcl")
	err := runApply(context.Background(), applyOptions{workflowPath: wf})
	if err != nil {
		t.Fatalf("expected successful auto-approve run, got: %v", err)
	}
}

func TestApplyLocal_AutoApprove_SignalWait(t *testing.T) {
	pluginDir := filepath.Dir(buildNoopPluginBinary(t))
	stateDir := t.TempDir()
	t.Setenv("CRITERIA_PLUGINS", pluginDir)
	t.Setenv("CRITERIA_STATE_DIR", stateDir)
	t.Setenv("CRITERIA_LOCAL_APPROVAL", "auto-approve")

	wf := filepath.Join("testdata", "local_signal_wait.hcl")
	err := runApply(context.Background(), applyOptions{workflowPath: wf})
	if err != nil {
		t.Fatalf("expected successful auto-approve signal run, got: %v", err)
	}
}

func TestApplyLocal_EnvMode_ApprovalApproved(t *testing.T) {
	pluginDir := filepath.Dir(buildNoopPluginBinary(t))
	stateDir := t.TempDir()
	t.Setenv("CRITERIA_PLUGINS", pluginDir)
	t.Setenv("CRITERIA_STATE_DIR", stateDir)
	t.Setenv("CRITERIA_LOCAL_APPROVAL", "env")
	t.Setenv("CRITERIA_APPROVAL_REVIEW", "approved")

	wf := filepath.Join("testdata", "local_approval_simple.hcl")
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

	wf := filepath.Join("testdata", "local_approval_simple.hcl")
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
	t.Setenv("CRITERIA_SIGNAL_GATE", "received")

	wf := filepath.Join("testdata", "local_signal_wait.hcl")
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

	wf := filepath.Join("testdata", "local_approval_simple.hcl")
	err := runApply(context.Background(), applyOptions{workflowPath: wf})
	if err != nil {
		t.Fatalf("expected successful file-mode approved run, got: %v", err)
	}
}

func TestApplyLocal_LocalApprovalDisabled_ApprovalNodeRejected(t *testing.T) {
	// Without CRITERIA_LOCAL_APPROVAL, approval nodes must be rejected.
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())

	wf := filepath.Join("testdata", "local_approval_simple.hcl")
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

	wf := filepath.Join("testdata", "local_signal_wait.hcl")
	err := runApply(context.Background(), applyOptions{workflowPath: wf})
	if err == nil {
		t.Fatal("expected error for signal wait without CRITERIA_LOCAL_APPROVAL")
	}
	if !strings.Contains(err.Error(), "signal waits require an orchestrator") {
		t.Fatalf("unexpected error: %v", err)
	}
}
