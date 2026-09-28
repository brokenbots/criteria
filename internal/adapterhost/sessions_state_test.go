package adapterhost

// sessions_state_test.go — CRI-201: the phase-1 verify handshake must carry
// the adapter's checkpoint-state declaration through to the cached engine
// surface. A valid declaration is stored so engine-side consumers (checkpoint
// save/restore, CRI-202) can read it via DeclaredState; an invalid one — an
// unknown mode or a blob/ref declaration without a schema version tag —
// fails the handshake loudly and is NOT cached (silently treating an
// unrecognized mode as none would drop checkpointing for a stateful
// adapter).

import (
	"context"
	"runtime"
	"strings"
	"testing"

	"github.com/brokenbots/criteria/workflow"
)

// stateHandleStub is a Handle whose Info returns a fixed Info; only the
// handshake uses it.
type stateHandleStub struct {
	cri269Handle
	info Info
}

func (h *stateHandleStub) Info(context.Context) (Info, error) { return h.info, nil }

// stateLoaderStub hands every Resolve the same stub handle.
type stateLoaderStub struct {
	handle Handle
}

func (l *stateLoaderStub) Resolve(context.Context, string) (Handle, error) {
	return l.handle, nil
}
func (l *stateLoaderStub) Shutdown(context.Context) error { return nil }

// verifyStateHandshake runs the phase-1 verify handshake against the given
// declaration and returns (verify error, declaration captured in the manager
// cache afterwards).
func verifyStateHandshake(t *testing.T, decl *workflow.StateDeclaration) (error, *workflow.StateDeclaration) {
	t.Helper()
	m := NewSessionManager(&stateLoaderStub{handle: &stateHandleStub{
		info: Info{AdapterInfo: workflow.AdapterInfo{State: decl}},
	}})
	_, err := m.verifyAdapterInfo(context.Background(), "state-test", "stateful", "", nil, nil)
	return err, m.DeclaredState("state-test")
}

func TestVerifyHandshake_PropagatesValidBlobDeclaration(t *testing.T) {
	decl := &workflow.StateDeclaration{
		Mode:        workflow.StateModeBlob,
		Schema:      "harness.v1",
		MaxBytes:    64 * 1024,
		Granularity: workflow.StateGranularityPerTurn,
	}
	err, declared := verifyStateHandshake(t, decl)
	if err != nil {
		t.Fatalf("verifyAdapterInfo: %v", err)
	}
	if declared == nil {
		t.Fatal("DeclaredState = nil; want the declaration captured in the engine surface")
	}
	if *decl != *declared {
		t.Fatalf("DeclaredState = %+v; want %+v", *declared, *decl)
	}
}

func TestVerifyHandshake_AcceptsExplicitNoneAndCapturesIt(t *testing.T) {
	decl := &workflow.StateDeclaration{Mode: workflow.StateModeNone}
	err, declared := verifyStateHandshake(t, decl)
	if err != nil {
		t.Fatalf("verifyAdapterInfo: %v", err)
	}
	if declared == nil || declared.Mode != workflow.StateModeNone {
		t.Fatalf("DeclaredState = %+v; want explicit mode-none declaration", declared)
	}
}

func TestVerifyHandshake_AbsentDeclarationBehavesAsToday(t *testing.T) {
	err, declared := verifyStateHandshake(t, nil)
	if err != nil {
		t.Fatalf("verifyAdapterInfo: %v; adapters without a declaration must verify as before", err)
	}
	if declared != nil {
		t.Fatalf("DeclaredState = %+v; want nil", declared)
	}
}

func TestVerifyHandshake_UnknownModeFailsLoudlyAndIsNotCached(t *testing.T) {
	err, declared := verifyStateHandshake(t, &workflow.StateDeclaration{
		Mode:   "snapshot",
		Schema: "harness.v1",
	})
	if err == nil {
		t.Fatal("verifyAdapterInfo = nil; want loud handshake failure for an unknown state mode")
	}
	if !strings.Contains(err.Error(), "unknown state mode") {
		t.Fatalf("error = %q; want it to name the unknown mode", err.Error())
	}
	if declared != nil {
		t.Fatalf("DeclaredState = %+v; want nil — a rejected declaration must not be cached", declared)
	}
}

