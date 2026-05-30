package adapterhost

import (
	"context"
	"errors"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapter/secrets"
	v2 "github.com/brokenbots/criteria/sdk/pb/criteria/v2"
	"github.com/brokenbots/criteria/workflow"
	"github.com/brokenbots/criteria/workflow/lockfile"
)

// snapshotMockHandle implements Handle and tracks Snapshot/Restore round-trips.
type snapshotMockHandle struct {
	mu            sync.Mutex
	state         []byte
	snapshotCount int
	restoreCount  int
	snapshotErr   error
	restoreErr    error
}

func (m *snapshotMockHandle) Info(context.Context) (Info, error) {
	return Info{Name: "snapshot-mock"}, nil
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
	m.mu.Lock()
	defer m.mu.Unlock()
	m.snapshotCount++
	if m.snapshotErr != nil {
		return nil, m.snapshotErr
	}
	return &v2.SnapshotResponse{State: append([]byte(nil), m.state...)}, nil
}
func (m *snapshotMockHandle) Restore(context.Context, string, []byte, uint32) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.restoreCount++
	if m.restoreErr != nil {
		return m.restoreErr
	}
	return nil
}

func makeTestSession(sm *SessionManager, name string, h Handle, refs map[string]secrets.OriginRef) *Session {
	s := &Session{
		Name:             name,
		Adapter:          "test",
		Config:           map[string]string{"k": "v"},
		Secrets:          map[string]string{"s": "v"},
		SecretOriginRefs: refs,
		OnCrash:          OnCrashFail,
		Capabilities:     []string{"snapshot"},
		handle:           h,
		PermissionState:  NewPermissionState(name, nil),
	}
	s.PermissionState.SetStreamCancel(func() {})
	sm.mu.Lock()
	sm.sessions[name] = s
	sm.mu.Unlock()
	return s
}

func TestSession_Snapshot_RoundTrip(t *testing.T) {
	ctx := context.Background()
	h := &snapshotMockHandle{state: []byte("adapter-state-v1")}
	sm := NewSessionManager(nil)
	s := makeTestSession(sm, "s1", h, map[string]secrets.OriginRef{
		"token": {Kind: "literal", Ref: "sekrit"},
	})

	snap, err := s.Snapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if string(snap.AdapterState) != "adapter-state-v1" {
		t.Fatalf("adapter state = %q; want %q", snap.AdapterState, "adapter-state-v1")
	}
	if snap.SchemaVersion != currentSnapshotSchemaVersion {
		t.Fatalf("schema version = %d; want %d", snap.SchemaVersion, currentSnapshotSchemaVersion)
	}
	if snap.HostArch != runtime.GOOS+"/"+runtime.GOARCH {
		t.Fatalf("host arch = %q; want %q", snap.HostArch, runtime.GOOS+"/"+runtime.GOARCH)
	}
	if len(snap.SecretOriginRefs) != 1 || snap.SecretOriginRefs["token"].Ref != "sekrit" {
		t.Fatalf("secret origin refs mismatch: %+v", snap.SecretOriginRefs)
	}
	if h.snapshotCount != 1 {
		t.Fatalf("expected 1 snapshot call, got %d", h.snapshotCount)
	}
}

func TestSessionManager_SnapshotAll(t *testing.T) {
	ctx := context.Background()
	sm := NewSessionManager(nil)
	makeTestSession(sm, "s1", &snapshotMockHandle{state: []byte("a")}, nil)
	makeTestSession(sm, "s2", &snapshotMockHandle{state: []byte("b")}, nil)

	snaps, err := sm.SnapshotAll(ctx)
	if err != nil {
		t.Fatalf("snapshot all: %v", err)
	}
	if len(snaps) != 2 {
		t.Fatalf("expected 2 snapshots, got %d", len(snaps))
	}
	if string(snaps["s1"].AdapterState) != "a" {
		t.Fatalf("s1 state mismatch")
	}
	if string(snaps["s2"].AdapterState) != "b" {
		t.Fatalf("s2 state mismatch")
	}
}

func TestSessionManager_SnapshotAll_ErrorCollected(t *testing.T) {
	ctx := context.Background()
	sm := NewSessionManager(nil)
	makeTestSession(sm, "s1", &snapshotMockHandle{state: []byte("a")}, nil)
	makeTestSession(sm, "s2", &snapshotMockHandle{snapshotErr: errors.New("snap-err")}, nil)

	snaps, err := sm.SnapshotAll(ctx)
	if err == nil {
		t.Fatal("expected error from s2")
	}
	if len(snaps) != 1 {
		t.Fatalf("expected 1 successful snapshot, got %d", len(snaps))
	}
	if !errors.Is(err, errors.New("snap-err")) {
		// errors.Join wraps; check string instead.
		if !contains(err.Error(), "snap-err") {
			t.Fatalf("expected error to contain snap-err, got %v", err)
		}
	}
}

