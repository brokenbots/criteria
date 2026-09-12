package engine

// Regression tests for CRI-137: a runner-pod restart mid-run used to rotate a
// fresh scope instance + accept token, so surviving adapter pods holding the
// pre-restart token could never re-handshake and the run wedged on the shim
// accept loop forever. The engine now persists the current scope instance per
// adapter and reuses it on re-entry (idempotent rotation), and falls back to a
// fresh rotation only when the persisted record is unusable or the scope was
// deliberately torn down.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow"
)

// newPerScopeTestHarness wires a fresh SessionManager, fake shim and lifecycle
// state against a shared dataDir, mirroring what a restarted runner sees (the
// data dir persists across restarts; in-memory state does not).
func newPerScopeTestHarness(t *testing.T, g *workflow.FSMGraph, dataDir string) (*eventTrackingSink, *fakeRemoteShim, *remoteLifecycleContext, Deps) {
	t.Helper()

	sessions := adapterhost.NewSessionManager(&fakeLoader{})
	sessions.SetGraph(g)
	shim := newFakeRemoteShim(&fakeRemoteHandle{})
	sessions.SetRemoteShim(shim)

	lifecycle := newScopeLifecycleState(dataDir)
	lifecycle.setRunID("run-123")
	sink := &eventTrackingSink{}
	rlc := &remoteLifecycleContext{scopeLifecycle: lifecycle}
	deps := Deps{Sessions: sessions, Sink: sink}
	return sink, shim, rlc, deps
}

func TestInitScopeAdapters_PerScope_ReusesScopeInstanceAfterRestart(t *testing.T) {
	ctx := context.Background()
	g := perScopeRemoteGraph(t)
	dataDir := t.TempDir()

	// First engine instance: initial scope entry rotates a fresh token.
	sink1, _, rlc1, deps1 := newPerScopeTestHarness(t, g, dataDir)
	if _, err := initScopeAdapters(ctx, g, deps1, nil, dataDir, "", nil, rlc1); err != nil {
		t.Fatalf("first initScopeAdapters: %v", err)
	}
	first, ok := sink1.firstStatus("provision_wanted")
	if !ok {
		t.Fatal("first init emitted no provision_wanted event")
	}

	// Simulated runner restart: brand-new in-memory state, same dataDir.
	sink2, shim2, rlc2, deps2 := newPerScopeTestHarness(t, g, dataDir)
	if _, err := initScopeAdapters(ctx, g, deps2, nil, dataDir, "", nil, rlc2); err != nil {
		t.Fatalf("post-restart initScopeAdapters: %v", err)
	}
	second, ok := sink2.firstStatus("provision_wanted")
	if !ok {
		t.Fatal("post-restart init emitted no provision_wanted event")
	}

	if second.ScopeInstanceID != first.ScopeInstanceID {
		t.Fatalf("post-restart ScopeInstanceID = %q, want reused %q", second.ScopeInstanceID, first.ScopeInstanceID)
	}
	if second.TokenRef != first.TokenRef {
		t.Fatalf("post-restart TokenRef = %q, want reused %q", second.TokenRef, first.TokenRef)
	}

	// The restarted shim must accept the same token the surviving pods hold.
	scopeKey := first.ScopeName + "/" + first.ScopeInstanceID
	token, err := os.ReadFile(first.TokenRef)
	if err != nil {
		t.Fatalf("read token file: %v", err)
	}
	if got := shim2.registeredToken(scopeKey); got != string(token) {
		t.Errorf("post-restart shim registered token %q, want surviving pods' token %q", got, string(token))
	}

	// The persisted record must be restricted like the token material.
	if info, err := os.Stat(filepath.Join(dataDir, "remote-tokens", "current")); err != nil {
		t.Fatalf("stat record dir: %v", err)
	} else if info.Mode().Perm() != 0o700 {
		t.Errorf("record dir permissions = %o, want 0o700", info.Mode().Perm())
	}
	if info, err := os.Stat(filepath.Join(dataDir, "remote-tokens", "current", "noop.default.json")); err != nil {
		t.Fatalf("stat record file: %v", err)
	} else if info.Mode().Perm() != 0o600 {
		t.Errorf("record file permissions = %o, want 0o600", info.Mode().Perm())
	}
}

