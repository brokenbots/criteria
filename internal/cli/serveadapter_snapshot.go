package cli

// Serve-adapter Snapshot/Restore (ADR-0008 child role, Mechanics 3): both
// verbs delegate to the child engine's REAL pause/checkpoint machinery, not
// an approximation. Snapshot is refused unless the pause machinery has landed
// a durable checkpoint boundary (Pause → ack), and captures exactly what the
// engine persists there — the boundary variable scope, visit counts, and the
// run's adapter-session checkpoints as recorded in the child run store — in a
// digest-covered envelope. Restore validates the envelope against this
// process's served workflow and parks it on the session; the NEXT Execute
// consumes it through the engine's real resume path (WithResumedVars /
// WithResumedVisits / WithResumedIter + RunFrom, with the captured session
// checkpoints re-persisted into the new child run's store so
// bootstrapSessionsForResume replays them through sessions.Restore).
//
// The host-side migration flow is: OpenSession(same config) → Restore(state)
// → Execute. CloseSession drops any parked restore with the session, so a
// restore must be re-issued after a re-open; that is fail-closed, not lossy.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	criteriav2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/zclconf/go-cty/cty"

	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/internal/engine"
	"github.com/brokenbots/criteria/internal/runtime/state"
	"github.com/brokenbots/criteria/workflow"
)

// serveAdapterSnapshotSchema and serveAdapterSnapshotVersion identify the
// envelope format carried in the v2 SnapshotResponse state blob. Both the
// envelope body and the request's schema_version must agree before Restore
// accepts a snapshot.
const (
	serveAdapterSnapshotSchema  = "criteria.serveadapter.snapshot/v1"
	serveAdapterSnapshotVersion = 1
)

// errServeSnapshotMalformed covers any state blob that fails envelope
// decoding: unknown schema, version disagreement, missing/zero digest, digest
// mismatch, invalid JSON, or an un-restorable variable scope. connectErrorStatus
// maps it to CodeInvalidArgument (the argument bytes are bad, not the state).
var errServeSnapshotMalformed = errors.New("snapshot state is malformed")

// errServeSnapshotPrecondition covers semantic refusals where the snapshot is
// well-formed but the session/run it names cannot accept it: snapshot without
// a durable pause, snapshot at a forbidden node kind, workflow digest
// mismatch, or an unresolvable paused node. connectErrorStatus maps it to
// CodeFailedPrecondition.
type errServeSnapshotPrecondition struct {
	verb   string
	reason string
}

func (e *errServeSnapshotPrecondition) Error() string {
	return fmt.Sprintf("%s: %s", e.verb, e.reason)
}

// serveAdapterSessionState holds one captured adapter-session checkpoint:
// the adapter's opaque state blob plus the SessionSnapshot metadata it was
// persisted with (CRI-202 tags ride inside the metadata). SessionSnapshot's
// AdapterState field is json:"-", so the blob travels in State and is
// re-attached on restore.
type serveAdapterSessionState struct {
	State       []byte                       `json:"state"`
	SessionMeta *adapterhost.SessionSnapshot `json:"session_meta"`
}

// serveAdapterSnapshotV1 is the snapshot envelope. Digest covers every other
// field (canonical JSON with Digest cleared), so any byte tampering or
// truncation fails restore in the envelope check instead of surfacing later
// as an engine restore failure.
type serveAdapterSnapshotV1 struct {
	Schema         string                              `json:"schema"`
	SchemaVersion  uint32                              `json:"schema_version"`
	WorkflowDigest string                              `json:"workflow_digest"`
	PausedNode     string                              `json:"paused_node"`
	Vars           string                              `json:"vars"`
	Visits         map[string]int                      `json:"visits"`
	Sessions       map[string]serveAdapterSessionState `json:"sessions"`
	CreatedAt      time.Time                           `json:"created_at"`
	Digest         string                              `json:"digest"`
}