func TestVerifyHandshake_BlobWithoutSchemaFailsLoudlyAndIsNotCached(t *testing.T) {
	err, declared := verifyStateHandshake(t, &workflow.StateDeclaration{
		Mode:        workflow.StateModeBlob,
		Granularity: workflow.StateGranularityPerTurn,
	})
	if err == nil {
		t.Fatal("verifyAdapterInfo = nil; want loud handshake failure for blob without schema")
	}
	if !strings.Contains(err.Error(), "requires a non-empty schema version tag") {
		t.Fatalf("error = %q; want it to name the missing schema tag", err.Error())
	}
	if declared != nil {
		t.Fatalf("DeclaredState = %+v; want nil — a rejected declaration must not be cached", declared)
	}
}

// stateRestoreHandle adapts snapshotMockHandle — which already satisfies the
// full Handle surface with successful Snapshot/Restore round-trips — to a
// relaunch whose Info returns the declaration under test. It counts Kill calls
// so the test can prove a rejected declaration tears the relaunched process
// down instead of leaving it running.
type stateRestoreHandle struct {
	snapshotMockHandle
	info      Info
	killCount int
}

func (h *stateRestoreHandle) Info(context.Context) (Info, error) { return h.info, nil }

func (h *stateRestoreHandle) Kill() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.killCount++
}

// TestRestore_RelaunchSurfacesInvalidDeclarationFailsLoudly covers the
// restore-path half of the handshake contract (CRI-201): a relaunch may
// surface a malformed declaration (e.g. the adapter binary was swapped
// mid-run), and the restore must fail loudly — never silently downgrade to
// mode none — while tearing the relaunched process down.
func TestRestore_RelaunchSurfacesInvalidDeclarationFailsLoudly(t *testing.T) {
	ctx := context.Background()
	h := &stateRestoreHandle{
		info: Info{AdapterInfo: workflow.AdapterInfo{State: &workflow.StateDeclaration{
			Mode:   "snapshot",
			Schema: "harness.v1",
		}}},
	}
	snap := &SessionSnapshot{
		AdapterState:  []byte("state"),
		SchemaVersion: currentSnapshotSchemaVersion,
		HostArch:      runtime.GOOS + "/" + runtime.GOARCH,
	}

	sm := NewSessionManager(&mockLoaderForRestore{handle: h})
	_, err := sm.Restore(ctx, "s1", "stateful", OnCrashFail, nil, nil, snap)
	if err == nil {
		t.Fatal("Restore = nil error; want loud failure for an unknown state mode on relaunch")
	}
	if !strings.Contains(err.Error(), "unknown state mode") {
		t.Fatalf("error = %q; want it to name the unknown mode", err.Error())
	}
	sm.mu.Lock()
	_, registered := sm.sessions["s1"]
	sm.mu.Unlock()
	if registered {
		t.Fatal("session registered despite a rejected declaration; restore must fail closed")
	}
	h.mu.Lock()
	killCount := h.killCount
	h.mu.Unlock()
	if killCount != 1 {
		t.Fatalf("Kill calls = %d; want 1 — the relaunched process must be torn down", killCount)
	}
	if declared := sm.DeclaredState("s1"); declared != nil {
		t.Fatalf("DeclaredState = %+v; want nil — a rejected declaration must not be cached", declared)
	}
}

// TestRestore_RelaunchSurfacesValidDeclarationCapturesIt verifies the
// restore-path propagation: a valid declaration surfaced by the relaunched
// process is re-captured into the engine surface, so a restored session keeps
// checkpointing on the adapter's declared state surface.
func TestRestore_RelaunchSurfacesValidDeclarationCapturesIt(t *testing.T) {
	ctx := context.Background()
	decl := &workflow.StateDeclaration{
		Mode:        workflow.StateModeBlob,
		Schema:      "harness.v1",
		MaxBytes:    64 * 1024,
		Granularity: workflow.StateGranularityPerTurn,
	}
	h := &stateRestoreHandle{info: Info{AdapterInfo: workflow.AdapterInfo{State: decl}}}
	snap := &SessionSnapshot{
		AdapterState:  []byte("state"),
		SchemaVersion: currentSnapshotSchemaVersion,
		HostArch:      runtime.GOOS + "/" + runtime.GOARCH,
	}

	sm := NewSessionManager(&mockLoaderForRestore{handle: h})
	if _, err := sm.Restore(ctx, "s1", "stateful", OnCrashFail, nil, nil, snap); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	declared := sm.DeclaredState("s1")
	if declared == nil || *declared != *decl {
		t.Fatalf("DeclaredState = %+v; want the relaunched adapter's blob declaration", declared)
	}
}
