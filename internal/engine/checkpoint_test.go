package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/internal/runtime/state"
	"github.com/brokenbots/criteria/workflow"
)

// checkpointGraph compiles a two-step workflow whose adapter is named ck;
// the session the engine checkpoints is the adapter key "ck.default".
func checkpointGraph(t *testing.T) *workflow.FSMGraph {
	t.Helper()
	return compile(t, `
workflow {
  name = "ck"
  version = "0.1"
  initial_state = "a"
  target_state  = "done"
}
adapter "ck" "default" {}
step "a" {
  target = adapter.ck.default
  outcome "success" { next = state.done }
}
state "done" { terminal = true }
`)
}

// twoStepGraph compiles a workflow with a second step so a run can be
// interrupted mid-flight (before its terminal janitor would run).
func twoStepGraph(t *testing.T) *workflow.FSMGraph {
	t.Helper()
	return compile(t, `
workflow {
  name = "ck"
  version = "0.1"
  initial_state = "a"
  target_state  = "done"
}
adapter "ck" "default" {}
step "a" {
  target = adapter.ck.default
  outcome "success" { next = step.b }
}
step "b" {
  target = adapter.ck.default
  outcome "success" { next = state.done }
}
state "done" { terminal = true }
`)
}

// restoreRecord captures one Restore RPC the adapter received.
type restoreRecord struct {
	blob          []byte
	schemaVersion uint32
}

// checkpointHandle is a stateful handle declaring blob/per-step checkpoint
// state. It records Restore RPCs (the resume replay) and mutates its state
// through Execute/Snapshot/Restore like a real adapter would.
type checkpointHandle struct {
	mu           sync.Mutex
	state        []byte
	restoreCalls []restoreRecord
	executeCalls int
	blockSecond  bool // second Execute blocks until its ctx is canceled
}

func (h *checkpointHandle) Info(context.Context) (adapterhost.Info, error) {
	return adapterhost.Info{
		Name: "ck",
		AdapterInfo: workflow.AdapterInfo{
			State: &workflow.StateDeclaration{
				Mode:        workflow.StateModeBlob,
				Schema:      "v1",
				Granularity: workflow.StateGranularityPerStep,
			},
		},
	}, nil
}
func (h *checkpointHandle) OpenSession(context.Context, string, map[string]string, map[string]string) error {
	return nil
}
func (h *checkpointHandle) Execute(ctx context.Context, _ string, _ *workflow.StepNode, _ adapter.EventSink) (adapter.Result, error) {
	h.mu.Lock()
	h.executeCalls++
	n := h.executeCalls
	h.mu.Unlock()
	if h.blockSecond && n >= 2 {
		<-ctx.Done()
		return adapter.Result{}, ctx.Err()
	}
	h.mu.Lock()
	h.state = []byte(fmt.Sprintf("state-after-call-%d", n))
	h.mu.Unlock()
	return adapter.Result{Outcome: "success"}, nil
}
func (h *checkpointHandle) Permit(context.Context, string, string, bool, string) error { return nil }
func (h *checkpointHandle) CloseSession(context.Context, string) error                 { return nil }
func (h *checkpointHandle) Kill()                                                      {}
func (h *checkpointHandle) Pause(context.Context, string) error                        { return nil }
func (h *checkpointHandle) Resume(context.Context, string) error                       { return nil }
func (h *checkpointHandle) Inspect(context.Context, string) (*v2.InspectResponse, error) {
	return &v2.InspectResponse{}, nil
}
func (h *checkpointHandle) Snapshot(context.Context, string) (*v2.SnapshotResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return &v2.SnapshotResponse{State: append([]byte(nil), h.state...)}, nil
}
func (h *checkpointHandle) Restore(_ context.Context, _ string, blob []byte, schemaVersion uint32) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.restoreCalls = append(h.restoreCalls, restoreRecord{blob: append([]byte(nil), blob...), schemaVersion: schemaVersion})
	h.state = append([]byte(nil), blob...)
	return nil
}

