package cli

import (
	"encoding/json"
	"errors"
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

// TestLocalState_LegacyCriteriaHomeSurvives verifies that an existing
// ~/.criteria directory from a prior install is used in place, so run history
// remains visible after the upgrade to CRITERIA_HOME.
func TestLocalState_LegacyCriteriaHomeSurvives(t *testing.T) {
	homeDir := t.TempDir()
	legacyRoot := filepath.Join(homeDir, ".criteria")
	runsDir := filepath.Join(legacyRoot, "runs")
	if err := os.MkdirAll(runsDir, 0o755); err != nil {
		t.Fatalf("mkdir legacy runs: %v", err)
	}

	t.Setenv("HOME", homeDir)
	t.Setenv("CRITERIA_HOME", "")
	t.Setenv("CRITERIA_STATE_DIR", "")

	cp := &StepCheckpoint{
		RunID:       "legacy-run",
		CurrentStep: "legacy-step",
	}
	if err := WriteStepCheckpoint(cp); err != nil {
		t.Fatalf("WriteStepCheckpoint: %v", err)
	}

	// Verify the checkpoint was written under the legacy root, not a new one.
	if _, err := os.Stat(filepath.Join(legacyRoot, "runs", "legacy-run.json")); err != nil {
		t.Fatalf("legacy checkpoint file not found: %v", err)
	}

	checkpoints, err := ListStepCheckpoints()
	if err != nil {
		t.Fatalf("ListStepCheckpoints: %v", err)
	}
	if len(checkpoints) != 1 {
		t.Fatalf("expected 1 checkpoint, got %d", len(checkpoints))
	}
	if checkpoints[0].RunID != "legacy-run" {
		t.Fatalf("run_id=%q want legacy-run", checkpoints[0].RunID)
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

	if _, err := os.Stat(filepath.Join(dir, "runs", "run-xyz", "run-state.json")); err != nil {
		t.Fatalf("per-run state file not created: %v", err)
	}

	got, err := readLocalRunState(st.RunID)
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

	_, err := readLocalRunState("run-missing")
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
	removeLocalRunState(st.RunID)
	// Double-remove must be a no-op.
	removeLocalRunState(st.RunID)

	if _, err := readLocalRunState(st.RunID); !os.IsNotExist(err) {
		t.Fatalf("expected state file to be removed, got err=%v", err)
	}
}

func TestLocalState_ConcurrentRunStates(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", dir)

	a := &localRunState{PID: 100, RunID: "run-a", Workflow: "wf-a", StartedAt: time.Now().UTC()}
	b := &localRunState{PID: 200, RunID: "run-b", Workflow: "wf-b", StartedAt: time.Now().UTC()}

	if err := writeLocalRunState(a); err != nil {
		t.Fatalf("writeLocalRunState a: %v", err)
	}
	if err := writeLocalRunState(b); err != nil {
		t.Fatalf("writeLocalRunState b: %v", err)
	}

	// Both files exist independently.
	if _, err := os.Stat(filepath.Join(dir, "runs", "run-a", "run-state.json")); err != nil {
		t.Fatalf("run-a state file missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "runs", "run-b", "run-state.json")); err != nil {
		t.Fatalf("run-b state file missing: %v", err)
	}

	gotA, err := readLocalRunState(a.RunID)
	if err != nil {
		t.Fatalf("readLocalRunState a: %v", err)
	}
	if gotA.PID != a.PID {
		t.Fatalf("run-a pid=%d want %d", gotA.PID, a.PID)
	}
	gotB, err := readLocalRunState(b.RunID)
	if err != nil {
		t.Fatalf("readLocalRunState b: %v", err)
	}
	if gotB.PID != b.PID {
		t.Fatalf("run-b pid=%d want %d", gotB.PID, b.PID)
	}

	states, err := ListLocalRunStates()
	if err != nil {
		t.Fatalf("ListLocalRunStates: %v", err)
	}
	if len(states) != 2 {
		t.Fatalf("expected 2 local run states, got %d", len(states))
	}

	// Removing one must not affect the other.
	removeLocalRunState(a.RunID)
	if _, err := readLocalRunState(a.RunID); !os.IsNotExist(err) {
		t.Fatalf("run-a state should be removed: %v", err)
	}
	if _, err := readLocalRunState(b.RunID); err != nil {
		t.Fatalf("run-b state should remain readable: %v", err)
	}
}

func TestLocalState_ListRunStates_SkipsInvalid(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", dir)

	runsDir := filepath.Join(dir, "runs")
	if err := os.MkdirAll(filepath.Join(runsDir, "run-valid"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(runsDir, "run-corrupt"), 0o700); err != nil {
		t.Fatal(err)
	}
	valid := &localRunState{PID: 1, RunID: "run-valid", Workflow: "w"}
	b, _ := json.MarshalIndent(valid, "", "  ")
	if err := os.WriteFile(filepath.Join(runsDir, "run-valid", "run-state.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runsDir, "run-corrupt", "run-state.json"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runsDir, "not-a-dir.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	states, err := ListLocalRunStates()
	if err != nil {
		t.Fatalf("ListLocalRunStates: %v", err)
	}
	if len(states) != 1 {
		t.Fatalf("expected 1 valid state, got %d", len(states))
	}
	if states[0].RunID != "run-valid" {
		t.Fatalf("run_id=%q want run-valid", states[0].RunID)
	}
}

func TestLocalState_WriteLocalRunState_CleansLegacyGlobal(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", dir)

	legacy := filepath.Join(dir, "criteria-state.json")
	if err := os.WriteFile(legacy, []byte(`{"run_id":"old"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	st := &localRunState{PID: 1, RunID: "run-new", Workflow: "w"}
	if err := writeLocalRunState(st); err != nil {
		t.Fatalf("writeLocalRunState: %v", err)
	}

	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("legacy criteria-state.json should be removed after per-run write, got err=%v", err)
	}
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

	stateFileInfo, err := os.Stat(filepath.Join(dir, "runs", "run-perms", "run-state.json"))
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

	runDirInfo, err := os.Stat(filepath.Join(dir, "runs", "run-perms"))
	if err != nil {
		t.Fatalf("stat per-run dir: %v", err)
	}
	if got := runDirInfo.Mode().Perm(); got != 0o700 {
		t.Errorf("per-run dir mode = %04o, want 0700", got)
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

// TestLocalState_StepCheckpoint_TornWriteSurvives injects a crash inside the
// checkpoint write window and proves the previous valid checkpoint survives:
// startup still decodes it, and a torn or empty file is never served.
//
// The publish seam fires after the temp file has been flushed but before the
// rename — exactly where a crash used to destroy the record under the old
// plain os.WriteFile implementation, whose truncate-then-write window on the
// live file left an empty or partial JSON body on disk.
func TestLocalState_StepCheckpoint_TornWriteSurvives(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("failure injection opens the temp file after close; POSIX semantics assumed")
	}

	for name, inject := range map[string]func(tmpName string) error{
		// Crash mid-way through writing the temp file: partial payload, no
		// sync, no rename.
		"mid-temp-write": func(tmpName string) error {
			f, err := os.OpenFile(tmpName, os.O_WRONLY|os.O_TRUNC, 0o600)
			if err != nil {
				return err
			}
			defer f.Close()
			if _, err := f.WriteString(`{"run_id":"run-torn","current`); err != nil {
				return err
			}
			// Abandon without completing or closing cleanly: the "crash".
			return errors.New("simulated crash mid-write")
		},
		// Crash right at the publish step, temp file already complete.
		"before-rename": func(tmpName string) error {
			return errors.New("simulated crash before rename")
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("CRITERIA_STATE_DIR", dir)
			workflowPath := filepath.Join(dir, "workflow.hcl")
			if err := os.WriteFile(workflowPath, []byte("workflow \"w\" {}"), 0o600); err != nil {
				t.Fatal(err)
			}

			previous := &StepCheckpoint{
				RunID:        "run-torn",
				Workflow:     "wf",
				WorkflowPath: workflowPath,
				CurrentStep:  "good-step",
				Attempt:      2,
				StartedAt:    time.Now().UTC().Truncate(time.Second),
			}
			if err := WriteStepCheckpoint(previous); err != nil {
				t.Fatalf("seed WriteStepCheckpoint: %v", err)
			}

			// Newer checkpoint whose write is destroyed mid-flight.
			next := *previous
			next.CurrentStep = "in-flight-step"
			next.Attempt = 3
			err := writeStepCheckpoint(&next, func(tmpName, target string) error {
				return inject(tmpName)
			})
			if err == nil {
				t.Fatal("expected injected crash to surface as an error (soft-degrade contract)")
			}

			// The previous valid checkpoint must still be the only record and
			// must decode fully — never the torn payload.
			target := filepath.Join(dir, "runs", "run-torn.json")
			raw, err := os.ReadFile(target)
			if err != nil {
				t.Fatalf("previous checkpoint file vanished after failed write: %v", err)
			}
			var decoded StepCheckpoint
			if derr := json.Unmarshal(raw, &decoded); derr != nil {
				t.Fatalf("startup cannot decode checkpoint after injected crash (torn write): %v; raw=%q", derr, raw)
			}
			if decoded.CurrentStep != "good-step" || decoded.Attempt != 2 {
				t.Fatalf("checkpoint content corrupted: got step=%q attempt=%d want step=%q attempt=2", decoded.CurrentStep, decoded.Attempt, "good-step")
			}

			// A crash mid-temp-write leaves a leftover temp file behind; it
			// must not confuse the startup scan.
			checkpoints, err := ListStepCheckpoints()
			if err != nil {
				t.Fatalf("ListStepCheckpoints: %v", err)
			}
			if len(checkpoints) != 1 || checkpoints[0].CurrentStep != "good-step" {
				t.Fatalf("startup scan disrupted by failed write: got %v", checkpoints)
			}
			if cp, err := readStepCheckpoint("run-torn"); err != nil || cp == nil || cp.CurrentStep != "good-step" {
				t.Fatalf("readStepCheckpoint after injected crash: cp=%+v err=%v", cp, err)
			}

			// The failed attempt must not leave torn content in the target
			// path itself.
			if raw, err := os.ReadFile(target); err == nil && !json.Valid(raw) {
				t.Fatalf("target path holds invalid JSON after failed write: %q", raw)
			}
		})
	}
}

// TestLocalState_StepCheckpoint_TornWriteOldWriterLosesRecord documents the
// mechanism the atomic writer protects against: a crash between the truncate
// and the write of a plain in-place save leaves the sole reattach record
// undecodable, losing crash recovery for the run.
func TestLocalState_StepCheckpoint_TornWriteOldWriterLosesRecord(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", dir)
	workflowPath := filepath.Join(dir, "workflow.hcl")
	if err := os.WriteFile(workflowPath, []byte("workflow \"w\" {}"), 0o600); err != nil {
		t.Fatal(err)
	}

	cp := &StepCheckpoint{
		RunID:        "run-old-writer",
		Workflow:     "wf",
		WorkflowPath: workflowPath,
		CurrentStep:  "good-step",
		Attempt:      1,
	}
	if err := WriteStepCheckpoint(cp); err != nil {
		t.Fatalf("seed WriteStepCheckpoint: %v", err)
	}

	// Reproduce exactly what the old os.WriteFile left on disk when the
	// process died inside its truncate-then-write window.
	target := filepath.Join(dir, "runs", "run-old-writer.json")
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"run_id":"run-old-writer","curre`); err != nil {
		t.Fatal(err)
	}
	f.Close() // <- the "crash": nothing further is ever written

	if _, err := readStepCheckpoint("run-old-writer"); err == nil {
		t.Fatal("expected undecodable checkpoint after simulated crash in the old truncate/write window")
	}
}

// TestLocalState_StepCheckpoint_ReplaceLeavesNoTempFiles verifies a successful
// atomic write publishes exactly the target file and cleans up its temp file,
// including when the target already exists.
func TestLocalState_StepCheckpoint_ReplaceLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", dir)
	workflowPath := filepath.Join(dir, "workflow.hcl")
	if err := os.WriteFile(workflowPath, []byte("workflow \"w\" {}"), 0o600); err != nil {
		t.Fatal(err)
	}

	runsDir := filepath.Join(dir, "runs")
	cp := &StepCheckpoint{
		RunID:        "run-replace",
		Workflow:     "wf",
		WorkflowPath: workflowPath,
		CurrentStep:  "step-1",
		Attempt:      1,
	}
	if err := WriteStepCheckpoint(cp); err != nil {
		t.Fatalf("first WriteStepCheckpoint: %v", err)
	}
	cp.Attempt = 2
	cp.CurrentStep = "step-2"
	if err := WriteStepCheckpoint(cp); err != nil {
		t.Fatalf("replacement WriteStepCheckpoint: %v", err)
	}

	entries, err := os.ReadDir(runsDir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 1 || names[0] != "run-replace.json" {
		t.Fatalf("expected exactly run-replace.json after atomic replace, got %v", names)
	}

	got, err := readStepCheckpoint("run-replace")
	if err != nil {
		t.Fatalf("decode replaced checkpoint: %v", err)
	}
	if got.CurrentStep != "step-2" || got.Attempt != 2 {
		t.Fatalf("replaced checkpoint content wrong: step=%q attempt=%d", got.CurrentStep, got.Attempt)
	}
}