func TestInitScopeAdapters_PerScope_FreshRotationAfterRelease(t *testing.T) {
	ctx := context.Background()
	g := perScopeRemoteGraph(t)
	dataDir := t.TempDir()

	sink1, _, rlc1, deps1 := newPerScopeTestHarness(t, g, dataDir)
	order, err := initScopeAdapters(ctx, g, deps1, nil, dataDir, "", nil, rlc1)
	if err != nil {
		t.Fatalf("first initScopeAdapters: %v", err)
	}
	first, ok := sink1.firstStatus("provision_wanted")
	if !ok {
		t.Fatal("first init emitted no provision_wanted event")
	}

	// Deliberate teardown (pause, body exit) releases the scope and must clear
	// the persisted instance so the next entry rotates fresh.
	tearDownScopeAdapters(ctx, order, deps1, rlc1)
	if _, err := os.Stat(filepath.Join(dataDir, "remote-tokens", "current", "noop.default.json")); !os.IsNotExist(err) {
		t.Fatalf("record file still present after release: %v", err)
	}

	sink2, _, rlc2, deps2 := newPerScopeTestHarness(t, g, dataDir)
	if _, err := initScopeAdapters(ctx, g, deps2, nil, dataDir, "", nil, rlc2); err != nil {
		t.Fatalf("post-release initScopeAdapters: %v", err)
	}
	second, ok := sink2.firstStatus("provision_wanted")
	if !ok {
		t.Fatal("post-release init emitted no provision_wanted event")
	}
	if second.ScopeInstanceID == first.ScopeInstanceID {
		t.Fatalf("post-release ScopeInstanceID = %q, want a fresh rotation after release", second.ScopeInstanceID)
	}
	if second.TokenRef == first.TokenRef {
		t.Fatalf("post-release TokenRef = %q, want a fresh token file after release", second.TokenRef)
	}
}

func TestInitScopeAdapters_PerScope_CorruptRecordFallsBackToFreshRotation(t *testing.T) {
	ctx := context.Background()
	g := perScopeRemoteGraph(t)
	dataDir := t.TempDir()

	sink1, _, rlc1, deps1 := newPerScopeTestHarness(t, g, dataDir)
	if _, err := initScopeAdapters(ctx, g, deps1, nil, dataDir, "", nil, rlc1); err != nil {
		t.Fatalf("first initScopeAdapters: %v", err)
	}
	first, ok := sink1.firstStatus("provision_wanted")
	if !ok {
		t.Fatal("first init emitted no provision_wanted event")
	}

	recordPath := filepath.Join(dataDir, "remote-tokens", "current", "noop.default.json")
	if err := os.WriteFile(recordPath, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("corrupt record: %v", err)
	}

	// Restart with a corrupt record: engine must self-heal with a fresh
	// rotation instead of failing the run.
	sink2, _, rlc2, deps2 := newPerScopeTestHarness(t, g, dataDir)
	if _, err := initScopeAdapters(ctx, g, deps2, nil, dataDir, "", nil, rlc2); err != nil {
		t.Fatalf("post-corruption initScopeAdapters: %v", err)
	}
	second, ok := sink2.firstStatus("provision_wanted")
	if !ok {
		t.Fatal("post-corruption init emitted no provision_wanted event")
	}
	if second.ScopeInstanceID == first.ScopeInstanceID {
		t.Fatalf("post-corruption ScopeInstanceID = %q, want a fresh rotation", second.ScopeInstanceID)
	}
	if _, err := os.Stat(recordPath); err != nil {
		t.Fatalf("record not rewritten after fallback: %v", err)
	}
}