func (h *checkpointHandle) restores() []restoreRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.restoreCalls
}

type checkpointLoader struct{ handle adapterhost.Handle }

func (l *checkpointLoader) Resolve(context.Context, string) (adapterhost.Handle, error) {
	return l.handle, nil
}
func (l *checkpointLoader) Shutdown(context.Context) error { return nil }

// hookSink exposes a hook fired after each step outcome event so tests can
// cancel a run at a precise point (after the step's checkpoint is durable).
type hookSink struct {
	fakeSink
	onStepOutcome func(step string)
}

func (s *hookSink) OnStepOutcome(step, outcome string, d time.Duration, err error) {
	s.fakeSink.OnStepOutcome(step, outcome, d, err)
	if s.onStepOutcome != nil {
		s.onStepOutcome(step)
	}
}

// writePriorCheckpoint persists a schema-stamped checkpoint holding
// "prior-state", simulating the state a previous (killed) run left behind.
// All tests reuse the testRunID run.
const testRunID = "run-ck"

func writePriorCheckpoint(t *testing.T, base, sessionID string) {
	t.Helper()
	snap := &adapterhost.SessionSnapshot{
		AdapterState:  []byte("prior-state"),
		SchemaVersion: 1,
		HostArch:      runtime.GOOS + "/" + runtime.GOARCH,
		StateSchema:   "v1",
		StateMode:     workflow.StateModeBlob,
		StateDigest:   adapterhost.ComputeStateDigest([]byte("prior-state")),
	}
	if _, err := state.WriteSnapshot(state.SnapshotDir(base, testRunID, sessionID), snap); err != nil {
		t.Fatalf("write prior checkpoint: %v", err)
	}
}

// TestEngine_CheckpointSavedAtStepBoundary runs a two-step workflow and
// cancels it as the first step's outcome event fires. The first step's
// checkpoint must be on disk — a step is only done once its state is durable
// — with the declared schema tag and a matching digest.
func TestEngine_CheckpointSavedAtStepBoundary(t *testing.T) {
	tmp := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h := &checkpointHandle{blockSecond: true}
	g := twoStepGraph(t)
	runDone := make(chan error, 1)
	go func() {
		e := New(g, &checkpointLoader{handle: h}, &hookSink{
			onStepOutcome: func(step string) {
				if step == "a" {
					cancel()
				}
			},
		}, WithSnapshotBase(tmp), WithRunID(testRunID))
		runDone <- e.Run(ctx)
	}()
	if err := <-runDone; err == nil {
		t.Fatal("expected canceled run to return an error")
	}

	dir := state.SnapshotDir(tmp, testRunID, "ck.default")
	snap, err := state.ReadLatestSnapshot(dir)
	if err != nil {
		t.Fatalf("read saved checkpoint: %v", err)
	}
	if snap.StateSchema != "v1" {
		t.Errorf("checkpoint schema = %q; want v1", snap.StateSchema)
	}
	if snap.StateDigest == "" {
		t.Error("checkpoint digest missing")
	}
	if len(snap.AdapterState) == 0 {
		t.Fatal("checkpoint blob empty")
	}
}

// TestEngine_ResumeFromCheckpoint_KillRestart is the acceptance test: a run
// checkpointed, the runner died, and the restarted run must complete with
// the adapter restored from the last checkpoint — not silently started
// fresh.
func TestEngine_ResumeFromCheckpoint_KillRestart(t *testing.T) {
	tmp := t.TempDir()
	writePriorCheckpoint(t, tmp, "ck.default")

	h := &checkpointHandle{}
	sink := &fakeSink{}
	e := New(checkpointGraph(t), &checkpointLoader{handle: h}, sink,
		WithSnapshotBase(tmp), WithRunID(testRunID))
	if err := e.RunFrom(context.Background(), "a", 1); err != nil {
		t.Fatalf("resumed run: %v", err)
	}
	if sink.terminal != "done" || !sink.terminalOK {
		t.Errorf("terminal = %q ok=%v; want done/true", sink.terminal, sink.terminalOK)
	}
	calls := h.restores()
	if len(calls) != 1 {
		t.Fatalf("adapter received %d restore calls; want 1", len(calls))
	}
	if string(calls[0].blob) != "prior-state" {
		t.Errorf("restored blob = %q; want prior-state", calls[0].blob)
	}
	if calls[0].schemaVersion != 1 {
		t.Errorf("restored schemaVersion = %d; want 1", calls[0].schemaVersion)
	}
}

