package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow"
)

// kb232PerScopeTwoAdapterGraph compiles a workflow whose two remote adapters
// are bound to one per_scope_sessions environment, mirroring the
// sub-workflow shape that burst-provisions a pod fleet.
func kb232PerScopeTwoAdapterGraph(t *testing.T) *workflow.FSMGraph {
	t.Helper()
	return compile(t, `
workflow {
  name = "kb232-two-adapter"
  version = "0.1"
  initial_state = "start"
  target_state  = "done"
}

environment "remote" "prod" {
  listen_address     = "127.0.0.1:0"
  per_scope_sessions = true
}

adapter "shell" "intake" {
  environment = remote.prod
}

adapter "copilot" "planner" {
  environment = remote.prod
}

step "start" {
  target = adapter.shell.intake
  outcome "success" { next = step.done }
}

state "done" {
  terminal = true
  success  = true
}`)
}

// kb232TokenPaths globs every rotated token file under the run data dir.
// Tokens live at remote-tokens/<scope>/<instance>/<type>.token, with the
// root workflow's empty scope name collapsing out (filepath.Join): the root
// shape is remote-tokens/<instance>/<type>.token.
func kb232TokenPaths(dataDir string) []string {
	patterns := []string{
		filepath.Join(dataDir, "remote-tokens", "*", "*.token"),
		filepath.Join(dataDir, "remote-tokens", "*", "*", "*.token"),
	}
	var out []string
	for _, pattern := range patterns {
		matches, _ := filepath.Glob(pattern)
		out = append(out, matches...)
	}
	return out
}

// TestRotateFreshRemoteScope_RegistersScopeBeforeTokenFileVisible pins the
// KB-232 ordering for contract item 1: when the shim's scope registry learns
// a freshly rotated scope, its accept token file must not be visible on disk
// yet. With the old disk-first order a pod that picked up the token file at
// the moment it appeared could only dial an unregistered scope (the engine
// registered the scope afterwards) and the KB-25 dial registrar refuses to
// re-register an instance a current record claims — so the step burned its
// deadline on "scope not registered" rejections.
func TestRotateFreshRemoteScope_RegistersScopeBeforeTokenFileVisible(t *testing.T) {
	g := perScopeRemoteGraph(t)
	dataDir := t.TempDir()

	sessions := adapterhost.NewSessionManager(&fakeLoader{})
	sessions.SetGraph(g)
	shim := newFakeRemoteShim(&fakeRemoteHandle{})
	shim.onRegister = func(scope, token string) {
		if visible := kb232TokenPaths(dataDir); len(visible) != 0 {
			t.Errorf("token file(s) already visible at shim registration time: %v", visible)
		}
	}
	sessions.SetRemoteShim(shim)

	lifecycle := newScopeLifecycleState(dataDir)
	lifecycle.setRunID("run-kb232-order")
	sink := &eventTrackingSink{}
	rlc := &remoteLifecycleContext{scopeLifecycle: lifecycle}
	deps := Deps{Sessions: sessions, Sink: sink}

	adapter := g.Adapters[g.AdapterOrder[0]]
	scopeKey, err := maybeRotateRemoteScope(deps, rlc, g, adapter, g.AdapterOrder[0], "")
	if err != nil {
		t.Fatalf("maybeRotateRemoteScope: %v", err)
	}
	if scopeKey == "" {
		t.Fatal("expected a per-scope verify scope key")
	}
	registeredToken := shim.registeredToken(scopeKey)
	if registeredToken == "" {
		t.Fatal("shim must hold the rotated scope token when rotation returns")
	}

	matches := kb232TokenPaths(dataDir)
	if len(matches) != 1 {
		t.Fatalf("token file must exist after rotation (matches=%v)", matches)
	}
	tokenBytes, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatalf("read token file: %v", err)
	}
	if string(tokenBytes) != registeredToken {
		t.Errorf("persisted token %q, want the shim-registered token %q", string(tokenBytes), registeredToken)
	}
}

