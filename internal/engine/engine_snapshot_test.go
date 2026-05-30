package engine

import (
	"context"
	"os"
	"testing"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/internal/runtime/state"
	v2 "github.com/brokenbots/criteria/sdk/pb/criteria/v2"
	"github.com/brokenbots/criteria/workflow"
)

// snapshotMockHandle implements Handle and preserves state across Snapshot/Restore.
type snapshotMockHandle struct {
	state []byte
}

func (m *snapshotMockHandle) Info(context.Context) (adapterhost.Info, error) {
	return adapterhost.Info{Name: "snapshot-mock"}, nil
}
func (m *snapshotMockHandle) OpenSession(context.Context, string, map[string]string, map[string]string) error {
	return nil
}
func (m *snapshotMockHandle) Execute(context.Context, string, *workflow.StepNode, adapter.EventSink) (adapter.Result, error) {
	return adapter.Result{Outcome: "success"}, nil
}
func (m *snapshotMockHandle) CloseSession(context.Context, string) error { return nil }
func (m *snapshotMockHandle) Kill()                                      {}
func (m *snapshotMockHandle) Pause(context.Context, string) error        { return nil }
func (m *snapshotMockHandle) Resume(context.Context, string) error       { return nil }
func (m *snapshotMockHandle) Inspect(context.Context, string) (*v2.InspectResponse, error) {
	return &v2.InspectResponse{}, nil
}
func (m *snapshotMockHandle) Snapshot(context.Context, string) (*v2.SnapshotResponse, error) {
	return &v2.SnapshotResponse{State: append([]byte(nil), m.state...)}, nil
}
func (m *snapshotMockHandle) Restore(context.Context, string, []byte, uint32) error { return nil }

type snapshotMockLoader struct {
	handle adapterhost.Handle
}

func (l *snapshotMockLoader) Resolve(_ context.Context, _ string) (adapterhost.Handle, error) {
	return l.handle, nil
}
func (l *snapshotMockLoader) Shutdown(_ context.Context) error { return nil }

func TestEngine_Pause_PersistsSnapshots(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()

	h := &snapshotMockHandle{state: []byte("engine-pause-v1")}
	sm := adapterhost.NewSessionManager(
		&snapshotMockLoader{handle: h})
	if err := sm.Open(ctx, "s1", "test", adapterhost.OnCrashFail, nil, nil); err != nil {
		t.Fatalf("open: %v", err)
	}

	e := &Engine{}
	e.mu.Lock()
	e.liveSessions = sm
	e.runID = "run-42"
	e.snapshotBase = tmp
	e.mu.Unlock()

	if err := e.Pause(ctx); err != nil {
		t.Fatalf("pause: %v", err)
	}

	dir := state.SnapshotDir(tmp, "run-42", "s1")
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("snapshot dir not created: %v", err)
	}

	snap, err := state.ReadLatestSnapshot(dir)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if string(snap.AdapterState) != "engine-pause-v1" {
		t.Fatalf("adapter state = %q; want %q", snap.AdapterState, "engine-pause-v1")
	}
}

func TestEngine_Resume_FromSnapshot(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()

	h := &snapshotMockHandle{state: []byte("engine-resume-v1")}
	loader := &snapshotMockLoader{handle: h}

	// Write a snapshot directly simulating a prior host run.
	snap := &adapterhost.SessionSnapshot{
		AdapterState:  []byte("engine-resume-v1"),
		SchemaVersion: 1,
		HostArch:      "linux/amd64",
	}
	dir := state.SnapshotDir(tmp, "run-99", "s1")
	if _, err := state.WriteSnapshot(dir, snap); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}

	graph := &workflow.FSMGraph{
		Adapters: map[string]*workflow.AdapterNode{
			"s1": {Type: "test", Name: "default", Environment: "env.default"},
		},
		Environments: map[string]*workflow.EnvironmentNode{
			"env.default": {Type: "local", Name: "default"},
		},
	}

	e := New(graph, loader, &fakeSink{}, WithSnapshotBase(tmp), WithRunID("run-99"))

	if err := e.Resume(ctx); err != nil {
		t.Fatalf("resume: %v", err)
	}

	// After resume, the session should exist and be resumable.
	e.mu.RLock()
	sessions := e.liveSessions
	e.mu.RUnlock()
	if sessions == nil {
		t.Fatal("expected liveSessions after resume")
	}
}

func TestEngine_Resume_NoSnapshotDir(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()

	e := New(nil, &snapshotMockLoader{}, &fakeSink{}, WithSnapshotBase(tmp), WithRunID("run-missing"))

	// Resume with no snapshots should fail gracefully.
	if err := e.Resume(ctx); err == nil {
		t.Fatal("expected error when no snapshots exist")
	}
}

func TestEngine_Pause_NoSnapshotBase(t *testing.T) {
	ctx := context.Background()
	sm := adapterhost.NewSessionManager(
		&snapshotMockLoader{handle: &snapshotMockHandle{}})
	if err := sm.Open(ctx, "s1", "test", adapterhost.OnCrashFail, nil, nil); err != nil {
		t.Fatalf("open: %v", err)
	}

	e := &Engine{}
	e.mu.Lock()
	e.liveSessions = sm
	e.runID = "run-1"
	// snapshotBase intentionally left empty
	e.mu.Unlock()

	// Pause without a snapshot base should still succeed (no persistence).
	if err := e.Pause(ctx); err != nil {
		t.Fatalf("pause: %v", err)
	}
}
