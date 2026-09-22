package adapterhost

// sessions_cri293_test.go — CRI-293: a local run starts one remote shim per
// remote environment, so the session manager must route each adapter (and each
// scope-token registration) to the shim serving the adapter's environment
// instead of a single run-wide shim. These tests pin the sessions-level
// contract: per-env registration, per-env dispatch for binds and respawn,
// per-env listen-address publication, legacy default-shim fallback, and
// Shutdown stopping every registered shim.

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/brokenbots/criteria/workflow"
)

// cri293Shim records which calls it received so tests can assert per-env
// routing. ListenAddr is configurable so per-env address publication can be
// asserted.
type cri293Shim struct {
	name    string
	addr    string
	stopped int

	mu         sync.Mutex
	waits      []string
	freshWaits []string
	scopes     map[string]string
	unscoped   []string
	closed     []string
}

func newCri293Shim(name, addr string) *cri293Shim {
	return &cri293Shim{name: name, addr: addr, scopes: map[string]string{}}
}

func (s *cri293Shim) WaitForHandle(_ context.Context, adapterType, scope string) (Handle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.waits = append(s.waits, adapterType+":"+scope)
	return cri269Handle{}, nil
}

func (s *cri293Shim) WaitForFreshHandle(_ context.Context, adapterType, scope string, _ Handle) (Handle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.freshWaits = append(s.freshWaits, adapterType+":"+scope)
	return cri269Handle{}, nil
}

func (s *cri293Shim) RegisterScope(scope, token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scopes[scope] = token
}

func (s *cri293Shim) UnregisterScope(scope string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unscoped = append(s.unscoped, scope)
}

func (s *cri293Shim) CloseHandle(_ context.Context, adapterType, scope string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = append(s.closed, adapterType+":"+scope)
	return nil
}

func (s *cri293Shim) ListenAddr() string { return s.addr }

func (s *cri293Shim) Stop(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopped++
	return nil
}

func (s *cri293Shim) waitForCalls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.waits...)
}

func (s *cri293Shim) freshWaitCalls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.freshWaits...)
}

func (s *cri293Shim) scopeToken(scope string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.scopes[scope]
}

func (s *cri293Shim) unregisterCalls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.unscoped...)
}

func (s *cri293Shim) closeCalls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.closed...)
}

func (s *cri293Shim) stopCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped
}

// cri293TwoEnvGraph returns a root graph declaring two remote environments
// (the CRI-293 shape) with one adapter bound to each.
func cri293TwoEnvGraph() *workflow.FSMGraph {
	return &workflow.FSMGraph{
		Adapters: map[string]*workflow.AdapterNode{
			"noop.alpha": {Name: "noop.alpha", Type: "noop", Environment: "remote.alpha"},
			"noop.beta":  {Name: "noop.beta", Type: "noop", Environment: "remote.beta"},
		},
		AdapterOrder: []string{"noop.alpha", "noop.beta"},
		Environments: map[string]*workflow.EnvironmentNode{
			"remote.alpha": {Type: "remote", Name: "alpha"},
			"remote.beta":  {Type: "remote", Name: "beta"},
		},
	}
}

func TestSessions_CRI293_PerEnvShimDispatch(t *testing.T) {
	g := cri293TwoEnvGraph()
	m := NewSessionManager(&cri269Loader{})
	m.SetGraph(g)
	alpha := newCri293Shim("alpha", "127.0.0.1:9001")
	beta := newCri293Shim("beta", "127.0.0.1:9002")
	m.SetRemoteShimForEnv("remote.alpha", alpha)
	m.SetRemoteShimForEnv("remote.beta", beta)

	ctx := context.Background()
	for _, tc := range []struct {
		instance, adapter string
		shim              *cri293Shim
	}{
		{"noop.alpha", "noop", alpha},
		{"noop.beta", "noop", beta},
	} {
		if _, err := m.resolveAdapterHandle(ctx, tc.instance, tc.adapter, "", nil); err != nil {
			t.Fatalf("resolveAdapterHandle(%s): %v", tc.instance, err)
		}
	}

	if got := alpha.waitForCalls(); len(got) != 1 || got[0] != "noop:" {
		t.Errorf("alpha shim WaitForHandle calls = %v, want exactly [noop:]", got)
	}
	if got := beta.waitForCalls(); len(got) != 1 || got[0] != "noop:" {
		t.Errorf("beta shim WaitForHandle calls = %v, want exactly [noop:]", got)
	}
}

