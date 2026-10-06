package cli

// Driver tests for the serve-adapter Snapshot/Restore verbs (KB-94,
// mechanics 3): the verbs delegate to the engine's real pause/checkpoint
// machinery (SerializeVarScope/RestoreVarScope + the runtime/state checkpoint
// store + RunFrom), so the tests assert the round trip through that machinery
// — a resumed run replays the captured scope and visit counts and enters
// through the real resume path (step_resumed), never re-executing nodes that
// already ran.

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	criteriav2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"

	"github.com/brokenbots/criteria/internal/adapterhost"

	"connectrpc.com/connect"
)

// pauseAndCaptureSnapshot drives an Execute to the deterministic mid-run
// window (the noop stage sleeps 4s), pauses the run, and captures a snapshot
// envelope. Pause acks only after durable state, so Snapshot succeeds
// immediately afterwards.
func pauseAndCaptureSnapshot(t *testing.T, env *serveAdapterTestEnv, sessionID string) (*criteriav2.SnapshotResponse, *serveAdapterSnapshotV1) {
	t.Helper()
	cap2 := &executeCapture{arrived: make(chan struct{}, 8), watch: slowStepStarted()}
	exErr := make(chan error, 1)
	go func() {
		exErr <- env.client.Execute(context.Background(), &criteriav2.ExecuteRequest{
			SessionId: sessionID,
			StepName:  "warmup",
		}, cap2)
	}()
	select {
	case <-cap2.arrived:
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for the slow step to start")
	}
	if _, err := env.client.Pause(context.Background(), &criteriav2.PauseRequest{SessionId: sessionID}); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	resp, err := env.client.Snapshot(context.Background(), &criteriav2.SnapshotRequest{SessionId: sessionID})
	if err != nil {
		t.Fatalf("Snapshot after Pause: %v", err)
	}
	envv, err := unmarshalServeAdapterSnapshot(resp.GetState())
	if err != nil {
		t.Fatalf("decoding Snapshot state: %v", err)
	}
	// Drain the paused run before the test ends: a run parked at the pause
	// boundary holds its control goroutines (localRunControl await +
	// event pump) open, which goleak reports. Resume and wait for the
	// terminal on the run that was captured.
	if _, err := env.client.Resume(context.Background(), &criteriav2.ResumeRequest{SessionId: sessionID}); err != nil {
		t.Fatalf("drain resume: %v", err)
	}
	select {
	case err := <-exErr:
		if err != nil {
			t.Fatalf("drain execute: %v", err)
		}
	case <-time.After(40 * time.Second):
		t.Fatal("drain execute did not return after resume")
	}
	if res := cap2.terminal(); res == nil || res.GetOutcome() != serveAdapterOutcomeSuccess {
		t.Fatalf("drain terminal = %+v, want success", res)
	}
	return resp, envv
}

