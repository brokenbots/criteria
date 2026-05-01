package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestLocalState_StepCheckpoint_ReadWrite(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", dir)
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
		ServerURL:    "http://localhost:8080",
		CriteriaID:   "criteria-xyz",
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
	if got.CriteriaID != cp.CriteriaID {
		t.Fatalf("criteria_id mismatch")
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
	t.Setenv("CRITERIA_STATE_DIR", dir)

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
	t.Setenv("CRITERIA_STATE_DIR", dir)

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
	t.Setenv("CRITERIA_STATE_DIR", filepath.Join(dir, "nonexistent"))

	checkpoints, err := ListStepCheckpoints()
	if err != nil {
		t.Fatalf("expected no error when runs dir missing, got: %v", err)
	}
	if len(checkpoints) != 0 {
		t.Fatalf("expected 0 checkpoints, got %d", len(checkpoints))
	}
}

func TestLocalState_WriteAndReadRunState(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", dir)

	st := &localRunState{
		PID:       12345,
		RunID:     "run-xyz",
		Workflow:  "my-workflow",
		ServerURL: "",
		StartedAt: time.Now().UTC().Truncate(time.Second),
	}
	if err := writeLocalRunState(st); err != nil {
		t.Fatalf("writeLocalRunState: %v", err)
	}

	got, err := readLocalRunState()
	if err != nil {
		t.Fatalf("readLocalRunState: %v", err)
	}
	if got.RunID != st.RunID {
		t.Fatalf("run_id=%q want %q", got.RunID, st.RunID)
	}
	if got.PID != st.PID {
		t.Fatalf("pid=%d want %d", got.PID, st.PID)
	}
	if got.Workflow != st.Workflow {
		t.Fatalf("workflow=%q want %q", got.Workflow, st.Workflow)
	}
}

func TestLocalState_ReadRunState_Missing(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", dir)

	_, err := readLocalRunState()
	if err == nil {
		t.Fatal("expected error reading missing state file")
	}
}

func TestLocalState_RemoveRunState(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", dir)

	st := &localRunState{PID: 1, RunID: "run-rm", Workflow: "w"}
	if err := writeLocalRunState(st); err != nil {
		t.Fatalf("writeLocalRunState: %v", err)
	}
	// Must not panic or error.
	removeLocalRunState()
	// Double-remove must be a no-op.
	removeLocalRunState()
}

func TestLocalState_StateDir_EnvOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", dir)

	got, err := stateDir()
	if err != nil {
		t.Fatalf("stateDir: %v", err)
	}
	if got != dir {
		t.Fatalf("stateDir=%q want %q", got, dir)
	}
}

func TestLocalState_CheckpointFilePath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", dir)

	p, err := checkpointFilePath("run-abc")
	if err != nil {
		t.Fatalf("checkpointFilePath: %v", err)
	}
	want := filepath.Join(dir, "runs", "run-abc.json")
	if p != want {
		t.Fatalf("got %q want %q", p, want)
	}
}

func TestLocalState_StepCheckpoint_NilErrors(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", dir)

	if err := WriteStepCheckpoint(nil); err == nil {
		t.Fatal("expected error for nil checkpoint")
	}
	if err := WriteStepCheckpoint(&StepCheckpoint{}); err == nil {
		t.Fatal("expected error for empty run_id")
	}
}