// marshalServeAdapterSnapshot renders the envelope and stamps Digest with the
// sha256 of the digest-free encoding.
func marshalServeAdapterSnapshot(env *serveAdapterSnapshotV1) ([]byte, error) {
	digest, err := digestServeAdapterSnapshot(env)
	if err != nil {
		return nil, fmt.Errorf("envelope digest: %w", err)
	}
	env.Digest = digest
	b, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("envelope encode: %w", err)
	}
	return b, nil
}

func digestServeAdapterSnapshot(env *serveAdapterSnapshotV1) (string, error) {
	probe := *env
	probe.Digest = ""
	b, err := json.Marshal(&probe)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// unmarshalServeAdapterSnapshot decodes and verifies the envelope: schema,
// version, and content digest all must hold. Anything else fails as malformed
// (fail loud, never partially trusted).
func unmarshalServeAdapterSnapshot(b []byte) (*serveAdapterSnapshotV1, error) {
	env := &serveAdapterSnapshotV1{}
	if err := json.Unmarshal(b, env); err != nil {
		return nil, fmt.Errorf("%w: decode: %w", errServeSnapshotMalformed, err)
	}
	if env.Schema != serveAdapterSnapshotSchema {
		return nil, fmt.Errorf("%w: schema %q, want %q", errServeSnapshotMalformed, env.Schema, serveAdapterSnapshotSchema)
	}
	if env.SchemaVersion != serveAdapterSnapshotVersion {
		return nil, fmt.Errorf("%w: schema_version %d, want %d", errServeSnapshotMalformed, env.SchemaVersion, serveAdapterSnapshotVersion)
	}
	if env.Digest == "" {
		return nil, fmt.Errorf("%w: missing content digest", errServeSnapshotMalformed)
	}
	want, err := digestServeAdapterSnapshot(env)
	if err != nil {
		return nil, fmt.Errorf("%w: envelope digest: %w", errServeSnapshotMalformed, err)
	}
	if want != env.Digest {
		return nil, fmt.Errorf("%w: content digest mismatch (envelope is truncated or corrupted)", errServeSnapshotMalformed)
	}
	return env, nil
}

// Snapshot captures the child run's checkpoint-boundary state: boundary
// variable scope, visit counts, and the run's real adapter-session checkpoints
// from the child run store. Because the scope and the checkpoints are read
// only after Pause's ack (the durable point of the real machinery), the bytes
// describe exactly the state a local-mode resume restarts from.
func (c *serveAdapterClient) Snapshot(_ context.Context, req *criteriav2.SnapshotRequest) (*criteriav2.SnapshotResponse, error) {
	run, err := c.sessionRun(req.GetSessionId(), "snapshot")
	if err != nil {
		return nil, connectErrorStatus(err)
	}
	if run == nil {
		return nil, connectErrorStatus(&errServeSnapshotPrecondition{
			verb:   "snapshot",
			reason: "no in-flight child run to snapshot (snapshot requires a paused child run)",
		})
	}
	run.mu.Lock()
	ctrl := run.ctrl
	run.mu.Unlock()
	if ctrl == nil {
		return nil, connectErrorStatus(errChildRunStillStarting)
	}
	if !ctrl.tracker.IsPaused() || ctrl.tracker.PausedAt() == "" {
		return nil, connectErrorStatus(&errServeSnapshotPrecondition{
			verb:   "snapshot",
			reason: "child run must be paused at a checkpoint boundary before Snapshot (Pause first)",
		})
	}

	env, err := c.captureServeAdapterSnapshot(run, ctrl)
	if err != nil {
		return nil, connectErrorStatus(err)
	}
	b, err := marshalServeAdapterSnapshot(env)
	if err != nil {
		return nil, connectErrorStatus(err)
	}
	return &criteriav2.SnapshotResponse{
		State:         b,
		SchemaVersion: serveAdapterSnapshotVersion,
	}, nil
}

// captureServeAdapterSnapshot gathers the envelope body: the scope/visits
// exports the engine's pause boundary persisted (VarScope/VisitCounts, W05/
// W07), and the per-session checkpoints the pause machinery wrote into the
// child run store. Serialization failures fail closed (refuse to snapshot)
// rather than shipping a lossy envelope.
func (c *serveAdapterClient) captureServeAdapterSnapshot(run *serveAdapterRun, ctrl *localRunControl) (*serveAdapterSnapshotV1, error) {
	if node := ctrl.tracker.PausedAt(); isApprovalOrSignalNode(ctrl.graph, node) {
		return nil, &errServeSnapshotPrecondition{
			verb:   "snapshot",
			reason: fmt.Sprintf("paused node %q is a wait/approval node; those kinds are rejected in serve-adapter mode", node),
		}
	}
	ctrl.mu.Lock()
	eng := ctrl.eng
	ctrl.mu.Unlock()
	if eng == nil {
		return nil, &errServeSnapshotPrecondition{verb: "snapshot", reason: "child run engine is not installed"}
	}
	vars, err := workflow.SerializeVarScope(eng.VarScope())
	if err != nil {
		return nil, fmt.Errorf("%w: serialize variable scope: %w", errServeSnapshotMalformed, err)
	}

	home, err := stateDir()
	if err != nil {
		return nil, fmt.Errorf("resolve criteria home for snapshot: %w", err)
	}
	store := state.NewCheckpointStore(home, run.id)
	sessions := map[string]serveAdapterSessionState{}
	sids, err := store.Sessions()
	if err != nil {
		return nil, fmt.Errorf("list child run checkpoints: %w", err)
	}
	for _, sid := range sids {
		snap, err := store.Latest(sid)
		if err != nil {
			return nil, fmt.Errorf("read child run checkpoint for session %q: %w", sid, err)
		}
		blob := snap.AdapterState
		snap.AdapterState = nil
		sessions[sid] = serveAdapterSessionState{
			State:       blob,
			SessionMeta: snap,
		}
	}
	return &serveAdapterSnapshotV1{
		Schema:         serveAdapterSnapshotSchema,
		SchemaVersion:  serveAdapterSnapshotVersion,
		WorkflowDigest: c.digest,
		PausedNode:     ctrl.tracker.PausedAt(),
		Vars:           vars,
		Visits:         run.engineVisits(),
		Sessions:       sessions,
		CreatedAt:      time.Now().UTC(),
	}, nil
}

// Restore validates a captured envelope against this process's served
// workflow and parks it on the session for the next Execute. Validation is
// fail-closed in order: malformed bytes, workflow digest mismatch (state
// belongs to another workflow), in-flight run (cancel first, matching the
// one-run doctrine), then paused-node resolution. The parked restore is
// consumed exactly once by openChildRun.
func (c *serveAdapterClient) Restore(_ context.Context, req *criteriav2.RestoreRequest) (*criteriav2.RestoreResponse, error) {
	if req.GetSchemaVersion() != serveAdapterSnapshotVersion {
		return nil, connectErrorStatus(fmt.Errorf("%w: request schema_version %d, want %d", errServeSnapshotMalformed, req.GetSchemaVersion(), serveAdapterSnapshotVersion))
	}
	run, err := c.sessionRun(req.GetSessionId(), "restore")
	if err != nil {
		return nil, connectErrorStatus(err)
	}
	if run != nil {
		return nil, connectErrorStatus(&ErrChildRunInFlight{RunID: run.id, Workflow: c.graph.Name})
	}
	env, err := unmarshalServeAdapterSnapshot(req.GetState())
	if err != nil {
		return nil, connectErrorStatus(err)
	}
	if env.WorkflowDigest != c.digest {
		return nil, connectErrorStatus(&errServeSnapshotPrecondition{
			verb:   "restore",
			reason: fmt.Sprintf("snapshot was captured for workflow digest %q; this session serves %q", env.WorkflowDigest, c.digest),
		})
	}
	if env.PausedNode == "" || !c.serveAdapterNodeResolvable(env.PausedNode) {
		return nil, connectErrorStatus(&errServeSnapshotPrecondition{
			verb:   "restore",
			reason: fmt.Sprintf("paused node %q does not resolve in the served workflow %q", env.PausedNode, c.graph.Name),
		})
	}
	if _, _, err := restoreRunScope(env.Vars, c.graph); err != nil {
		return nil, connectErrorStatus(fmt.Errorf("%w: %w", errServeSnapshotMalformed, err))
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	sess, ok := c.sessions[req.GetSessionId()]
	if !ok {
		// sessionRun passed a moment ago; re-check under the stash lock so a
		// concurrent CloseSession cannot park a restore on a closed session.
		return nil, connectErrorStatus(errSessionUnknownConnect)
	}
	sess.pendingRestore = env
	return &criteriav2.RestoreResponse{}, nil
}

// applyChildRunRestore re-persists the captured adapter-session checkpoints
// into the new child run's store and returns the engine options that restart
// the run from the snapshot's paused node. Session state compatibility (CRI-202
// schema/digest verification, adapter digest and host-arch checks) is enforced
// by the engine's bootstrapSessionsForResume through sessions.Restore — this
// path never re-implements it.
func (c *serveAdapterClient) applyChildRunRestore(run *serveAdapterRun) ([]engine.Option, error) {
	env := run.restore
	vars, iter, err := restoreRunScope(env.Vars, c.graph)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errServeSnapshotMalformed, err)
	}
	home, err := stateDir()
	if err != nil {
		return nil, fmt.Errorf("resolve criteria home for restore: %w", err)
	}
	for sid, captured := range env.Sessions {
		if captured.SessionMeta == nil {
			return nil, fmt.Errorf("%w: session %q checkpoint metadata missing", errServeSnapshotMalformed, sid)
		}
		snap := *captured.SessionMeta
		snap.AdapterState = captured.State
		dir := state.SnapshotDir(home, run.id, sid)
		if _, err := state.WriteSnapshot(dir, &snap); err != nil {
			return nil, fmt.Errorf("re-persist checkpoint for session %q into run %q: %w", sid, run.id, err)
		}
	}
	opts := []engine.Option{
		engine.WithResumedVars(vars),
		engine.WithResumedVisits(env.Visits),
	}
	if len(iter) > 0 {
		opts = append(opts, engine.WithResumedIter(iter))
	}
	return opts, nil
}