// TestServeAdapter_SnapshotRestoreRoundTripResumesFromSnapshot covers the
// mechanics-3 delegation contract end to end: a paused child run captures its
// real engine state (var scope, visit counts, paused node) into the envelope,
// a fresh serve instance's session restores those state bytes, and the
// restored run enters through the engine's resume path — no re-execution of
// nodes that already ran — and lands on the same projected output.
func TestServeAdapter_SnapshotRestoreRoundTripResumesFromSnapshot(t *testing.T) {
	env := newServeAdapterEnv(t)
	sessionID := "sess-snap"
	env.openSessionWithConfig(t, env.client, sessionID, map[string]string{"label": "restored-check"})

	resp, envv := pauseAndCaptureSnapshot(t, env, sessionID)

	if envv.Schema != serveAdapterSnapshotSchema || envv.SchemaVersion != serveAdapterSnapshotVersion {
		t.Errorf("schema/version = %q/%d, want %q/%d", envv.Schema, envv.SchemaVersion, serveAdapterSnapshotSchema, serveAdapterSnapshotVersion)
	}
	if envv.WorkflowDigest != env.digest {
		t.Errorf("WorkflowDigest = %q, want %q", envv.WorkflowDigest, env.digest)
	}
	if envv.PausedNode == "" {
		t.Error("PausedNode empty; want the node the engine parked on")
	}
	if !strings.Contains(envv.Vars, "restored-check") {
		t.Errorf("Vars %q lost the seeded variable value", envv.Vars)
	}
	if len(envv.Visits) == 0 {
		t.Error("Visits empty; want the pre-pause visit counts")
	}
	// The noop fixture publishes no adapter session checkpoints, so the
	// sessions map is truthfully empty here; the checkpoint-bytes leg is
	// covered by the codec test below.
	if len(envv.Sessions) != 0 {
		t.Errorf("Sessions = %d entries, want 0 for the checkpoint-less fixture", len(envv.Sessions))
	}
	if v := resp.GetSchemaVersion(); v != serveAdapterSnapshotVersion {
		t.Errorf("SnapshotResponse schema_version = %d, want %d", v, serveAdapterSnapshotVersion)
	}

	other := env.secondClient(t)
	env.openSessionWithConfig(t, other, sessionID, nil)
	if _, err := other.Restore(context.Background(), &criteriav2.RestoreRequest{
		SessionId:     sessionID,
		SchemaVersion: serveAdapterSnapshotVersion,
		State:         resp.GetState(),
	}); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	capB := &executeCapture{arrived: make(chan struct{}, 8), watch: slowStepStarted()}
	if err := other.Execute(context.Background(), &criteriav2.ExecuteRequest{
		SessionId: sessionID,
		StepName:  "warmup",
	}, capB); err != nil {
		t.Fatalf("Execute after restore: %v", err)
	}
	res := capB.terminal()
	if res == nil || res.GetOutcome() != serveAdapterOutcomeSuccess {
		t.Fatalf("terminal after restore = %+v, want success", res)
	}

	// The restored run replays the captured variable scope into the output
	// projection instead of the session config's fresh seed.
	var outputs map[string]string
	if err := json.Unmarshal(res.GetOutputsJson(), &outputs); err != nil {
		t.Fatalf("outputs_json decode: %v (raw %q)", err, string(res.GetOutputsJson()))
	}
	if outputs["final"] != "restored-check" {
		t.Fatalf("output final = %q, want \"restored-check\" carried by the snapshot", outputs["final"])
	}

	// The engine entered through the real resume path: run_resumed present,
	// no run_paused (the restored run starts paused-at-node), and no
	// step_started for warmup — nodes ahead of the pause boundary may run
	// again, but completed ones must not.
	kinds := capB.adapterEventKinds()
	var sawPaused, sawResumed bool
	ranWarmup := false
	for _, ev := range capB.snapshot() {
		a := ev.GetAdapter()
		if a == nil {
			continue
		}
		switch a.GetEventKind() {
		case serveAdapterEventRunPaused:
			sawPaused = true
		case serveAdapterEventRunResumed:
			sawResumed = true
		case serveAdapterEventStepStarted:
			step, _ := a.GetPayload().AsMap()["step"].(string)
			if step == "warmup" {
				ranWarmup = true
			}
		}
	}
	if sawPaused {
		t.Errorf("restored run emitted run_paused (kinds %v); want a resume, not a new pause", kinds)
	}
	if !sawResumed {
		t.Errorf("restored run lacks the resume event (kinds %v); want the engine's run.resumed path", kinds)
	}
	if ranWarmup {
		t.Errorf("restored run re-executed 'warmup' (kinds %v); want the engine to start from the paused node", kinds)
	}
}

