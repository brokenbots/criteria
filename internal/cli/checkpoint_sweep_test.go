package cli

// checkpoint_sweep_test.go — CRI-202: the criteria-side retention janitor.
// Terminal runs lose their checkpoints (retention), runs with live or
// resumable metadata keep them (the STOPPED exemption — a stopped run must
// not lose its state to the janitor, or the stop feature loses its own
// state), and runs referenced by nothing are swept as orphans. The sweep
// never touches run metadata.

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/internal/runtime/state"
)

// sweepHome points CRITERIA_HOME at a scratch dir for the test and returns it.
func sweepHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CRITERIA_HOME", home)
	return home
}

// seedRunState writes a run-state file for runID with the given status.
func seedRunState(t *testing.T, home, runID, status string) {
	t.Helper()
	b, err := json.Marshal(&localRunState{
		PID:       os.Getpid(),
		RunID:     runID,
		Workflow:  "wf.hcl",
		StartedAt: time.Now(),
		Status:    status,
	})
	if err != nil {
		t.Fatalf("marshal run state: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(home, "runs", runID), 0o755); err != nil {
		t.Fatalf("mkdir runs/%s: %v", runID, err)
	}
	p, err := runStateFilePath(runID)
	if err != nil {
		t.Fatalf("run state path: %v", err)
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatalf("write run state: %v", err)
	}
}

// seedRunSnapshot writes one adapter checkpoint under the run's snapshot dir.
func seedRunSnapshot(t *testing.T, home, runID, session string) {
	t.Helper()
	store := state.NewCheckpointStore(home, runID)
	if _, err := store.Save(session, &adapterhost.SessionSnapshot{SchemaVersion: 1}); err != nil {
		t.Fatalf("save snapshot: %v", err)
	}
}

func runSnapshotDir(t *testing.T, home, runID string) string {
	t.Helper()
	return filepath.Join(home, "runs", runID, "snapshots")
}

func sweepLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func TestSweepCheckpointRun_TerminalRunDeletesCheckpoints(t *testing.T) {
	for _, status := range []string{agentRunStatusTerminal, "succeeded", "failed", "cancelled"} {
		home := sweepHome(t)
		seedRunSnapshot(t, home, "run-1", "s1")
		seedRunState(t, home, "run-1", status)

		sweepCheckpointRun(sweepLog(), home, "run-1")

		if _, err := os.Stat(runSnapshotDir(t, home, "run-1")); !os.IsNotExist(err) {
			t.Fatalf("status %q: snapshots not deleted (stat err %v)", status, err)
		}
	}
}

func TestSweepCheckpointRun_StoppedRunKeepsCheckpoints(t *testing.T) {
	home := sweepHome(t)
	seedRunSnapshot(t, home, "run-1", "s1")
	seedRunState(t, home, "run-1", "stopped")

	sweepCheckpointRun(sweepLog(), home, "run-1")

	if _, err := os.Stat(runSnapshotDir(t, home, "run-1")); err != nil {
		t.Fatalf("stopped run's snapshots must survive the janitor: %v", err)
	}
}

func TestSweepCheckpointRun_RunningRunKeepsCheckpoints(t *testing.T) {
	home := sweepHome(t)
	seedRunSnapshot(t, home, "run-1", "s1")
	seedRunState(t, home, "run-1", "")

	sweepCheckpointRun(sweepLog(), home, "run-1")

	if _, err := os.Stat(runSnapshotDir(t, home, "run-1")); err != nil {
		t.Fatalf("running run's snapshots must survive the janitor: %v", err)
	}
}

func TestSweepCheckpointRun_OrphanWithoutMetadataDeleted(t *testing.T) {
	home := sweepHome(t)
	seedRunSnapshot(t, home, "run-1", "s1")

	sweepCheckpointRun(sweepLog(), home, "run-1")

	if _, err := os.Stat(runSnapshotDir(t, home, "run-1")); !os.IsNotExist(err) {
		t.Fatalf("orphaned snapshots not deleted (stat err %v)", err)
	}
}

func TestSweepCheckpointRun_OrphanWithStepCheckpointKept(t *testing.T) {
	home := sweepHome(t)
	seedRunSnapshot(t, home, "run-1", "s1")
	// A step checkpoint without run-state: the run is resumable, so its
	// checkpoints must survive (a crash between state writes).
	cp := &StepCheckpoint{RunID: "run-1", Workflow: "wf.hcl", CurrentStep: "a", StartedAt: time.Now()}
	if err := writeStepCheckpoint(cp, nil); err != nil {
		t.Fatalf("write step checkpoint: %v", err)
	}
	t.Cleanup(func() { RemoveStepCheckpoint("run-1") })

	sweepCheckpointRun(sweepLog(), home, "run-1")

	if _, err := os.Stat(runSnapshotDir(t, home, "run-1")); err != nil {
		t.Fatalf("resumable orphan's snapshots must survive the janitor: %v", err)
	}
}

func TestSweepOrphanCheckpointState_NoRunsDirIsNoOp(t *testing.T) {
	sweepHome(t) // empty home: no runs dir at all
	sweepOrphanCheckpointState(sweepLog())
}

func TestSweepOrphanCheckpointState_SweepsAllRuns(t *testing.T) {
	home := sweepHome(t)
	seedRunSnapshot(t, home, "done-run", "s1")
	seedRunState(t, home, "done-run", "terminal")
	seedRunSnapshot(t, home, "stop-run", "s1")
	seedRunState(t, home, "stop-run", "stopped")
	seedRunSnapshot(t, home, "orphan-run", "s1")

	sweepOrphanCheckpointState(sweepLog())

	if _, err := os.Stat(runSnapshotDir(t, home, "done-run")); !os.IsNotExist(err) {
		t.Fatalf("terminal run kept snapshots (stat err %v)", err)
	}
	if _, err := os.Stat(runSnapshotDir(t, home, "stop-run")); err != nil {
		t.Fatalf("stopped run lost snapshots: %v", err)
	}
	if _, err := os.Stat(runSnapshotDir(t, home, "orphan-run")); !os.IsNotExist(err) {
		t.Fatalf("orphaned run kept snapshots (stat err %v)", err)
	}
}

func TestSweepOrphanCheckpointState_UnavailableHomeIsNoOp(t *testing.T) {
	// An unwritable CRITERIA_HOME target must not panic or log at Info.
	t.Setenv("CRITERIA_HOME", filepath.Join(t.TempDir(), "missing", "criteria"))
	sweepOrphanCheckpointState(nil) // nil log must fall back to slog.Default()
}