package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLocalState_StepCheckpoint_ReadWrite(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OVERSEER_STATE_DIR", dir)
	workflowPath := filepath.Join(dir, "workflow.hcl")
	if err := os.WriteFile(workflowPath, []byte("workflow \"w\" { version = \"0.1\" }"), 0o600); err != nil {
		t.Fatal(err)
	}

	cp := &StepCheckpoint{
		RunID:        "run-abc",
		Workflow:     "my-workflow",
		WorkflowPath: workflowPath,
		CurrentStep:  "build",
		Attempt:      1,
		StartedAt:    time.Now().UTC().Truncate(time.Second),
		CastleURL:    "http://localhost:8080",
		OverseerID:   "overseer-xyz",
		Token:        "secret-token",
	}

	if err := WriteStepCheckpoint(cp); err != nil {
		t.Fatalf("WriteStepCheckpoint: %v", err)
	}

	// Verify the file exists at the expected path.
	p := filepath.Join(dir, "runs", "run-abc.json")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("checkpoint file not created: %v", err)
	}

	// Read it back via ListStepCheckpoints.
	checkpoints, err := ListStepCheckpoints()
	if err != nil {
		t.Fatalf("ListStepCheckpoints: %v", err)
	}
	if len(checkpoints) != 1 {
		t.Fatalf("expected 1 checkpoint, got %d", len(checkpoints))
	}
	got := checkpoints[0]
	if got.RunID != cp.RunID {
		t.Fatalf("run_id=%q want %q", got.RunID, cp.RunID)
	}
	if got.CurrentStep != cp.CurrentStep {
		t.Fatalf("current_step=%q want %q", got.CurrentStep, cp.CurrentStep)
	}
	if got.Token != cp.Token {
		t.Fatalf("token mismatch")
	}
	if got.OverseerID != cp.OverseerID {
		t.Fatalf("overseer_id mismatch")
	}

	// Remove and verify it's gone.
	RemoveStepCheckpoint(cp.RunID)
	checkpoints, err = ListStepCheckpoints()
	if err != nil {
		t.Fatalf("ListStepCheckpoints after remove: %v", err)
	}
	if len(checkpoints) != 0 {
		t.Fatalf("expected 0 checkpoints after remove, got %d", len(checkpoints))
	}
}

func TestLocalState_StepCheckpoint_InaccessibleWorkflowPath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OVERSEER_STATE_DIR", dir)

	cp := &StepCheckpoint{
		RunID:        "run-missing-workflow",
		WorkflowPath: filepath.Join(dir, "does-not-exist.hcl"),
	}
	if err := WriteStepCheckpoint(cp); err == nil {
		t.Fatal("expected error for inaccessible workflow path")
	}
}

func TestLocalState_StepCheckpoint_ToleratesCorruptFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OVERSEER_STATE_DIR", dir)

	runsDir := filepath.Join(dir, "runs")
	if err := os.MkdirAll(runsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Write a corrupt JSON file.
	corrupt := filepath.Join(runsDir, "run-bad.json")
	if err := os.WriteFile(corrupt, []byte("not json {{"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Write a valid file alongside it.
	valid := &StepCheckpoint{
		RunID:       "run-good",
		CurrentStep: "test",
	}
	b, _ := json.MarshalIndent(valid, "", "  ")
	if err := os.WriteFile(filepath.Join(runsDir, "run-good.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}

	checkpoints, err := ListStepCheckpoints()
	if err != nil {
		t.Fatalf("ListStepCheckpoints should not error on corrupt file: %v", err)
	}
	if len(checkpoints) != 1 {
		t.Fatalf("expected 1 valid checkpoint (corrupt skipped), got %d", len(checkpoints))
	}
	if checkpoints[0].RunID != "run-good" {
		t.Fatalf("unexpected run_id %q", checkpoints[0].RunID)
	}
}

func TestLocalState_NoStateDir_IsNoOp(t *testing.T) {
	dir := t.TempDir()
	// Point to a non-existent subdirectory.
	t.Setenv("OVERSEER_STATE_DIR", filepath.Join(dir, "nonexistent"))

	checkpoints, err := ListStepCheckpoints()
	if err != nil {
		t.Fatalf("expected no error when runs dir missing, got: %v", err)
	}
	if len(checkpoints) != 0 {
		t.Fatalf("expected 0 checkpoints, got %d", len(checkpoints))
	}
}
