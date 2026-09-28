// Adapter checkpoint capture and save (CRI-202).
//
// At every step boundary — and, for adapters declaring per-turn granularity,
// at every completed tool-call turn — the session manager captures the
// adapter's state blob and persists it through CheckpointSave (the engine
// wires the state home) BEFORE the step-outcome event is emitted: a step is
// only done once its checkpoint is durable. Save failures are fatal to the
// run (no step-done event without its checkpoint). Restore replays the saved
// state through SessionManager.Restore / RestoreIntoLiveSession after the
// declared schema is re-checked against the checkpoint's stamp.
package adapterhost

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"time"

	"github.com/brokenbots/criteria/workflow"
)

// stateDigestPrefix marks the algorithm of a saved-state digest (CRI-202).
const stateDigestPrefix = "sha256:"

// turnCheckpointTimeout bounds a per-turn checkpoint save. The save runs on
// its own goroutine so the reply path can never be wedged by a hung adapter
// Snapshot RPC, but it must not leak forever: after the timeout the save is
// abandoned (logged loudly) and the mandatory step-boundary save remains the
// durability point.
const turnCheckpointTimeout = 30 * time.Second

// ComputeStateDigest returns the digest stamped on every saved state blob
// ("sha256:<hex>"). The checkpoint store verifies it on restore so a
// truncated or corrupted blob fails loudly instead of replaying partial
// state.
func ComputeStateDigest(blob []byte) string {
	sum := sha256.Sum256(blob)
	return stateDigestPrefix + hex.EncodeToString(sum[:])
}

// autoCheckpoints reports the session's declared automatic checkpoint policy:
// perStep saves at every adapter-call boundary, perTurn additionally saves at
// every completed tool-call turn. An adapter with no state declaration (or
// mode none) checkpoints nothing. on-demand adapters opt out of automatic
// saves entirely — the engine saves their state only at explicit pauses.
// Unknown granularities fall back to the engine-default per-step policy.
func (m *SessionManager) autoCheckpoints(sess *Session) (perStep, perTurn bool) {
	return autoCheckpointsWithDecl(m.DeclaredState(sess.Name))
}

func autoCheckpointsWithDecl(d *workflow.StateDeclaration) (perStep, perTurn bool) {
	if d == nil {
		return false, false
	}
	switch d.Mode {
	case "", workflow.StateModeNone:
		return false, false
	}
	switch d.Granularity {
	case workflow.StateGranularityOnDemand:
		return false, false
	case workflow.StateGranularityPerTurn:
		return true, true
	default:
		return true, false
	}
}

// declaredStateLocked returns the cached state declaration for name. The
// caller must hold m.mu (registerSession stamps sessions under the lock;
// the locking DeclaredState accessor would self-deadlock there).
func (m *SessionManager) declaredStateLocked(name string) *workflow.StateDeclaration {
	if info := m.adapterInfos[name]; info != nil {
		return info.State
	}
	return nil
}

// stampStateFields records the adapter's declared checkpoint-state surface on
// a session before it is published (CRI-202).
func (m *SessionManager) stampStateFields(sess *Session, d *workflow.StateDeclaration) {
	if d == nil {
		return
	}
	sess.stateMode = d.Mode
	sess.stateSchema = d.Schema
	sess.stateGranularity = d.Granularity
}

// wireTurnCheckpoint attaches the per-turn checkpoint hook to an open
// session's permission state when the adapter declared per-turn granularity
// (CRI-202). Callers invoke it with the session's bootstrap context after the
// permission state exists; d may be nil (no declaration).
func (m *SessionManager) wireTurnCheckpoint(sess *Session, ctx context.Context, d *workflow.StateDeclaration) {
	if _, perTurn := autoCheckpointsWithDecl(d); perTurn && sess.PermissionState != nil {
		sess.PermissionState.turnCheckpoint = m.turnCheckpointFor(ctx, sess)
	}
}