// restoreRunScope rebuilds a run's variable scope and iteration cursor stack
// from a serialized scope blob (the exact blob engine VarScope serializes at
// a pause boundary). workflow.RestoreVarScope rehydrates step outputs and the
// iteration stack; the "var" section is re-applied through
// workflow.ApplyVarOverrides — the same typed-conversion path the CLI uses for
// --var overrides — so variables absent from the blob fall back to graph
// defaults and captured values are coerced to their declared types.
func restoreRunScope(scopeJSON string, g *workflow.FSMGraph) (map[string]cty.Value, []workflow.IterCursor, error) {
	vars, iter, err := workflow.RestoreVarScope(scopeJSON, g)
	if err != nil {
		return nil, nil, fmt.Errorf("restore variable scope: %w", err)
	}
	var raw struct {
		Var map[string]string `json:"var"`
	}
	if err := json.Unmarshal([]byte(scopeJSON), &raw); err != nil {
		return nil, nil, fmt.Errorf("restore variable scope: %w", err)
	}
	overrides := make(map[string]cty.Value, len(raw.Var))
	for k, v := range raw.Var {
		overrides[k] = cty.StringVal(v)
	}
	merged, err := workflow.ApplyVarOverrides(g, vars, overrides)
	if err != nil {
		return nil, nil, fmt.Errorf("restore variables: %w", err)
	}
	return merged, iter, nil
}

// serveAdapterNodeResolvable reports whether node names a node kind a
// serve-adapter run can be resumed from: steps, terminal/signal-bearing
// states, and switch nodes. Wait/approval nodes stay refused — they are
// compile-rejected in this mode, and a snapshot can only name one if the
// envelope was tampered with.
func (c *serveAdapterClient) serveAdapterNodeResolvable(node string) bool {
	if isApprovalOrSignalNode(c.graph, node) {
		return false
	}
	if _, ok := c.graph.Steps[node]; ok {
		return true
	}
	if _, ok := c.graph.States[node]; ok {
		return true
	}
	_, ok := c.graph.Switches[node]
	return ok
}