func TestSessionManager_Restore_RoundTrip(t *testing.T) {
	ctx := context.Background()
	h := &snapshotMockHandle{state: []byte("roundtrip-v1")}
	sm := NewSessionManager(nil)
	makeTestSession(sm, "s1", h, map[string]secrets.OriginRef{
		"token": {Kind: "literal", Ref: "sekrit"},
	})

	snap, err := sm.sessions["s1"].Snapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	// New manager simulates host restart.
	loader := &mockLoaderForRestore{handle: h}
	sm2 := NewSessionManager(loader)

	sess, err := sm2.Restore(ctx, "s1", "test", OnCrashFail, map[string]string{"k": "v"}, nil, snap)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if sess.Name != "s1" {
		t.Fatalf("name = %q; want s1", sess.Name)
	}
	if sess.Adapter != "test" {
		t.Fatalf("adapter = %q; want test", sess.Adapter)
	}
	if h.restoreCount != 1 {
		t.Fatalf("expected 1 restore call, got %d", h.restoreCount)
	}
	if len(sess.SecretOriginRefs) != 1 || sess.SecretOriginRefs["token"].Ref != "sekrit" {
		t.Fatalf("restored origin refs mismatch: %+v", sess.SecretOriginRefs)
	}

	// Restoring into a manager that already has the session should fail.
	_, err = sm2.Restore(ctx, "s1", "test", OnCrashFail, nil, nil, snap)
	if !errors.Is(err, ErrSessionAlreadyOpen) {
		t.Fatalf("expected ErrSessionAlreadyOpen, got %v", err)
	}
}

func TestSessionManager_Restore_RefusesSchemaMismatch(t *testing.T) {
	ctx := context.Background()
	snap := &SessionSnapshot{
		SchemaVersion: 9999,
		HostArch:      runtime.GOOS + "/" + runtime.GOARCH,
	}
	sm := NewSessionManager(nil)
	_, err := sm.Restore(ctx, "s1", "test", OnCrashFail, nil, nil, snap)
	if err == nil {
		t.Fatal("expected error for schema mismatch")
	}
	if !contains(err.Error(), "schema version") {
		t.Fatalf("expected schema version error, got %v", err)
	}
}

func TestSessionManager_Restore_RefusesArchMismatch(t *testing.T) {
	ctx := context.Background()
	snap := &SessionSnapshot{
		SchemaVersion: currentSnapshotSchemaVersion,
		HostArch:      "plan9/amd64",
	}
	sm := NewSessionManager(nil)
	_, err := sm.Restore(ctx, "s1", "test", OnCrashFail, nil, nil, snap)
	if err == nil {
		t.Fatal("expected error for arch mismatch")
	}
	if !contains(err.Error(), "host arch") {
		t.Fatalf("expected host arch error, got %v", err)
	}
}

func TestSessionManager_Restore_RefusesDigestMismatch(t *testing.T) {
	ctx := context.Background()
	snap := &SessionSnapshot{
		SchemaVersion: currentSnapshotSchemaVersion,
		HostArch:      runtime.GOOS + "/" + runtime.GOARCH,
		AdapterDigest: digest.FromString("old-adapter"),
	}
	sm := NewSessionManager(nil)
	sm.lockfile = &lockfile.Lockfile{
		Adapters: []lockfile.LockedAdapter{
			{Type: "test", Name: "default", ResolvedDigest: digest.FromString("new-adapter").String()},
		},
	}
	_, err := sm.Restore(ctx, "test.default", "test", OnCrashFail, nil, nil, snap)
	if err == nil {
		t.Fatal("expected error for digest mismatch")
	}
	if !contains(err.Error(), "Resume requires the same adapter version") {
		t.Fatalf("expected digest mismatch error, got %v", err)
	}
}

func TestSessionManager_Restore_MissingSecret(t *testing.T) {
	ctx := context.Background()
	h := &snapshotMockHandle{}
	sm := NewSessionManager(nil)
	makeTestSession(sm, "s1", h, map[string]secrets.OriginRef{
		"token": {Kind: "env", Ref: "SNAP_TEST_MISSING_SECRET_42"},
	})

	snap, err := sm.sessions["s1"].Snapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	// Ensure env var is absent.
	_ = os.Unsetenv("SNAP_TEST_MISSING_SECRET_42")

	sm2 := NewSessionManager(nil)
	_, err = sm2.Restore(ctx, "s1", "test", OnCrashFail, nil, nil, snap)
	if err == nil {
		t.Fatal("expected error for missing secret")
	}
	if !contains(err.Error(), "missing secret") {
		t.Fatalf("expected missing secret error, got %v", err)
	}
}