// turnCheckpointFor builds the per-turn checkpoint hook for a session: an
// asynchronous, serialized save that never blocks the reply path (a hung
// adapter Snapshot RPC must not wedge the turn delivery). The save derives
// its deadline from the session's bootstrap context: once the run stops, a
// racing turn-boundary save is abandoned just like the step-boundary one.
// Failures are logged loudly, never silently swallowed; the step-boundary
// save remains the mandatory durability point.
func (m *SessionManager) turnCheckpointFor(ctx context.Context, sess *Session) func() {
	return func() {
		if !sess.ckptInFlight.CompareAndSwap(false, true) {
			return // a save is already in flight for this session
		}
		go func() {
			defer sess.ckptInFlight.Store(false)
			if sess.closing.Load() || sess.isPaused() {
				return
			}
			saveCtx, cancel := context.WithTimeout(ctx, turnCheckpointTimeout)
			defer cancel()
			if err := m.persistCheckpoint(saveCtx, sess); err != nil {
				slog.Warn("per-turn checkpoint save failed; step-boundary save remains mandatory",
					"session", sess.Name, "adapter", sess.Adapter, "error", err)
			}
		}()
	}
}

// persistCheckpoint captures and saves one checkpoint for the session,
// enforcing the adapter's declared max_bytes: a save exceeding the cap fails
// loudly and is never truncated (a truncated transcript restores as a
// plausible-looking broken session, CRI-201).
func (m *SessionManager) persistCheckpoint(ctx context.Context, sess *Session) error {
	if m.CheckpointSave == nil {
		return nil
	}
	sess.ckptMu.Lock()
	defer sess.ckptMu.Unlock()

	snap, err := sess.captureState(ctx)
	if err != nil {
		return fmt.Errorf("session %q (%s): capture checkpoint: %w", sess.Name, sess.Adapter, err)
	}
	maxBytes := m.DeclaredState(sess.Name).EffectiveMaxBytes()
	if int64(len(snap.AdapterState)) > int64(maxBytes) {
		return fmt.Errorf("session %q (%s): state blob is %d bytes, exceeding the declared max_bytes %d; refusing to truncate",
			sess.Name, sess.Adapter, len(snap.AdapterState), maxBytes)
	}
	if err := m.CheckpointSave(sess.Name, snap); err != nil {
		return fmt.Errorf("session %q (%s): save checkpoint: %w", sess.Name, sess.Adapter, err)
	}
	return nil
}

// checkpointAfterExecute persists the session's checkpoint at an adapter-call
// boundary. It runs inside SessionManager.execute's success path, before the
// engine emits the step-outcome event: a step is only done once its state is
// durable, so a save failure aborts the run (FatalRunError) instead of
// completing the step without a checkpoint. A canceled context means the run
// is stopping; no step-outcome will be emitted, so the save is skipped.
func (m *SessionManager) checkpointAfterExecute(ctx context.Context, sess *Session) error {
	select {
	case <-ctx.Done():
		return nil
	default:
	}
	perStep, _ := m.autoCheckpoints(sess)
	if !perStep {
		return nil
	}
	if err := m.persistCheckpoint(ctx, sess); err != nil {
		return fmt.Errorf("checkpointing is required before the step may complete: %w", err)
	}
	return nil
}

// validateRestoredStateSchema compares a checkpoint's stamped state schema
// with the relaunched adapter's declared schema (CRI-202). Drift in either
// direction refuses loudly: replaying mismatched bytes would misinterpret
// session state, and dropping a stateful checkpoint because the adapter no
// longer declares state would be a silent fresh start. A checkpoint without
// a schema tag (pre-CRI-202 layout) proceeds with a warning — there is no
// tag to compare, and refusing it would strand snapshots taken by earlier
// binaries.
func validateRestoredStateSchema(adapterName string, d *workflow.StateDeclaration, snap *SessionSnapshot) error {
	if snap.StateSchema == "" {
		if len(snap.AdapterState) > 0 {
			slog.Warn("checkpoint carries no state schema tag (pre-CRI-202 layout); skipping schema verification",
				"adapter", adapterName)
		}
		return nil
	}
	if d == nil || d.Mode == "" || d.Mode == workflow.StateModeNone {
		return fmt.Errorf("adapter %q checkpoint carries state (schema %q) but the adapter declares no checkpointable state; refusing to restore",
			adapterName, snap.StateSchema)
	}
	if snap.StateSchema != d.Schema {
		return fmt.Errorf("adapter %q state schema mismatch: checkpoint carries %q, adapter declares %q; refusing to restore mismatched state",
			adapterName, snap.StateSchema, d.Schema)
	}
	return nil
}