func TestInitScopeAdapters_PerScope_MissingTokenFileFallsBackToFreshRotation(t *testing.T) {
	ctx := context.Background()
	g := perScopeRemoteGraph(t)
	dataDir := t.TempDir()

	sink1, _, rlc1, deps1 := newPerScopeTestHarness(t, g, dataDir)
	if _, err := initScopeAdapters(ctx, g, deps1, nil, dataDir, "", nil, rlc1); err != nil {
		t.Fatalf("first initScopeAdapters: %v", err)
	}
	first, ok := sink1.firstStatus("provision_wanted")
	if !ok {
		t.Fatal("first init emitted no provision_wanted event")
	}
	if err := os.Remove(first.TokenRef); err != nil {
		t.Fatalf("remove token file: %v", err)
	}

	sink2, _, rlc2, deps2 := newPerScopeTestHarness(t, g, dataDir)
	if _, err := initScopeAdapters(ctx, g, deps2, nil, dataDir, "", nil, rlc2); err != nil {
		t.Fatalf("post-removal initScopeAdapters: %v", err)
	}
	second, ok := sink2.firstStatus("provision_wanted")
	if !ok {
		t.Fatal("post-removal init emitted no provision_wanted event")
	}
	if second.ScopeInstanceID == first.ScopeInstanceID {
		t.Fatalf("post-removal ScopeInstanceID = %q, want a fresh rotation", second.ScopeInstanceID)
	}
}

// TestInitScopeAdapters_VerifyFailureSurfacesAcceptTokenError covers the
// engine-side half of the CRI-137 fail-fast: when the shim gives up waiting
// (stale pods can never verify), Verify fails and the engine must surface the
// accept_token error through init_failed and return it so the run fails
// terminally instead of retrying forever.
func TestInitScopeAdapters_VerifyFailureSurfacesAcceptTokenError(t *testing.T) {
	ctx := context.Background()
	g := perScopeRemoteGraph(t)
	dataDir := t.TempDir()

	sessions := adapterhost.NewSessionManager(&fakeLoader{})
	sessions.SetGraph(g)
	shim := &failingRemoteShim{fakeRemoteShim: newFakeRemoteShim(&fakeRemoteHandle{}), waitErr: errors.New(`remote adapter "noop" identity verification failed 150 consecutive times over 2m0s for scope ""; last error: accept_token verification failed for scope ""; failing the pending session wait instead of retrying indefinitely (stale adapter pod holding a pre-rotation accept token? CRI-137)`)}
	sessions.SetRemoteShim(shim)

	lifecycle := newScopeLifecycleState(dataDir)
	lifecycle.setRunID("run-123")
	sink := &eventTrackingSink{}
	rlc := &remoteLifecycleContext{scopeLifecycle: lifecycle}
	deps := Deps{Sessions: sessions, Sink: sink}

	_, err := initScopeAdapters(ctx, g, deps, nil, dataDir, "", nil, rlc)
	if err == nil {
		t.Fatal("expected initScopeAdapters to fail when the shim wait fails")
	}
	if !strings.Contains(err.Error(), "accept_token verification failed for scope") {
		t.Errorf("error must surface the accept_token failure, got: %v", err)
	}
	initFailed := false
	for _, s := range sink.lifecycleStatuses {
		if strings.HasSuffix(s, ":init_failed") {
			initFailed = true
		}
	}
	if !initFailed {
		t.Errorf("expected init_failed lifecycle event, got %v", sink.lifecycleStatuses)
	}
}

// failingRemoteShim behaves like fakeRemoteShim but fails the initial
// WaitForHandle, as the real shim does when the CRI-137 failure bound trips.
type failingRemoteShim struct {
	*fakeRemoteShim
	waitErr error
}

func (f *failingRemoteShim) WaitForHandle(_ context.Context, adapterType, scope string) (adapterhost.Handle, error) {
	f.record(fmt.Sprintf("WaitForHandle:%s:%s", adapterType, scope))
	return nil, f.waitErr
}

func (f *failingRemoteShim) WaitForFreshHandle(_ context.Context, adapterType, scope string, _ adapterhost.Handle) (adapterhost.Handle, error) {
	f.record(fmt.Sprintf("WaitForFreshHandle:%s:%s", adapterType, scope))
	return nil, f.waitErr
}