// TestRotateFreshRemoteScope_PersistFailure_UnregistersScope pins the
// rollback side of the reordered rotation: when persisting the rotated token
// or the current-instance record fails after the shim registration, the scope
// must be unregistered again and no provision_wanted may be emitted (an
// operator would otherwise provision a pod for a token the runner cleaned
// up).
func TestRotateFreshRemoteScope_PersistFailure_UnregistersScope(t *testing.T) {
	g := perScopeRemoteGraph(t)
	dataDir := t.TempDir()

	sessions := adapterhost.NewSessionManager(&fakeLoader{})
	sessions.SetGraph(g)
	shim := newFakeRemoteShim(&fakeRemoteHandle{})
	sessions.SetRemoteShim(shim)

	lifecycle := newScopeLifecycleState(dataDir)
	lifecycle.setRunID("run-kb232-rollback")
	sink := &eventTrackingSink{}
	rlc := &remoteLifecycleContext{scopeLifecycle: lifecycle}
	deps := Deps{Sessions: sessions, Sink: sink}

	adapter := g.Adapters[g.AdapterOrder[0]]

	// Block the current-instance record write: a file where the record
	// directory must be created makes the atomic record write fail.
	scopeDir := filepath.Join(dataDir, "remote-tokens")
	if err := os.MkdirAll(scopeDir, 0o700); err != nil {
		t.Fatalf("seed scope dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(scopeDir, "current"), []byte("blocker"), 0o600); err != nil {
		t.Fatalf("seed record blocker: %v", err)
	}

	_, err := maybeRotateRemoteScope(deps, rlc, g, adapter, g.AdapterOrder[0], "")
	if err == nil {
		t.Fatal("expected rotation to fail on the blocked record write")
	}
	sink.mu.Lock()
	statuses := append([]string(nil), sink.lifecycleStatuses...)
	sink.mu.Unlock()
	hasInitFailed := false
	for _, s := range statuses {
		if strings.HasSuffix(s, ":init_failed") {
			hasInitFailed = true
		}
	}
	if !hasInitFailed {
		t.Errorf("expected an init_failed lifecycle status, got %v", statuses)
	}
	if sink.hasStatus("provision_wanted") {
		t.Errorf("provision_wanted must not be emitted when rotation fails, got %v", sink.provisionEvents)
	}
	if got := len(shim.unregistered); got != 1 {
		t.Fatalf("expected the failed rotation's scope to be unregistered once, got %d (%v)", got, shim.unregistered)
	}
	if shim.unregistered[0] == "" {
		t.Errorf("unregistered scope key must not be empty")
	}
}

// TestInitScopeAdapters_TwoPhase_RegistersAllScopesBeforeAnyVerify pins the
// KB-232 two-phase split: every adapter's per-scope registration happens in
// phase A (prepare + rotate + register) and the first blocking handshake
// (phase B Verify → WaitForHandle) starts only after all scopes are
// registered with the shim. With the old interleaved order, adapter N+1's
// registration was gated behind adapter N's handshake, leaving
// burst-provisioned pods dialing unregistered scopes for as long as the
// earlier handshakes took.
func TestInitScopeAdapters_TwoPhase_RegistersAllScopesBeforeAnyVerify(t *testing.T) {
	ctx := context.Background()
	g := kb232PerScopeTwoAdapterGraph(t)
	dataDir := t.TempDir()

	sessions := adapterhost.NewSessionManager(&fakeLoader{})
	sessions.SetGraph(g)
	shim := newFakeRemoteShim(&fakeRemoteHandle{})
	sessions.SetRemoteShim(shim)

	lifecycle := newScopeLifecycleState(dataDir)
	lifecycle.setRunID("run-kb232-two-phase")
	sink := &eventTrackingSink{}
	rlc := &remoteLifecycleContext{scopeLifecycle: lifecycle}
	deps := Deps{Sessions: sessions, Sink: sink}

	order, err := initScopeAdapters(ctx, g, deps, nil, dataDir, "run_handler", nil, rlc)
	if err != nil {
		t.Fatalf("initScopeAdapters: %v", err)
	}
	if len(order) != 2 {
		t.Fatalf("expected 2 adapters provisioned, got %v", order)
	}

	shim.mu.Lock()
	calls := append([]string(nil), shim.calls...)
	shim.mu.Unlock()

	lastRegister, firstWait := -1, -1
	registrations, waits := 0, 0
	for i, call := range calls {
		switch {
		case strings.HasPrefix(call, "RegisterScope:"):
			registrations++
			lastRegister = i
		case strings.HasPrefix(call, "WaitForHandle:"):
			waits++
			if firstWait == -1 {
				firstWait = i
			}
		}
	}
	if registrations != 2 {
		t.Fatalf("expected 2 scope registrations, got %d in %v", registrations, calls)
	}
	if waits == 0 {
		t.Fatalf("expected WaitForHandle calls for both adapters, calls=%v", calls)
	}
	if lastRegister > firstWait {
		t.Fatalf("scope registrations must complete before the first verify handshake: first WaitForHandle at %d, last RegisterScope at %d in %v", firstWait, lastRegister, calls)
	}

	provisionedCount := 0
	for _, ev := range sink.provisionEvents {
		if ev.Status == "provision_wanted" {
			provisionedCount++
		}
	}
	if provisionedCount != 2 {
		t.Errorf("expected 2 provision_wanted events, got %d (%v)", provisionedCount, sink.provisionEvents)
	}
}