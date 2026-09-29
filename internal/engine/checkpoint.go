// Engine-side checkpoint wiring (CRI-202): the save callback to the state
// home, the restore pass at resume initialization, and the criteria-side
// retention janitor for terminal runs.
package engine

import (
	"fmt"
	"log/slog"

	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/internal/runtime/state"
	"github.com/brokenbots/criteria/workflow"
)

// wireCheckpointStore creates the run's checkpoint store and wires the save
// callback into the session manager (CRI-202). Both a snapshot base and a
// run id are required; without either, checkpointing stays disabled and
// sessions skip saves. sink is the run's engine sink (already redaction-
// wrapped by Run/RunFrom); every durable save emits its checkpoint pointer
// through it (CRI-203) so consumers track engine-local checkpoints without
// ever being their restore path.
func (e *Engine) wireCheckpointStore(sessions *adapterhost.SessionManager, sink Sink) {
	if e.snapshotBase == "" || e.runID == "" {
		return
	}
	store := state.NewCheckpointStore(e.snapshotBase, e.runID)
	sessions.CheckpointSave = func(name string, snap *adapterhost.SessionSnapshot) error {
		seq, err := store.Save(name, snap)
		if err != nil {
			return err
		}
		e.emitCheckpointPointer(sink, name, snap, seq)
		return nil
	}
}

// saveSessionCheckpoint persists one pause-path checkpoint and emits its
// pointer event (CRI-203). The explicit-pause save is an on-demand adapter's
// only save, so its pointer joins the step/turn-boundary ones.
func (e *Engine) saveSessionCheckpoint(name string, snap *adapterhost.SessionSnapshot) error {
	store := state.NewCheckpointStore(e.snapshotBase, e.runID)
	seq, err := store.Save(name, snap)
	if err != nil {
		return err
	}
	e.emitCheckpointPointer(e.runSink, name, snap, seq)
	return nil
}

// emitCheckpointPointer publishes the advisory checkpoint pointer for a
// durable save (CRI-203). Emission is fire-and-forget: the pointer is
// advisory metadata and must never affect the save outcome. Adapter identity
// resolves from the workflow graph (checkpoint session keys are graph adapter
// keys "<type>.<name>"); sessions without a graph entry — e.g. directly bound
// test fixtures — emit with empty adapter identity fields.
func (e *Engine) emitCheckpointPointer(sink Sink, sessionID string, snap *adapterhost.SessionSnapshot, seq int) {
	if sink == nil {
		return
	}
	ptr := &CheckpointPointerEvent{
		RunID:       e.runID,
		SessionID:   sessionID,
		StateID:     fmt.Sprintf("%s/%010d", sessionID, seq),
		StateSchema: snap.StateSchema,
		StateDigest: snap.StateDigest,
		StateSize:   int64(len(snap.AdapterState)),
		Granularity: snap.Granularity,
	}
	if ad := e.graphAdapter(sessionID); ad != nil {
		ptr.AdapterKind = ad.Type
		ptr.AdapterName = ad.Name
	}
	sink.OnCheckpointPointer(ptr)
}

// graphAdapter resolves a checkpoint session key to the workflow's adapter
// declaration. Session keys are graph adapter keys ("copilot.exec" style);
// nil when the engine has no graph or the session is unknown to it.
func (e *Engine) graphAdapter(sessionID string) *workflow.AdapterNode {
	if e.graph == nil {
		return nil
	}
	return e.graph.Adapters[sessionID]
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
