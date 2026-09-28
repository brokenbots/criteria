package cli

import (
	"log/slog"
	"os"
	"path/filepath"

	"github.com/brokenbots/criteria/internal/runtime/state"
)

// sweepOrphanCheckpointState enforces the CRI-202 criteria-side retention
// janitor at startup. Adapter checkpoints live under
// <home>/runs/<runID>/snapshots; the janitor deletes the checkpoint subtree
// when the run is terminal (retention) and when nothing references the run
// anymore (orphans left by local terminal runs, whose run-state and step
// checkpoint were removed on completion). Runs with live metadata — running,
// paused, or otherwise resumable — are exempt: a stopped run must not lose
// its checkpoints to the janitor, or the stop feature loses its own state.
//
// The sweep never removes run metadata; it only removes the checkpoints
// themselves.
func sweepOrphanCheckpointState(log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	home, err := stateDir()
	if err != nil {
		log.Debug("skipping checkpoint sweep; criteria home unavailable", "error", err)
		return
	}
	entries, err := os.ReadDir(filepath.Join(home, "runs"))
	if err != nil {
		if !os.IsNotExist(err) {
			log.Debug("skipping checkpoint sweep; runs directory unreadable", "error", err)
		}
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		sweepCheckpointRun(log, home, e.Name())
	}
}

// deleteRunCheckpoints removes a run's adapter checkpoint subtree
// (CRI-202). Best-effort: failures surface through the next startup sweep.
func deleteRunCheckpoints(runID string) {
	home, err := stateDir()
	if err != nil {
		return
	}
	_ = state.NewCheckpointStore(home, runID).DeleteRun()
}

// sweepCheckpointRun applies the retention rules to one run's checkpoints.
func sweepCheckpointRun(log *slog.Logger, home, runID string) {
	st, err := readLocalRunState(runID)
	if err != nil {
		st = nil // unreadable metadata counts as no metadata
	}
	stepCp, err := readStepCheckpoint(runID)
	if err != nil {
		stepCp = nil
	}
	var reason string
	switch {
	case st != nil && (st.Status == agentRunStatusTerminal || isTerminalRunStatus(st.Status)):
		reason = "terminal"
	case st == nil && stepCp == nil:
		reason = "orphaned"
	default:
		// Live or resumable: the STOPPED exemption keeps these checkpoints.
		return
	}
	store := state.NewCheckpointStore(home, runID)
	if err := store.DeleteRun(); err != nil {
		log.Warn("failed to delete checkpoint state", "run_id", runID, "reason", reason, "error", err)
		return
	}
	log.Info("deleted adapter checkpoints", "run_id", runID, "reason", reason)
}