// TestEngine_ResumeFromCheckpoint_TruncatedBlobFailsLoud: a corrupted blob
// fails the run with a diagnostic naming the adapter and schema version.
func TestEngine_ResumeFromCheckpoint_TruncatedBlobFailsLoud(t *testing.T) {
	tmp := t.TempDir()
	writePriorCheckpoint(t, tmp, "ck.default")
	binPath := filepath.Join(state.SnapshotDir(tmp, testRunID, "ck.default"), "0000000001.bin")
	if err := os.Truncate(binPath, 2); err != nil {
		t.Fatalf("truncate blob: %v", err)
	}

	h := &checkpointHandle{}
	sink := &fakeSink{}
	e := New(checkpointGraph(t), &checkpointLoader{handle: h}, sink,
		WithSnapshotBase(tmp), WithRunID(testRunID))
	err := e.RunFrom(context.Background(), "a", 1)
	if err == nil {
		t.Fatal("expected truncated blob to fail the run loudly")
	}
	if !strings.Contains(err.Error(), "truncated or corrupted") || !strings.Contains(err.Error(), "ck.default") || !strings.Contains(err.Error(), "v1") {
		t.Errorf("restore error lacks adapter/schema diagnostic: %v", err)
	}
	if len(h.restores()) != 0 {
		t.Error("adapter must not receive state from a corrupted checkpoint")
	}
	if sink.failure == "" {
		t.Error("OnRunFailed not emitted for loud restore failure")
	}
}

// TestEngine_ResumeFromCheckpoint_MissingBlobFailsLoud: metadata without its
// blob (interrupted or partially deleted save) fails the run loudly.
func TestEngine_ResumeFromCheckpoint_MissingBlobFailsLoud(t *testing.T) {
	tmp := t.TempDir()
	writePriorCheckpoint(t, tmp, "ck.default")
	if err := os.Remove(filepath.Join(state.SnapshotDir(tmp, testRunID, "ck.default"), "0000000001.bin")); err != nil {
		t.Fatalf("remove blob: %v", err)
	}

	h := &checkpointHandle{}
	e := New(checkpointGraph(t), &checkpointLoader{handle: h}, &fakeSink{},
		WithSnapshotBase(tmp), WithRunID(testRunID))
	err := e.RunFrom(context.Background(), "a", 1)
	if err == nil {
		t.Fatal("expected missing blob to fail the run loudly")
	}
	if !strings.Contains(err.Error(), "missing its state blob") || !strings.Contains(err.Error(), "ck.default") {
		t.Errorf("restore error lacks missing-blob diagnostic: %v", err)
	}
	if len(h.restores()) != 0 {
		t.Error("adapter must not receive state from an incomplete checkpoint")
	}
}

// TestEngine_ResumeFromCheckpoint_BlobWithoutMetadataFailsLoud: a save that
// was interrupted between the blob and its metadata must fail loudly.
func TestEngine_ResumeFromCheckpoint_BlobWithoutMetadataFailsLoud(t *testing.T) {
	tmp := t.TempDir()
	writePriorCheckpoint(t, tmp, "ck.default")
	if err := os.Remove(filepath.Join(state.SnapshotDir(tmp, testRunID, "ck.default"), "0000000001.json")); err != nil {
		t.Fatalf("remove metadata: %v", err)
	}

	h := &checkpointHandle{}
	e := New(checkpointGraph(t), &checkpointLoader{handle: h}, &fakeSink{},
		WithSnapshotBase(tmp), WithRunID(testRunID))
	err := e.RunFrom(context.Background(), "a", 1)
	if err == nil {
		t.Fatal("expected blob without metadata to fail the run loudly")
	}
	if !strings.Contains(err.Error(), "incomplete") || !strings.Contains(err.Error(), "ck.default") {
		t.Errorf("restore error lacks interrupted-save diagnostic: %v", err)
	}
}