// TestServeAdapter_SnapshotEnvelopeCodec covers the envelope wire codec: the
// digest covers the whole envelope and re-attaches opaque adapter-session
// state bytes (kept out of SessionSnapshot by its json:"-"), any byte
// corruption, digest-staleness, unknown schema, or version bump is rejected.
func TestServeAdapter_SnapshotEnvelopeCodec(t *testing.T) {
	meta := adapterhost.SessionSnapshot{
		SchemaVersion:   1,
		AdapterDigest:   "sha256:abc",
		HostArch:        "linux/amd64",
		WorkingDir:      "/tmp/fixture",
		StateSchema:     "state.schema/v1",
		StateMode:       "blob",
		StateDigest:     "sha256:def",
		Granularity:     "per-step",
		ScopeInstanceID: "scope-1",
		CreatedAt:       time.Now().UTC().Round(0),
	}
	env := &serveAdapterSnapshotV1{
		Schema:         serveAdapterSnapshotSchema,
		SchemaVersion:  serveAdapterSnapshotVersion,
		WorkflowDigest: "sha256:aa",
		PausedNode:     "slow",
		Vars:           `{"label":cty.StringVal("restored-check")}`,
		Visits:         map[string]int{"warmup": 1},
		Sessions: map[string]serveAdapterSessionState{
			"sess-a": {State: []byte("blob-1"), SessionMeta: cloneSnapshotMeta(meta)},
			"sess-b": {State: []byte("blob-2"), SessionMeta: cloneSnapshotMeta(meta)},
		},
		CreatedAt: time.Now().UTC().Round(0),
	}
	blob, err := marshalServeAdapterSnapshot(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got, err := unmarshalServeAdapterSnapshot(blob)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(got, env) {
		t.Errorf("round trip mismatch:\n got %+v\nwant %+v", got, env)
	}
	if got.Sessions["sess-a"].State == nil || string(got.Sessions["sess-a"].State) != "blob-1" {
		t.Errorf("adapter state bytes for sess-a = %q, want \"blob-1\"", got.Sessions["sess-a"].State)
	}

	// A flipped structural byte invalidates the envelope — rejected. (A flip
	// inside a JSON *key name* is not a corruption signal: encoding/json
	// matches field names case-insensitively, so it decodes to the same
	// struct; the digest covers the decoded content, not raw bytes.)
	corrupted := append([]byte(nil), blob...)
	corrupted[0] ^= 0x20
	if _, err := unmarshalServeAdapterSnapshot(corrupted); !errors.Is(err, errServeSnapshotMalformed) {
		t.Errorf("unmarshal(corrupted) = %v (%T), want malformed", err, err)
	}

	// A mutated envelope without a recomputed digest fails the digest check.
	env.Visits["warmup"] = 2
	stale, _ := json.Marshal(env)
	assertMalformed(t, stale)

	// Unknown schema string, with a recomputed digest, is rejected by the
	// schema check — not by accident of the digest.
	badSchema := cloneSnapshotEnvelope(env)
	badSchema.Schema = "other.schema/v1"
	assertMalformed(t, mustMarshal(t, badSchema))

	// A future schema_version is rejected.
	badVersion := cloneSnapshotEnvelope(env)
	badVersion.SchemaVersion = 2
	assertMalformed(t, mustMarshal(t, badVersion))
}

func assertMalformed(t *testing.T, blob []byte) {
	t.Helper()
	if _, err := unmarshalServeAdapterSnapshot(blob); !errors.Is(err, errServeSnapshotMalformed) {
		t.Errorf("unmarshal = %v (%T), want malformed", err, err)
	}
}

func mustMarshal(t *testing.T, env *serveAdapterSnapshotV1) []byte {
	t.Helper()
	blob, err := marshalServeAdapterSnapshot(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return blob
}

func cloneSnapshotMeta(meta adapterhost.SessionSnapshot) *adapterhost.SessionSnapshot {
	cp := meta
	return &cp
}

func cloneSnapshotEnvelope(env *serveAdapterSnapshotV1) *serveAdapterSnapshotV1 {
	clone := *env
	clone.Sessions = map[string]serveAdapterSessionState{}
	for k, v := range env.Sessions {
		clone.Sessions[k] = v
	}
	return &clone
}

// TestServeAdapter_SnapshotRestoreFailClosed covers the verb-level refusal
// battery: snapshot requires a paused run; restore refuses unknown sessions,
// restore while in-flight, malformed bytes, wrong schema/version, foreign
// workflow digests, and paused nodes that don't resolve in the graph.
func TestServeAdapter_SnapshotRestoreFailClosed(t *testing.T) {
	env := newServeAdapterEnv(t)
	sessionID := "sess-fc"

	// Restore against an unknown session → not found.
	if _, err := env.client.Restore(context.Background(), &criteriav2.RestoreRequest{
		SessionId:     "sess-ghost",
		SchemaVersion: serveAdapterSnapshotVersion,
		State:         []byte("{}"),
	}); err == nil {
		t.Fatal("Restore for an unknown session succeeded; want not-found")
	} else if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("unknown-session restore code = %q, want not_found (%v)", connect.CodeOf(err), err)
	}

	// Snapshot without a paused run (session exists, no run at all) →
	// failed precondition.
	env.openSessionWithConfig(t, env.client, sessionID, nil)
	if _, err := env.client.Snapshot(context.Background(), &criteriav2.SnapshotRequest{SessionId: sessionID}); err == nil {
		t.Fatal("Snapshot with no child run succeeded; want failed precondition")
	} else if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("no-run snapshot code = %q, want failed_precondition (%v)", connect.CodeOf(err), err)
	}

	// Snapshot while the run is live but not yet paused → failed precondition
	// pointing at Pause.
	cap2 := &executeCapture{arrived: make(chan struct{}, 8), watch: slowStepStarted()}
	exErr := make(chan error, 1)
	go func() {
		exErr <- env.client.Execute(context.Background(), &criteriav2.ExecuteRequest{
			SessionId: sessionID,
			StepName:  "warmup",
		}, cap2)
	}()
	select {
	case <-cap2.arrived:
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for the slow step to start")
	}
	_, snapErr := env.client.Snapshot(context.Background(), &criteriav2.SnapshotRequest{SessionId: sessionID})
	if snapErr == nil {
		t.Fatal("Snapshot on a running (not paused) child run succeeded; want failed precondition")
	} else if connect.CodeOf(snapErr) != connect.CodeFailedPrecondition || !strings.Contains(snapErr.Error(), "Pause first") {
		t.Errorf("running snapshot code = %q, err %v, want failed_precondition with a pause hint", connect.CodeOf(snapErr), snapErr)
	}

	if _, err := env.client.Pause(context.Background(), &criteriav2.PauseRequest{SessionId: sessionID}); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	resp, err := env.client.Snapshot(context.Background(), &criteriav2.SnapshotRequest{SessionId: sessionID})
	if err != nil {
		t.Fatalf("Snapshot after Pause: %v", err)
	}

	// Restore while the capturing run is still parked → failed precondition
	// naming the in-flight run.
	if _, err := env.client.Restore(context.Background(), &criteriav2.RestoreRequest{
		SessionId:     sessionID,
		SchemaVersion: serveAdapterSnapshotVersion,
		State:         resp.GetState(),
	}); err == nil {
		t.Fatal("Restore while in-flight succeeded; want failed precondition")
	} else {
		var typed *ErrChildRunInFlight
		if !errors.As(err, &typed) || connect.CodeOf(err) != connect.CodeFailedPrecondition {
			t.Errorf("in-flight restore err = %v (%T), want ErrChildRunInFlight failed_precondition", err, err)
		}
	}

	// Finish the run so later restore attempts reach the envelope checks.
	if _, err := env.client.Resume(context.Background(), &criteriav2.ResumeRequest{SessionId: sessionID}); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	select {
	case err := <-exErr:
		if err != nil {
			t.Fatalf("Execute after resume: %v", err)
		}
	case <-time.After(40 * time.Second):
		t.Fatal("Execute did not return after resume")
	}

	valid := resp.GetState()

	// Request-side schema_version mismatch → invalid argument.
	if _, err := env.client.Restore(context.Background(), &criteriav2.RestoreRequest{
		SessionId:     sessionID,
		SchemaVersion: serveAdapterSnapshotVersion + 1,
		State:         valid,
	}); err == nil || connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("restore schema_version=%d: err = %v, want invalid_argument", serveAdapterSnapshotVersion+1, err)
	}

	// Digest-vs-workflow mismatch → failed precondition.
	envv, err := unmarshalServeAdapterSnapshot(valid)
	if err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	foreign := cloneSnapshotEnvelope(envv)
	foreign.WorkflowDigest = strings.Repeat("0", 64)
	if _, err := env.client.Restore(context.Background(), &criteriav2.RestoreRequest{
		SessionId:     sessionID,
		SchemaVersion: serveAdapterSnapshotVersion,
		State:         mustMarshal(t, foreign),
	}); err == nil {
		t.Fatal("Restore with a foreign workflow digest succeeded; want failed precondition")
	} else if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("foreign-digest restore code = %q, want failed_precondition (%v)", connect.CodeOf(err), err)
	}

	// A paused node that doesn't resolve in the graph → failed precondition.
	unknownNode := cloneSnapshotEnvelope(envv)
	unknownNode.PausedNode = "no-such-node"
	if _, err := env.client.Restore(context.Background(), &criteriav2.RestoreRequest{
		SessionId:     sessionID,
		SchemaVersion: serveAdapterSnapshotVersion,
		State:         mustMarshal(t, unknownNode),
	}); err == nil {
		t.Fatal("Restore with an unresolvable paused node succeeded; want failed precondition")
	} else if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("unknown-node restore code = %q, want failed_precondition (%v)", connect.CodeOf(err), err)
	}

	// Corrupted bytes → invalid argument (structural byte flip breaks JSON
	// decoding before the digest is even consulted).
	corrupted := append([]byte(nil), valid...)
	corrupted[0] ^= 0x20
	if _, err := env.client.Restore(context.Background(), &criteriav2.RestoreRequest{
		SessionId:     sessionID,
		SchemaVersion: serveAdapterSnapshotVersion,
		State:         corrupted,
	}); err == nil || connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("corrupted restore: err = %v, want invalid_argument", err)
	}
}