func TestLocalState_ListCheckpoints_SkipsDirectories(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", dir)

	runsDir := filepath.Join(dir, "runs")
	if err := os.MkdirAll(runsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A subdirectory inside runs/ must be silently skipped.
	if err := os.Mkdir(filepath.Join(runsDir, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A file with a non-.json extension must be silently skipped.
	if err := os.WriteFile(filepath.Join(runsDir, "not-json.txt"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A valid checkpoint.
	cp := &StepCheckpoint{RunID: "run-list", CurrentStep: "step1"}
	b, _ := json.MarshalIndent(cp, "", "  ")
	if err := os.WriteFile(filepath.Join(runsDir, "run-list.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}

	checkpoints, err := ListStepCheckpoints()
	if err != nil {
		t.Fatalf("ListStepCheckpoints: %v", err)
	}
	if len(checkpoints) != 1 {
		t.Fatalf("expected 1, got %d", len(checkpoints))
	}
}

// TestStateDirPerms verifies that writeLocalRunState and WriteStepCheckpoint
// create directories with mode 0o700 (operator-only) and files with 0o600.
func TestStateDirPerms(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits do not apply on Windows")
	}

	// Point CRITERIA_STATE_DIR at a subdirectory that does not yet exist so
	// that os.MkdirAll creates it fresh with the requested mode (0o700).
	dir := filepath.Join(t.TempDir(), "state")
	t.Setenv("CRITERIA_STATE_DIR", dir)

	// --- writeLocalRunState ---
	st := &localRunState{
		PID:       99,
		RunID:     "run-perms",
		Workflow:  "perm-wf",
		StartedAt: time.Now().UTC(),
	}
	if err := writeLocalRunState(st); err != nil {
		t.Fatalf("writeLocalRunState: %v", err)
	}

	stateFileInfo, err := os.Stat(filepath.Join(dir, "criteria-state.json"))
	if err != nil {
		t.Fatalf("stat state file: %v", err)
	}
	if got := stateFileInfo.Mode().Perm(); got != 0o600 {
		t.Errorf("state file mode = %04o, want 0600", got)
	}

	stateDirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat state dir: %v", err)
	}
	if got := stateDirInfo.Mode().Perm(); got != 0o700 {
		t.Errorf("state dir mode = %04o, want 0700", got)
	}

	// --- WriteStepCheckpoint ---
	workflowPath := filepath.Join(dir, "wf.hcl")
	if err := os.WriteFile(workflowPath, []byte("workflow \"w\" {}"), 0o600); err != nil {
		t.Fatal(err)
	}
	cp := &StepCheckpoint{
		RunID:        "run-perms-cp",
		Workflow:     "perm-wf",
		WorkflowPath: workflowPath,
		CurrentStep:  "step1",
		Attempt:      1,
		StartedAt:    time.Now().UTC(),
	}
	if err := WriteStepCheckpoint(cp); err != nil {
		t.Fatalf("WriteStepCheckpoint: %v", err)
	}

	runsDir := filepath.Join(dir, "runs")
	runsDirInfo, err := os.Stat(runsDir)
	if err != nil {
		t.Fatalf("stat runs dir: %v", err)
	}
	if got := runsDirInfo.Mode().Perm(); got != 0o700 {
		t.Errorf("runs dir mode = %04o, want 0700", got)
	}

	cpFileInfo, err := os.Stat(filepath.Join(runsDir, "run-perms-cp.json"))
	if err != nil {
		t.Fatalf("stat checkpoint file: %v", err)
	}
	if got := cpFileInfo.Mode().Perm(); got != 0o600 {
		t.Errorf("checkpoint file mode = %04o, want 0600", got)
	}
}

func TestLocalState_StepCheckpoint_VisitsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", dir)
	workflowPath := filepath.Join(dir, "workflow.hcl")
	if err := os.WriteFile(workflowPath, []byte("workflow \"w\" { version = \"0.1\" }"), 0o600); err != nil {
		t.Fatal(err)
	}

	cp := &StepCheckpoint{
		RunID:        "run-visits",
		Workflow:     "visit-wf",
		WorkflowPath: workflowPath,
		CurrentStep:  "work",
		Attempt:      1,
		StartedAt:    time.Now().UTC().Truncate(time.Second),
		Visits:       map[string]int{"work": 3, "prep": 1},
	}
	if err := WriteStepCheckpoint(cp); err != nil {
		t.Fatalf("WriteStepCheckpoint: %v", err)
	}

	checkpoints, err := ListStepCheckpoints()
	if err != nil {
		t.Fatalf("ListStepCheckpoints: %v", err)
	}
	if len(checkpoints) != 1 {
		t.Fatalf("expected 1 checkpoint, got %d", len(checkpoints))
	}
	got := checkpoints[0]
	if got.Visits["work"] != 3 {
		t.Errorf("Visits[work] = %d, want 3", got.Visits["work"])
	}
	if got.Visits["prep"] != 1 {
		t.Errorf("Visits[prep] = %d, want 1", got.Visits["prep"])
	}
}

func TestLocalState_StepCheckpoint_VisitsOmittedWhenEmpty(t *testing.T) {
	// A checkpoint with no Visits should not produce a "visits" key in JSON,
	// ensuring backward compatibility with pre-W07 checkpoint files.
	dir := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", dir)
	workflowPath := filepath.Join(dir, "workflow.hcl")
	if err := os.WriteFile(workflowPath, []byte("workflow \"w\" { version = \"0.1\" }"), 0o600); err != nil {
		t.Fatal(err)
	}

	cp := &StepCheckpoint{
		RunID:        "run-no-visits",
		Workflow:     "wf",
		WorkflowPath: workflowPath,
		CurrentStep:  "step1",
		Attempt:      1,
	}
	if err := WriteStepCheckpoint(cp); err != nil {
		t.Fatalf("WriteStepCheckpoint: %v", err)
	}

	p := filepath.Join(dir, "runs", "run-no-visits.json")
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read checkpoint: %v", err)
	}
	if strings.Contains(string(raw), `"visits"`) {
		t.Errorf("checkpoint JSON should not contain 'visits' key when Visits is nil/empty; got: %s", raw)
	}
}

func TestValidateNodeName(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", dir)

	validNames := []string{"review", "gate", "my-approval", "step_1", "approval.check"}
	for _, name := range validNames {
		t.Run("valid/"+name, func(t *testing.T) {
			if err := validateNodeName(name); err != nil {
				t.Errorf("validateNodeName(%q) unexpectedly failed: %v", name, err)
			}
		})
	}

	invalidNames := []string{
		"../etc/passwd",
		"../../secret",
		"node/with/slash",
		"node\\backslash",
	}
	for _, name := range invalidNames {
		t.Run("invalid/"+name, func(t *testing.T) {
			if err := validateNodeName(name); err == nil {
				t.Errorf("validateNodeName(%q) expected error, got nil", name)
			}
		})
	}
}

func TestApprovalDecisionPath_RejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", dir)

	_, err := ApprovalDecisionPath("run-1", "../etc/passwd")
	if err == nil {
		t.Error("ApprovalDecisionPath with traversal node name should return error")
	}
}

func TestApprovalRequestPath_RejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", dir)

	_, err := ApprovalRequestPath("run-1", "../../evil")
	if err == nil {
		t.Error("ApprovalRequestPath with traversal node name should return error")
	}
}