// TestEngine_ResumeFromCheckpoint_UnknownSessionFailsLoud: a checkpoint for
// an adapter that is not in the workflow graph cannot be replayed and must
// fail loudly rather than be ignored.
func TestEngine_ResumeFromCheckpoint_UnknownSessionFailsLoud(t *testing.T) {
	tmp := t.TempDir()
	writePriorCheckpoint(t, tmp, "ghost")

	e := New(checkpointGraph(t), &checkpointLoader{handle: &checkpointHandle{}}, &fakeSink{},
		WithSnapshotBase(tmp), WithRunID(testRunID))
	err := e.RunFrom(context.Background(), "a", 1)
	if err == nil {
		t.Fatal("expected unknown checkpointed session to fail the run loudly")
	}
	if !strings.Contains(err.Error(), "ghost") || !strings.Contains(err.Error(), "workflow graph") {
		t.Errorf("restore error lacks unknown-session diagnostic: %v", err)
	}
}

// TestEngine_TerminalRunDeletesCheckpoints: the retention janitor is
// criteria-side — a run that reaches a terminal state releases its own
// checkpoints immediately.
func TestEngine_TerminalRunDeletesCheckpoints(t *testing.T) {
	tmp := t.TempDir()
	h := &checkpointHandle{}
	e := New(checkpointGraph(t), &checkpointLoader{handle: h}, &fakeSink{},
		WithSnapshotBase(tmp), WithRunID(testRunID))
	if err := e.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, err := os.Stat(state.CheckpointRunDir(tmp, testRunID)); !os.IsNotExist(err) {
		t.Fatalf("terminal run's checkpoints not deleted (stat err: %v)", err)
	}
}