func TestSessions_CRI293_PerEnvRespawnDispatch(t *testing.T) {
	g := cri293TwoEnvGraph()
	m := NewSessionManager(&cri269Loader{})
	m.SetGraph(g)
	alpha := newCri293Shim("alpha", "127.0.0.1:9001")
	beta := newCri293Shim("beta", "127.0.0.1:9002")
	m.SetRemoteShimForEnv("remote.alpha", alpha)
	m.SetRemoteShimForEnv("remote.beta", beta)

	ctx := context.Background()
	sess := &Session{Name: "noop.beta", Adapter: "noop"}
	if _, err := m.resolveAdapterForRespawn(ctx, sess, nil); err != nil {
		t.Fatalf("resolveAdapterForRespawn: %v", err)
	}
	if got := beta.freshWaitCalls(); len(got) != 1 || got[0] != "noop:" {
		t.Errorf("beta shim WaitForFreshHandle calls = %v, want exactly [noop:]", got)
	}
	if got := alpha.freshWaitCalls(); len(got) != 0 {
		t.Errorf("alpha shim WaitForFreshHandle calls = %v, want none", got)
	}
}

func TestSessions_CRI293_PerEnvScopeRegistration(t *testing.T) {
	g := cri293TwoEnvGraph()
	m := NewSessionManager(&cri269Loader{})
	m.SetGraph(g)
	alpha := newCri293Shim("alpha", "127.0.0.1:9001")
	beta := newCri293Shim("beta", "127.0.0.1:9002")
	m.SetRemoteShimForEnv("remote.alpha", alpha)
	m.SetRemoteShimForEnv("remote.beta", beta)

	if err := m.RegisterRemoteScopeForEnv("remote.alpha", "scope-a/1", "tok-a"); err != nil {
		t.Fatalf("RegisterRemoteScopeForEnv(alpha): %v", err)
	}
	if err := m.RegisterRemoteScopeForEnv("remote.beta", "scope-b/1", "tok-b"); err != nil {
		t.Fatalf("RegisterRemoteScopeForEnv(beta): %v", err)
	}
	if got := alpha.scopeToken("scope-a/1"); got != "tok-a" {
		t.Errorf("alpha scope token = %q, want tok-a", got)
	}
	if got := beta.scopeToken("scope-b/1"); got != "tok-b" {
		t.Errorf("beta scope token = %q, want tok-b", got)
	}
	if err := m.UnregisterRemoteScopeForEnv("remote.beta", "scope-b/1"); err != nil {
		t.Fatalf("UnregisterRemoteScopeForEnv(beta): %v", err)
	}
	if got := beta.unregisterCalls(); len(got) != 1 || got[0] != "scope-b/1" {
		t.Errorf("beta unregister calls = %v, want [scope-b/1]", got)
	}
	if got := alpha.unregisterCalls(); len(got) != 0 {
		t.Errorf("alpha unregister calls = %v, want none", got)
	}
	ctx := context.Background()
	if err := m.CloseRemoteHandleForEnv(ctx, "remote.alpha", "noop", "scope-a/1"); err != nil {
		t.Fatalf("CloseRemoteHandleForEnv(alpha): %v", err)
	}
	if got := alpha.closeCalls(); len(got) != 1 || got[0] != "noop:scope-a/1" {
		t.Errorf("alpha close calls = %v, want [noop:scope-a/1]", got)
	}

	// An environment without a dedicated shim falls back to the default
	// shim (the latest registration) — that is the legacy single-shim
	// behavior every pre-CRI-293 caller relies on.
	if got := m.RegisterRemoteScopeForEnv("remote.gamma", "scope-c/1", "tok-c"); got != nil {
		t.Errorf("RegisterRemoteScopeForEnv(gamma) fallback: %v", got)
	}
}

func TestSessions_CRI293_PerEnvListenAddrPublication(t *testing.T) {
	g := cri293TwoEnvGraph()
	m := NewSessionManager(&cri269Loader{})
	m.SetGraph(g)
	alpha := newCri293Shim("alpha", "127.0.0.1:9001")
	beta := newCri293Shim("beta", "127.0.0.1:9002")
	m.SetRemoteShimForEnv("remote.alpha", alpha)
	m.SetRemoteShimForEnv("remote.beta", beta)

	if got := m.RemoteListenAddrForEnv("remote.alpha"); got != "127.0.0.1:9001" {
		t.Errorf("RemoteListenAddrForEnv(alpha) = %q, want 127.0.0.1:9001", got)
	}
	if got := m.RemoteListenAddrForEnv("remote.beta"); got != "127.0.0.1:9002" {
		t.Errorf("RemoteListenAddrForEnv(beta) = %q, want 127.0.0.1:9002", got)
	}
	if got := m.RemoteListenAddrForEnv("remote.missing"); got != "127.0.0.1:9002" {
		// Missing env keys fall back to the default shim (the latest
		// registration) so legacy single-shim callers keep their address.
		t.Errorf("RemoteListenAddrForEnv(missing) fallback = %q, want 127.0.0.1:9002", got)
	}
}

