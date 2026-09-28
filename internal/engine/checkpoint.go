// Engine-side checkpoint wiring (CRI-202): the save callback to the state
// home, the restore pass at resume initialization, and the criteria-side
// retention janitor for terminal runs.
package engine

import (
	"log/slog"

	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/internal/runtime/state"
)

// wireCheckpointStore creates the run's checkpoint store and wires the save
// callback into the session manager (CRI-202). Both a snapshot base and a
// run id are required; without either, checkpointing stays disabled and
// sessions skip saves.
func (e *Engine) wireCheckpointStore(sessions *adapterhost.SessionManager) {
	if e.snapshotBase == "" || e.runID == "" {
		return
	}
	store := state.NewCheckpointStore(e.snapshotBase, e.runID)
	sessions.CheckpointSave = func(name string, snap *adapterhost.SessionSnapshot) error {
		_, err := store.Save(name, snap)
		return err
	}
}

// discardRunCheckpoints removes this run's adapter checkpoints (CRI-202
// retention). The janitor is criteria-side because the control plane cannot
// delete criteria-owned state: the engine calls it only for terminal runs —
// stopped/paused runs are exempt so they cannot lose their own checkpoints
// to the retention sweep. Best-effort: a cleanup failure is logged, never
// surfaced as a run failure, since the run outcome is already recorded.
func (e *Engine) discardRunCheckpoints(reason string) {
	if e.snapshotBase == "" || e.runID == "" {
		return
	}
	store := state.NewCheckpointStore(e.snapshotBase, e.runID)
	if err := store.DeleteRun(); err != nil {
		slog.Warn("checkpoint retention cleanup failed", "run_id", e.runID, "reason", reason, "error", err)
	}
}