func TestSessionManager_Restore_ResolvesLiteralSecret(t *testing.T) {
	ctx := context.Background()
	h := &snapshotMockHandle{}
	sm := NewSessionManager(nil)
	makeTestSession(sm, "s1", h, map[string]secrets.OriginRef{
		"token": {Kind: "literal", Ref: "my-value"},
	})

	snap, err := sm.sessions["s1"].Snapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	loader := &mockLoaderForRestore{handle: h}
	sm2 := NewSessionManager(loader)
	sess, err := sm2.Restore(ctx, "s1", "test", OnCrashFail, nil, nil, snap)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if sess.Secrets["token"] != "my-value" {
		t.Fatalf("secret = %q; want my-value", sess.Secrets["token"])
	}
}

func TestSessionManager_Restore_PermissionStateRoundTrip(t *testing.T) {
	ctx := context.Background()
	h := &snapshotMockHandle{}
	makeTestSession(NewSessionManager(nil), "s1", h, nil)

	// Make a deterministic permission decision.
	ps := NewPermissionState("s1", nil)
	ps.SetStreamCancel(func() {})
	ps.SetPolicy(NewPolicy([]string{"read_file"}))
	decision, _ := ps.Evaluate("read_file", "read_file", "", "")
	if !decision {
		t.Fatal("expected allow")
	}
	permBlob, err := ps.MarshalState()
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}

	snap := &SessionSnapshot{
		AdapterState:     []byte("adapter"),
		SchemaVersion:    currentSnapshotSchemaVersion,
		PermissionState:  permBlob,
		SecretOriginRefs: nil,
		HostArch:         runtime.GOOS + "/" + runtime.GOARCH,
		CreatedAt:        time.Now(),
	}

	sm2 := NewSessionManager(&mockLoaderForRestore{handle: h})
	sess, err := sm2.Restore(ctx, "s1", "test", OnCrashFail, nil, nil, snap)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if sess.PermissionState == nil {
		t.Fatal("expected permission state to be restored")
	}
	// Policy must be set after restore for Evaluate to work.
	sess.PermissionState.SetPolicy(NewPolicy([]string{"read_file"}))
	// Previously-allowed request should still be allowed.
	decision2, _ := sess.PermissionState.Evaluate("read_file", "read_file", "", "")
	if !decision2 {
		t.Fatal("expected previously-allowed request to still be allowed after restore")
	}
}

func TestSessionManager_Restore_AdapterRestoreError(t *testing.T) {
	ctx := context.Background()
	h := &snapshotMockHandle{restoreErr: errors.New("adapter-restore-fail")}
	snap := &SessionSnapshot{
		AdapterState:  []byte("state"),
		SchemaVersion: currentSnapshotSchemaVersion,
		HostArch:      runtime.GOOS + "/" + runtime.GOARCH,
	}

	loader := &mockLoaderForRestore{handle: h}
	sm := NewSessionManager(loader)
	_, err := sm.Restore(ctx, "s1", "test", OnCrashFail, nil, nil, snap)
	if err == nil {
		t.Fatal("expected error")
	}
	if !contains(err.Error(), "adapter restore") {
		t.Fatalf("expected adapter restore error, got %v", err)
	}
}

func TestSessionManager_Restore_ResolveAdapterError(t *testing.T) {
	ctx := context.Background()
	snap := &SessionSnapshot{
		AdapterState:  []byte("state"),
		SchemaVersion: currentSnapshotSchemaVersion,
		HostArch:      runtime.GOOS + "/" + runtime.GOARCH,
	}
	loader := &mockLoaderForRestore{err: errors.New("resolve-fail")}
	sm := NewSessionManager(loader)
	_, err := sm.Restore(ctx, "s1", "test", OnCrashFail, nil, nil, snap)
	if err == nil {
		t.Fatal("expected error")
	}
	if !contains(err.Error(), "resolve-fail") {
		t.Fatalf("expected resolve error, got %v", err)
	}
}

// mockLoaderForRestore is a minimal Loader used only in restore tests.
type mockLoaderForRestore struct {
	handle Handle
	err    error
}

func (l *mockLoaderForRestore) Resolve(ctx context.Context, name string) (Handle, error) {
	if l.err != nil {
		return nil, l.err
	}
	return l.handle, nil
}
func (l *mockLoaderForRestore) Shutdown(context.Context) error { return nil }

func contains(s, substr string) bool {
	return strings.Contains(s, substr)
}