func TestSessions_CRI293_LegacyDefaultShimFallback(t *testing.T) {
	g := cri293TwoEnvGraph()
	m := NewSessionManager(&cri269Loader{})
	m.SetGraph(g)
	legacy := newCri293Shim("legacy", "127.0.0.1:9000")
	m.SetRemoteShim(legacy)

	// With only the legacy default shim registered, every remote adapter
	// dispatches to it — the pre-CRI-293 behavior for existing callers.
	ctx := context.Background()
	if _, err := m.resolveAdapterHandle(ctx, "noop.alpha", "noop", "", nil); err != nil {
		t.Fatalf("resolveAdapterHandle(alpha): %v", err)
	}
	if got := legacy.waitForCalls(); len(got) != 1 || got[0] != "noop:" {
		t.Errorf("legacy shim WaitForHandle calls = %v, want exactly [noop:]", got)
	}
	if err := m.RegisterRemoteScope("scope-x/1", "tok-x"); err != nil {
		t.Fatalf("RegisterRemoteScope: %v", err)
	}
	if got := legacy.scopeToken("scope-x/1"); got != "tok-x" {
		t.Errorf("legacy scope token = %q, want tok-x", got)
	}

	// Registering a dedicated env shim later keeps the default for adapters
	// in environments that have none.
	dedicated := newCri293Shim("dedicated", "127.0.0.1:9003")
	m.SetRemoteShimForEnv("remote.alpha", dedicated)
	if got := m.RemoteListenAddrForEnv("remote.beta"); got != "127.0.0.1:9003" {
		// The default shim now points at the latest registration; beta has
		// no dedicated entry so it falls back to it.
		t.Errorf("RemoteListenAddrForEnv(beta) fallback = %q, want 127.0.0.1:9003", got)
	}
}

func TestSessions_CRI293_ShutdownStopsAllShims(t *testing.T) {
	g := cri293TwoEnvGraph()
	m := NewSessionManager(&cri269Loader{})
	m.SetGraph(g)
	alpha := newCri293Shim("alpha", "127.0.0.1:9001")
	beta := newCri293Shim("beta", "127.0.0.1:9002")
	m.SetRemoteShimForEnv("remote.alpha", alpha)
	m.SetRemoteShimForEnv("remote.beta", beta)

	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	// Every registered shim must be stopped. The default shim aliases one
	// environment's shim; since RemoteShim implementations cannot be
	// assumed comparable, duplicates are kept and receive a second Stop,
	// which must be a no-op (Shim.Stop idempotency contract). A shim
	// registered only as the environment shim is stopped exactly once.
	for _, s := range []*cri293Shim{alpha, beta} {
		if got := s.stopCount(); got < 1 {
			t.Errorf("%s shim stop count = %d, want >= 1", s.name, got)
		}
	}
	if got := beta.stopCount(); got > 2 {
		t.Errorf("%s shim stop count = %d, want <= 2 (default alias is a duplicate)", beta.name, got)
	}
}

func TestSessions_CRI293_ShutdownCollectsStopErrors(t *testing.T) {
	g := cri293TwoEnvGraph()
	m := NewSessionManager(&cri269Loader{})
	m.SetGraph(g)
	stopErr := errors.New("boom")
	alpha := newCri293Shim("alpha", "127.0.0.1:9001")
	beta := &crashStopShim{cri293Shim: *newCri293Shim("beta", "127.0.0.1:9002"), err: stopErr}
	m.SetRemoteShimForEnv("remote.alpha", alpha)
	m.SetRemoteShimForEnv("remote.beta", beta)

	if err := m.Shutdown(context.Background()); err == nil {
		t.Fatal("Shutdown with a failing shim: want error, got nil")
	}
}

// crashStopShim is a cri293Shim whose Stop always fails.
type crashStopShim struct {
	cri293Shim
	err error
}

func (s *crashStopShim) Stop(context.Context) error { return s.err }
