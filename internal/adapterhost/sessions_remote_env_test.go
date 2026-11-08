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
	m.SetEnvHostForEnv("remote.alpha", NewShimHost(alpha))
	m.SetEnvHostForEnv("remote.beta", NewShimHost(beta))

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

	if got := alpha.waitForCalls(); len(got) != 1 || got[0] != "noop.alpha:" {
		t.Errorf("alpha shim WaitForHandle calls = %v, want exactly [noop.alpha:]", got)
	}
	if got := beta.waitForCalls(); len(got) != 1 || got[0] != "noop.beta:" {
		t.Errorf("beta shim WaitForHandle calls = %v, want exactly [noop.beta:]", got)
	}
}

func TestSessions_CRI293_PerEnvRespawnDispatch(t *testing.T) {
	g := cri293TwoEnvGraph()
	m := NewSessionManager(&cri269Loader{})
	m.SetGraph(g)
	alpha := newCri293Shim("alpha", "127.0.0.1:9001")
	beta := newCri293Shim("beta", "127.0.0.1:9002")
	m.SetEnvHostForEnv("remote.alpha", NewShimHost(alpha))
	m.SetEnvHostForEnv("remote.beta", NewShimHost(beta))

	ctx := context.Background()
	sess := &Session{Name: "noop.beta", Adapter: "noop"}
	if _, err := m.resolveAdapterForRespawn(ctx, sess, nil); err != nil {
		t.Fatalf("resolveAdapterForRespawn: %v", err)
	}
	if got := beta.freshWaitCalls(); len(got) != 1 || got[0] != "noop.beta:" {
		t.Errorf("beta shim WaitForFreshHandle calls = %v, want exactly [noop.beta:]", got)
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
	m.SetEnvHostForEnv("remote.alpha", NewShimHost(alpha))
	m.SetEnvHostForEnv("remote.beta", NewShimHost(beta))

	seamRegister(t, m, "remote.alpha", "scope-a/1", "tok-a")
	seamRegister(t, m, "remote.beta", "scope-b/1", "tok-b")
	if got := alpha.scopeToken("scope-a/1"); got != "tok-a" {
		t.Errorf("alpha scope token = %q, want tok-a", got)
	}
	if got := beta.scopeToken("scope-b/1"); got != "tok-b" {
		t.Errorf("beta scope token = %q, want tok-b", got)
	}
	seamUnregister(t, m, "remote.beta", "scope-b/1")
	if got := beta.unregisterCalls(); len(got) != 1 || got[0] != "scope-b/1" {
		t.Errorf("beta unregister calls = %v, want [scope-b/1]", got)
	}
	if got := alpha.unregisterCalls(); len(got) != 0 {
		t.Errorf("alpha unregister calls = %v, want none", got)
	}
	seamCloseHandle(t, m, "remote.alpha", "noop", "scope-a/1")
	if got := alpha.closeCalls(); len(got) != 1 || got[0] != "noop:scope-a/1" {
		t.Errorf("alpha close calls = %v, want [noop:scope-a/1]", got)
	}

	// An environment without a host of record refuses the registration:
	// the engine binds a host of record for every declared remote
	// environment, and the per-env register path has no shim-less fallback
	// past the seam. The legacy scope-key lane answers only the default
	// shim, which is unregistered here — the same fail-closed surface the
	// engine sees for a missing host.
	if err := m.RegisterRemoteScope("scope-c/1", "tok-c"); err == nil || err.Error() != "no remote shim registered" {
		t.Errorf("register without any host = %v, want \"no remote shim registered\"", err)
	}
}

func TestSessions_CRI293_PerEnvListenAddrPublication(t *testing.T) {
	g := cri293TwoEnvGraph()
	m := NewSessionManager(&cri269Loader{})
	m.SetGraph(g)
	alpha := newCri293Shim("alpha", "127.0.0.1:9001")
	beta := newCri293Shim("beta", "127.0.0.1:9002")
	m.SetEnvHostForEnv("remote.alpha", NewShimHost(alpha))
	m.SetEnvHostForEnv("remote.beta", NewShimHost(beta))

	if got := seamListenAddress(t, m, "remote.alpha"); got != "127.0.0.1:9001" {
		t.Errorf("listen address of remote.alpha = %q, want 127.0.0.1:9001", got)
	}
	if got := seamListenAddress(t, m, "remote.beta"); got != "127.0.0.1:9002" {
		t.Errorf("listen address of remote.beta = %q, want 127.0.0.1:9002", got)
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
	if got := legacy.waitForCalls(); len(got) != 1 || got[0] != "noop.alpha:" {
		t.Errorf("legacy shim WaitForHandle calls = %v, want exactly [noop.alpha:]", got)
	}
	if err := m.RegisterRemoteScope("scope-x/1", "tok-x"); err != nil {
		t.Fatalf("RegisterRemoteScope: %v", err)
	}
	if got := legacy.scopeToken("scope-x/1"); got != "tok-x" {
		t.Errorf("legacy scope token = %q, want tok-x", got)
	}

	// Registering a dedicated env host later keeps the default lane only
	// for environments that have no dedicated host.
	dedicated := newCri293Shim("dedicated", "127.0.0.1:9003")
	m.SetEnvHostForEnv("remote.alpha", NewShimHost(dedicated))
	if got := m.RemoteShimForEnv("remote.alpha"); got != dedicated {
		t.Errorf("RemoteShimForEnv(alpha) after the dedicated bind = %v, want the dedicated shim", got)
	}
	if got := m.RemoteShimForEnv("remote.beta"); got != legacy {
		t.Errorf("RemoteShimForEnv(beta) fallback = %v, want the legacy default shim", got)
	}
}

func TestSessions_CRI293_ShutdownStopsAllShims(t *testing.T) {
	g := cri293TwoEnvGraph()
	m := NewSessionManager(&cri269Loader{})
	m.SetGraph(g)
	alpha := newCri293Shim("alpha", "127.0.0.1:9001")
	beta := newCri293Shim("beta", "127.0.0.1:9002")
	m.SetEnvHostForEnv("remote.alpha", NewShimHost(alpha))
	m.SetEnvHostForEnv("remote.beta", NewShimHost(beta))

	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	// Every bound shim must be stopped (Shutdown walks the shim substrate
	// behind each bound host). Duplicate folds (shim under several hosts,
	// default aliasing) receive a second Stop, which must be a no-op
	// (Shim.Stop idempotency contract); a distinct shim is stopped at least
	// once.
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
	m.SetEnvHostForEnv("remote.alpha", NewShimHost(alpha))
	m.SetEnvHostForEnv("remote.beta", NewShimHost(beta))

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