// TestEngine_CanceledRunKeepsCheckpoints: the STOPPED exemption — a run
// interrupted by context cancellation keeps its checkpoints so the stop
// feature does not lose its own state.
func TestEngine_CanceledRunKeepsCheckpoints(t *testing.T) {
	tmp := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h := &checkpointHandle{blockSecond: true}
	g := twoStepGraph(t)
	runDone := make(chan error, 1)
	go func() {
		e := New(g, &checkpointLoader{handle: h}, &hookSink{
			onStepOutcome: func(step string) {
				if step == "a" {
					cancel()
				}
			},
		}, WithSnapshotBase(tmp), WithRunID(testRunID))
		runDone <- e.Run(ctx)
	}()
	select {
	case err := <-runDone:
		if err == nil && ctx.Err() == nil {
			t.Fatal("run returned before cancellation took effect")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("canceled run did not return")
	}
	snap, err := state.ReadLatestSnapshot(state.SnapshotDir(tmp, testRunID, "ck.default"))
	if err != nil {
		t.Fatalf("canceled run's checkpoints must survive: %v", err)
	}
	if len(snap.AdapterState) == 0 {
		t.Error("surviving checkpoint blob empty")
	}
}

// TestEngine_ResumeNoCheckpointsStartsFresh: a resumed run whose adapter
// never checkpointed starts fresh as a logged normal start (not a silent
// fallback — there is nothing to restore).
func TestEngine_ResumeNoCheckpointsStartsFresh(t *testing.T) {
	tmp := t.TempDir()
	h := &checkpointHandle{}
	sink := &fakeSink{}
	e := New(checkpointGraph(t), &checkpointLoader{handle: h}, sink,
		WithSnapshotBase(tmp), WithRunID("run-empty"))
	if err := e.RunFrom(context.Background(), "a", 1); err != nil {
		t.Fatalf("fresh resume cycle: %v", err)
	}
	if sink.terminal != "done" {
		t.Errorf("terminal = %q; want done", sink.terminal)
	}
	if len(h.restores()) != 0 {
		t.Errorf("adapter received %d restore calls; want 0", len(h.restores()))
	}
}

// TestEngine_ResumeFromCheckpoint_SchemaMismatchFailsLoud: an older or
// mismatched declaration reading a newer schema must refuse loudly.
func TestEngine_ResumeFromCheckpoint_SchemaMismatchFailsLoud(t *testing.T) {
	tmp := t.TempDir()
	writePriorCheckpoint(t, tmp, "ck.default")

	// The relaunched adapter declares schema v2; the checkpoint carries v1.
	mutating := &schemaOverrideHandle{inner: &checkpointHandle{}, schema: "v2"}
	e := New(checkpointGraph(t), &checkpointLoader{handle: mutating}, &fakeSink{},
		WithSnapshotBase(tmp), WithRunID(testRunID))
	err := e.RunFrom(context.Background(), "a", 1)
	if err == nil {
		t.Fatal("expected schema mismatch to fail the run loudly")
	}
	if !strings.Contains(err.Error(), "state schema mismatch") || !strings.Contains(err.Error(), "v1") || !strings.Contains(err.Error(), "v2") {
		t.Errorf("restore error lacks schema-mismatch diagnostic: %v", err)
	}
}

// schemaOverrideHandle wraps a handle and rewrites its declared state schema,
// simulating an adapter binary whose schema changed between the save and the
// restore.
type schemaOverrideHandle struct {
	inner  adapterhost.Handle
	schema string
}

func (o *schemaOverrideHandle) Info(ctx context.Context) (adapterhost.Info, error) {
	info, err := o.inner.Info(ctx)
	if err != nil {
		return info, err
	}
	if info.AdapterInfo.State != nil {
		info.AdapterInfo.State.Schema = o.schema
	}
	return info, nil
}
func (o *schemaOverrideHandle) OpenSession(ctx context.Context, name string, config, secrets map[string]string) error {
	return o.inner.OpenSession(ctx, name, config, secrets)
}
func (o *schemaOverrideHandle) Execute(ctx context.Context, step string, n *workflow.StepNode, s adapter.EventSink) (adapter.Result, error) {
	return o.inner.Execute(ctx, step, n, s)
}
func (o *schemaOverrideHandle) CloseSession(ctx context.Context, name string) error {
	return o.inner.CloseSession(ctx, name)
}
func (o *schemaOverrideHandle) Kill() { o.inner.Kill() }
func (o *schemaOverrideHandle) Pause(ctx context.Context, name string) error {
	return o.inner.Pause(ctx, name)
}
func (o *schemaOverrideHandle) Resume(ctx context.Context, name string) error {
	return o.inner.Resume(ctx, name)
}
func (o *schemaOverrideHandle) Inspect(ctx context.Context, name string) (*v2.InspectResponse, error) {
	return o.inner.Inspect(ctx, name)
}
func (o *schemaOverrideHandle) Snapshot(ctx context.Context, name string) (*v2.SnapshotResponse, error) {
	return o.inner.Snapshot(ctx, name)
}
func (o *schemaOverrideHandle) Restore(ctx context.Context, name string, blob []byte, version uint32) error {
	return o.inner.Restore(ctx, name, blob, version)
}

// TestCheckpointStoreDeleteRunIsBestEffortOnMissingDir: deleting checkpoints
// for a run that never persisted any must not error.
func TestCheckpointStoreDeleteRunIsBestEffortOnMissingDir(t *testing.T) {
	tmp := t.TempDir()
	store := state.NewCheckpointStore(tmp, "no-such-run")
	if err := store.DeleteRun(); err != nil {
		t.Fatalf("DeleteRun on missing dir: %v (want nil)", err)
	}
}
