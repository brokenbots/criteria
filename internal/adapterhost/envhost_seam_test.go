package adapterhost

// envhost_seam_test.go — pins the envhost seam consumption (ADR-0014 D1
// stage 1): a bound host serves the scope/kill/listen knobs and the remote
// dispatch branches; shim-only registrations fall back to the shim adapter
// with byte-identical semantics and error text; binding precedence, borrow
// propagation, scope-key round-trips, and the handle-recovery identity
// guards behave exactly as the seam contract declares.

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/brokenbots/criteria/internal/envhost"
)

var _ envhost.Host = shimHost{}

// seamHost is the scope bookkeeping path every resolver now takes: resolve
// the bound host of record for the environment, then drive its control
// arms. ok=false keeps the caller-owned failure lane (the pre-carve "no
// remote shim registered" error), so every seam test resolves through this
// path.
func seamHost(t *testing.T, m *SessionManager, envKey string) envhost.Host {
	t.Helper()
	host, ok := m.EnvHost(envKey)
	if !ok || host == nil {
		t.Fatalf("no bound host of record for %q", envKey)
	}
	return host
}

// seamRegister drives the register arm on the bound host of record for one
// scope/token, and fails the test when the arm errors.
func seamRegister(t *testing.T, m *SessionManager, envKey, scope, token string) {
	t.Helper()
	if _, err := seamHost(t, m, envKey).Control(context.Background(), &envhost.ControlRequest{
		Scope: scopeRefForKey(scope),
		Kind:  envhost.ControlRegisterScope,
		Token: token,
	}); err != nil {
		t.Fatalf("register scope %q on %q: %v", scope, envKey, err)
	}
}

// seamUnregister drives the unregister arm on the bound host of record for
// one scope, and fails the test when the arm errors.
func seamUnregister(t *testing.T, m *SessionManager, envKey, scope string) {
	t.Helper()
	if _, err := seamHost(t, m, envKey).Control(context.Background(), &envhost.ControlRequest{
		Scope: scopeRefForKey(scope),
		Kind:  envhost.ControlUnregisterScope,
	}); err != nil {
		t.Fatalf("unregister scope %q on %q: %v", scope, envKey, err)
	}
}

// seamCloseHandle drives the close arm on the bound host of record for one
// scope/definition, and fails the test when the arm errors.
func seamCloseHandle(t *testing.T, m *SessionManager, envKey, adapterName, scope string) {
	t.Helper()
	if _, err := seamHost(t, m, envKey).Control(context.Background(), &envhost.ControlRequest{
		Scope:   scopeRefForKey(scope),
		Adapter: adapterName,
		Kind:    envhost.ControlCloseHandle,
	}); err != nil {
		t.Fatalf("close handle %q on %q: %v", adapterName, envKey, err)
	}
}

// seamListenAddress reads the listen arm of the bound host of record.
func seamListenAddress(t *testing.T, m *SessionManager, envKey string) string {
	t.Helper()
	resp, err := seamHost(t, m, envKey).Control(context.Background(), &envhost.ControlRequest{Kind: envhost.ControlListenAddress})
	if err != nil {
		t.Fatalf("listen arm on %q: %v", envKey, err)
	}
	return resp.Address
}

// seamSpyHost records every seam call it receives so tests can assert knob
// routing and arm shapes. Configurable responses let the handle-recovery and
// error-propagation paths be exercised without a second host type.
type seamSpyHost struct {
	mu sync.Mutex

	ensureScopes []string
	ensureDefs   [][]envhost.Definition
	ensureErr    error
	ensureHandle Handle

	controlReqs []envhost.ControlRequest
	controlResp envhost.ControlResponse
	controlErr  error

	releaseScopes  []string
	releaseFates   []envhost.Fate
	journalScopes  []string
	journalAdapter string

	defersVerify bool
}

// DefersVerify is the VerifyDeferrer capability the verify path probes on
// hosts of record.
func (h *seamSpyHost) DefersVerify() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.defersVerify
}

func (h *seamSpyHost) Ensure(_ context.Context, scope envhost.ScopeRef, defs []envhost.Definition) (envhost.Handles, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ensureScopes = append(h.ensureScopes, scope.Key())
	h.ensureDefs = append(h.ensureDefs, defs)
	if h.ensureErr != nil {
		return nil, h.ensureErr
	}
	handles := make(envhost.Handles, len(defs))
	// A spy with no configured handle settles nothing — the unsettle path.
	if h.ensureHandle != nil {
		for _, def := range defs {
			handles[def.Name] = h.ensureHandle
		}
	}
	return handles, nil
}

func (h *seamSpyHost) Release(_ context.Context, scope envhost.ScopeRef, policy envhost.FatePolicy) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.releaseScopes = append(h.releaseScopes, scope.Key())
	h.releaseFates = append(h.releaseFates, policy.Fate)
	return nil
}

func (h *seamSpyHost) Journal(scope envhost.ScopeRef, adapter string) envhost.JournalReader {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.journalScopes = append(h.journalScopes, scope.Key())
	h.journalAdapter = adapter
	return shimJournalUnavailable{}
}

func (h *seamSpyHost) Control(_ context.Context, req *envhost.ControlRequest) (envhost.ControlResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.controlReqs = append(h.controlReqs, *req)
	return h.controlResp, h.controlErr
}

func (h *seamSpyHost) ensured() (scopes []string, defs [][]envhost.Definition) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.ensureScopes...), append([][]envhost.Definition(nil), h.ensureDefs...)
}

func (h *seamSpyHost) controls() []envhost.ControlRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]envhost.ControlRequest(nil), h.controlReqs...)
}

func TestEnvhostSeam_BoundHostServesScopeAndKillKnobs(t *testing.T) {
	g := cri293TwoEnvGraph()
	m := NewSessionManager(&cri269Loader{})
	m.SetGraph(g)
	beta := newCri293Shim("beta", "127.0.0.1:9002")
	spy := &seamSpyHost{controlResp: envhost.ControlResponse{Address: "10.0.0.1:1"}}
	m.SetEnvHostForEnv("remote.beta", NewShimHost(beta))
	m.SetEnvHostForEnv("remote.alpha", spy)

	seamRegister(t, m, "remote.alpha", "scope-a/1", "tok-a")
	ctrl := spy.controls()
	if len(ctrl) != 1 || ctrl[0].Kind != envhost.ControlRegisterScope {
		t.Fatalf("spy controls = %+v, want exactly one ControlRegisterScope", ctrl)
	}
	if ctrl[0].Scope.Key() != "scope-a/1" || ctrl[0].Token != "tok-a" {
		t.Errorf("register request = %+v, want scope scope-a/1 token tok-a", ctrl[0])
	}
	if got := beta.unregisterCalls(); len(got) != 0 {
		t.Errorf("beta scope calls = %v, want none", got)
	}

	seamUnregister(t, m, "remote.alpha", "scope-a/1")
	seamCloseHandle(t, m, "remote.alpha", "noop.alpha", "scope-a/1")
	if got := seamListenAddress(t, m, "remote.alpha"); got != "10.0.0.1:1" {
		t.Errorf("listen address of remote.alpha = %q, want 10.0.0.1:1", got)
	}
	ctrl = spy.controls()
	if len(ctrl) != 4 {
		t.Fatalf("spy controls = %d, want 4", len(ctrl))
	}
	for i, want := range []struct {
		kind    envhost.ControlKind
		scope   string
		adapter string
	}{
		{envhost.ControlUnregisterScope, "scope-a/1", ""},
		{envhost.ControlCloseHandle, "scope-a/1", "noop.alpha"},
		{envhost.ControlListenAddress, "", ""},
	} {
		got := ctrl[i+1]
		if got.Kind != want.kind || got.Scope.Key() != want.scope || got.Adapter != want.adapter {
			t.Errorf("control[%d] = %+v, want kind %v scope %q adapter %q", i, got, want.kind, want.scope, want.adapter)
		}
	}

	// The shim-registered environment keeps its shim behavior unchanged:
	// the shimHost fold serves the same arms with the shim underneath.
	seamRegister(t, m, "remote.beta", "scope-b/1", "tok-b")
	if got := beta.scopeToken("scope-b/1"); got != "tok-b" {
		t.Errorf("beta scope token = %q, want tok-b", got)
	}
}

func TestEnvhostSeam_ShimOnlyEnvsRouteThroughShimAdapter(t *testing.T) {
	g := cri293TwoEnvGraph()
	m := NewSessionManager(&cri269Loader{})
	m.SetGraph(g)
	alpha := newCri293Shim("alpha", "127.0.0.1:9001")
	m.SetEnvHostForEnv("remote.beta", &seamSpyHost{}) // decoy for the other env
	m.SetEnvHostForEnv("remote.alpha", NewShimHost(alpha))

	seamRegister(t, m, "remote.alpha", "scope-a/1", "tok-a")
	if got := alpha.scopeToken("scope-a/1"); got != "tok-a" {
		t.Errorf("alpha scope token = %q, want tok-a", got)
	}
	seamUnregister(t, m, "remote.alpha", "scope-a/1")
	if got := alpha.unregisterCalls(); len(got) != 1 || got[0] != "scope-a/1" {
		t.Errorf("alpha unregister calls = %v, want [scope-a/1]", got)
	}
	seamCloseHandle(t, m, "remote.alpha", "noop.alpha", "scope-a/1")
	if got := alpha.closeCalls(); len(got) != 1 || got[0] != "noop.alpha:scope-a/1" {
		t.Errorf("alpha close calls = %v, want [noop.alpha:scope-a/1]", got)
	}
	if got := seamListenAddress(t, m, "remote.alpha"); got != "127.0.0.1:9001" {
		t.Errorf("listen address of remote.alpha = %q, want 127.0.0.1:9001", got)
	}

	// An environment whose provisioning never binds a host of record leaves
	// the per-env register refused; the legacy scope-key lane still falls
	// back to the default shim (the legacy registration) — the pre-carve
	// fallback the single-shim callers rely on.
	m.SetRemoteShim(alpha)
	if err := m.RegisterRemoteScope("scope-c/1", "tok-c"); err != nil {
		t.Fatalf("default-shim fallback register: %v", err)
	}
	if got := alpha.scopeToken("scope-c/1"); got != "tok-c" {
		t.Errorf("default-shim fallback scope token = %q, want tok-c", got)
	}
	if got := m.RemoteListenAddr(); got != "127.0.0.1:9001" {
		t.Errorf("default-shim fallback listen = %q, want 127.0.0.1:9001", got)
	}
	errMsg := "no remote shim registered"
	empty := NewSessionManager(&cri269Loader{})
	empty.SetGraph(cri293TwoEnvGraph())
	if got := empty.RegisterRemoteScope("scope-c/1", "tok-c"); got == nil || got.Error() != errMsg {
		t.Errorf("register without any shim = %v, want %q", got, errMsg)
	}
	if got := empty.CloseRemoteHandle(context.Background(), "noop.alpha", "scope-c/1"); got != nil {
		t.Errorf("close handle without any shim = %v, want nil no-op", got)
	}
}

// TestEnvhostSeam_KnobControlErrorsPropagate pins the failure lane of the
// Control arms every scope/kill resolver drives: a bound host's Control
// error must surface verbatim (the dispatch Ensure error path is asserted
// separately in DispatchBranchesUseBoundHost).
func TestEnvhostSeam_KnobControlErrorsPropagate(t *testing.T) {
	m := NewSessionManager(&cri269Loader{})
	m.SetGraph(cri293TwoEnvGraph())
	ctrlErr := errors.New("control arm refused")
	spy := &seamSpyHost{controlErr: ctrlErr}
	m.SetEnvHostForEnv("remote.alpha", spy)

	if _, err := seamHost(t, m, "remote.alpha").Control(context.Background(), &envhost.ControlRequest{Scope: scopeRefForKey("scope-a/1"), Kind: envhost.ControlRegisterScope, Token: "tok-a"}); !errors.Is(err, ctrlErr) {
		t.Errorf("register arm error = %v, want verbatim control error", err)
	}
	if _, err := seamHost(t, m, "remote.alpha").Control(context.Background(), &envhost.ControlRequest{Scope: scopeRefForKey("scope-a/1"), Kind: envhost.ControlUnregisterScope}); !errors.Is(err, ctrlErr) {
		t.Errorf("unregister arm error = %v, want verbatim control error", err)
	}
	if _, err := seamHost(t, m, "remote.alpha").Control(context.Background(), &envhost.ControlRequest{Scope: scopeRefForKey("scope-a/1"), Adapter: "noop.alpha", Kind: envhost.ControlCloseHandle}); !errors.Is(err, ctrlErr) {
		t.Errorf("close arm error = %v, want verbatim control error", err)
	}
	if got := len(spy.controls()); got != 3 {
		t.Errorf("spy controls = %d, want one per failing arm (3)", got)
	}
}

func TestEnvhostSeam_LegacyDefaultKnobsRouteThroughSeam(t *testing.T) {
	g := cri293TwoEnvGraph()
	m := NewSessionManager(&cri269Loader{})
	m.SetGraph(g)
	legacy := newCri293Shim("legacy", "127.0.0.1:9000")
	m.SetRemoteShim(legacy)

	if err := m.RegisterRemoteScope("scope-x/1", "tok-x"); err != nil {
		t.Fatalf("RegisterRemoteScope: %v", err)
	}
	if got := legacy.scopeToken("scope-x/1"); got != "tok-x" {
		t.Errorf("legacy scope token = %q, want tok-x", got)
	}
	if err := m.UnregisterRemoteScope("scope-x/1"); err != nil {
		t.Fatalf("UnregisterRemoteScope: %v", err)
	}
	if got := legacy.unregisterCalls(); len(got) != 1 || got[0] != "scope-x/1" {
		t.Errorf("legacy unregister calls = %v, want [scope-x/1]", got)
	}
	if err := m.CloseRemoteHandle(context.Background(), "noop.alpha", "scope-x/1"); err != nil {
		t.Fatalf("CloseRemoteHandle: %v", err)
	}
	if got := legacy.closeCalls(); len(got) != 1 || got[0] != "noop.alpha:scope-x/1" {
		t.Errorf("legacy close calls = %v, want [noop.alpha:scope-x/1]", got)
	}
	if got := m.RemoteListenAddr(); got != "127.0.0.1:9000" {
		t.Errorf("RemoteListenAddr = %q, want 127.0.0.1:9000", got)
	}

	// The same knobs without any shim keep the legacy failure surfaces.
	empty := NewSessionManager(&cri269Loader{})
	empty.SetGraph(cri293TwoEnvGraph())
	errMsg := "no remote shim registered"
	if got := empty.RegisterRemoteScope("scope-x/1", "tok-x"); got == nil || got.Error() != errMsg {
		t.Errorf("RegisterRemoteScope without shim = %v, want %q", got, errMsg)
	}
	if got := empty.UnregisterRemoteScope("scope-x/1"); got == nil || got.Error() != errMsg {
		t.Errorf("UnregisterRemoteScope without shim = %v, want %q", got, errMsg)
	}
	if got := empty.CloseRemoteHandle(context.Background(), "noop.alpha", "scope-x/1"); got != nil {
		t.Errorf("CloseRemoteHandle without shim = %v, want nil no-op", got)
	}
	if got := empty.RemoteListenAddr(); got != "" {
		t.Errorf("RemoteListenAddr without shim = %q, want empty", got)
	}
}

func TestEnvhostSeam_BindingPrecedence(t *testing.T) {
	g := cri293TwoEnvGraph()

	// A later SetEnvHostForEnv over a shim-only (shimHost) registration
	// wins.
	m := NewSessionManager(&cri269Loader{})
	m.SetGraph(g)
	spy := &seamSpyHost{controlResp: envhost.ControlResponse{Address: "10.0.0.1:1"}}
	alpha := newCri293Shim("alpha", "127.0.0.1:9001")
	m.SetEnvHostForEnv("remote.alpha", NewShimHost(alpha))
	m.SetEnvHostForEnv("remote.alpha", spy)
	if got := seamListenAddress(t, m, "remote.alpha"); got != "10.0.0.1:1" {
		t.Errorf("bound host must win: got %q, want 10.0.0.1:1", got)
	}

	// A later NewShimHost bind re-shadows an earlier host binding with the
	// shim adapter — the latest registration wins, matching the shim map.
	m2 := NewSessionManager(&cri269Loader{})
	m2.SetGraph(g)
	spy2 := &seamSpyHost{controlResp: envhost.ControlResponse{Address: "10.0.0.1:1"}}
	alpha2 := newCri293Shim("alpha2", "127.0.0.1:9003")
	m2.SetEnvHostForEnv("remote.alpha", spy2)
	m2.SetEnvHostForEnv("remote.alpha", NewShimHost(alpha2))
	if got := seamListenAddress(t, m2, "remote.alpha"); got != "127.0.0.1:9003" {
		t.Errorf("re-shadowed shim must win: got %q, want 127.0.0.1:9003", got)
	}
	seamRegister(t, m2, "remote.alpha", "scope-b/1", "tok-b")
	if got := alpha2.scopeToken("scope-b/1"); got != "tok-b" {
		t.Errorf("re-shadowed shim scope token = %q, want tok-b", got)
	}
	for _, req := range spy2.controls() {
		t.Errorf("spy2 saw %+v after re-shadowing; the latest shim registration must own the env", req)
	}
}

func TestEnvhostSeam_ShareCarriesBoundHosts(t *testing.T) {
	src := NewSessionManager(&cri269Loader{})
	src.SetGraph(cri293TwoEnvGraph())
	spy := &seamSpyHost{}
	src.SetEnvHostForEnv("remote.alpha", spy)

	dst := NewSessionManager(&cri269Loader{})
	dst.SetGraph(cri293TwoEnvGraph())
	dst.ShareEnvHostsFrom(src)

	seamRegister(t, dst, "remote.alpha", "scope-a/1", "tok-a")
	ctrl := spy.controls()
	if len(ctrl) != 1 || ctrl[0].Kind != envhost.ControlRegisterScope || ctrl[0].Scope.Key() != "scope-a/1" {
		t.Fatalf("shared env routed to spy = %+v, want one register for scope-a/1", ctrl)
	}
}

func TestEnvhostSeam_HandleRecoveryAndIdentityGuards(t *testing.T) {
	handle := cri269Handle{}
	errEnsure := errors.New("ensure failed verbatim")

	spy := &seamSpyHost{ensureHandle: handle}
	got, err := ensureHostHandle(context.Background(), spy, "noop.alpha", "noop", "scope-a/1")
	if err != nil || got == nil {
		t.Fatalf("ensureHostHandle = %v, %v; want handle, nil", got, err)
	}
	scopes, defs := spy.ensured()
	if len(scopes) != 1 || scopes[0] != "scope-a/1" {
		t.Errorf("ensure scopes = %v, want [scope-a/1]", scopes)
	}
	// Both definition identities must travel: the wait keys on the instance
	// name while the bare adapter type rides along for host-side attribution
	// (a Name/Type swap here must be caught by this assertion pair).
	if len(defs) != 1 || len(defs[0]) != 1 || defs[0][0].Name != "noop.alpha" || defs[0][0].Type != "noop" {
		t.Errorf("ensure defs = %+v, want exactly [{Name:noop.alpha Type:noop}]", defs)
	}

	spyErr := &seamSpyHost{ensureErr: errEnsure}
	if got, err := ensureHostHandle(context.Background(), spyErr, "noop.alpha", "noop", "scope-a/1"); !errors.Is(err, errEnsure) || got != nil {
		t.Errorf("ensureHostHandle error path = %v, %v; want nil, verbatim ensure error", got, err)
	}

	spyNone := &seamSpyHost{}
	// A host that settles no handle for the definition must be refused.
	if got, err := ensureHostHandle(context.Background(), spyNone, "noop.alpha", "noop", "scope-a/1"); !errors.Is(err, errUnsettledHandle) || got != nil {
		t.Errorf("unsettled handle path = %v, %v; want nil, errUnsettledHandle", got, err)
	}
	// A handle foreign to the session layer's identity must be refused via
	// the fresh-wait recovery.
	foreignCtl := &seamSpyHost{controlResp: envhost.ControlResponse{Handle: 42}}
	if got, err := freshHostHandle(context.Background(), foreignCtl, "noop.alpha", "scope-a/1", nil); !errors.Is(err, errForeignHandleIdentity) || got != nil {
		t.Errorf("foreign handle recovery = %v, %v; want nil, errForeignHandleIdentity", got, err)
	}

	// The stale handle rides on the fresh-wait arm untouched.
	spyFresh := &seamSpyHost{controlResp: envhost.ControlResponse{Handle: handle}}
	if got, err := freshHostHandle(context.Background(), spyFresh, "noop.alpha", "scope-a/1", handle); err != nil || got == nil {
		t.Fatalf("freshHostHandle = %v, %v; want handle, nil", got, err)
	}
	ctl := spyFresh.controls()
	if len(ctl) != 1 || ctl[0].Kind != envhost.ControlEnsureFreshHandle {
		t.Fatalf("fresh controls = %+v, want one ControlEnsureFreshHandle", ctl)
	}
	if ctl[0].Scope.Key() != "scope-a/1" || ctl[0].Adapter != "noop.alpha" || ctl[0].Stale != Handle(handle) {
		t.Errorf("fresh request = %+v, want scope scope-a/1 adapter noop.alpha stale handle", ctl[0])
	}
}

// TestEnvhostSeam_ShimAdapterFreshWaitAndStaleGuard pins the shimHost fold
// of the EnsureFreshHandle arm: the stale identity guard trips before the
// shim, and a well-formed request reaches the shim's fresh wait.
func TestEnvhostSeam_ShimAdapterFreshWaitAndStaleGuard(t *testing.T) {
	alpha := newCri293Shim("alpha", "127.0.0.1:9001")
	host := shimHost{shim: alpha}

	// A stale handle that is not a session-layer handle is refused without
	// ever reaching the shim.
	resp, err := host.Control(context.Background(), &envhost.ControlRequest{
		Scope: scopeRefForKey("scope-a/1"),
		Kind:  envhost.ControlEnsureFreshHandle,
		Stale: 42,
	})
	if !errors.Is(err, errStaleHandleIdentity) || resp != (envhost.ControlResponse{}) {
		t.Errorf("shimHost stale guard = %+v, %v; want zero response, errStaleHandleIdentity", resp, err)
	}
	if got := alpha.freshWaitCalls(); len(got) != 0 {
		t.Errorf("shim fresh waits = %v, want none", got)
	}

	if resp, err := host.Control(context.Background(), &envhost.ControlRequest{
		Scope:   scopeRefForKey("scope-a/1"),
		Adapter: "noop.alpha",
		Kind:    envhost.ControlEnsureFreshHandle,
	}); err != nil || resp.Handle == nil {
		t.Fatalf("shimHost fresh wait = %+v, %v; want handle, nil", resp, err)
	}
	if got := alpha.freshWaitCalls(); len(got) != 1 || got[0] != "noop.alpha:scope-a/1" {
		t.Errorf("shim fresh waits = %v, want [noop.alpha:scope-a/1]", got)
	}
}

func TestEnvhostSeam_ScopeRefRoundTripThroughKnobs(t *testing.T) {
	for _, tc := range []struct {
		key  string
		want string
	}{
		{"", ""},
		{"/uuid-1", "/uuid-1"},
		{"scope-a/1", "scope-a/1"},
		{"a/b/c", "a/b/c"},
		{"plain", "plain"},
		// The trailing-slash shape does not exist in production, but the
		// folder must not mangle it either — the ref keeps the spelling.
		{"abc/", "abc/"},
	} {
		if got := scopeRefForKey(tc.key).Key(); got != tc.want {
			t.Errorf("scopeRefForKey(%q).Key() = %q, want %q", tc.key, got, tc.want)
		}
	}
}

func TestEnvhostSeam_DispatchBranchesUseBoundHost(t *testing.T) {
	g := cri293TwoEnvGraph()
	m := NewSessionManager(&cri269Loader{})
	m.SetGraph(g)
	spy := &seamSpyHost{
		ensureHandle: cri269Handle{},
		controlResp:  envhost.ControlResponse{Handle: cri269Handle{}},
	}
	alpha := newCri293Shim("alpha", "127.0.0.1:9001")
	m.SetEnvHostForEnv("remote.alpha", NewShimHost(alpha))
	m.SetEnvHostForEnv("remote.alpha", spy)

	ctx := context.Background()

	handle, err := m.resolveAdapterHandle(ctx, "noop.alpha", "noop", "scope-a/1", nil)
	if err != nil || handle == nil {
		t.Fatalf("resolveAdapterHandle = %v, %v; want handle, nil", handle, err)
	}
	scopes, defs := spy.ensured()
	if len(scopes) != 1 || scopes[0] != "scope-a/1" {
		t.Errorf("dispatch ensure scopes = %v, want [scope-a/1]", scopes)
	}
	if len(defs) != 1 || len(defs[0]) != 1 || defs[0][0].Name != "noop.alpha" || defs[0][0].Type != "noop" {
		t.Errorf("dispatch ensure defs = %+v, want [{Name:noop.alpha Type:noop}]", defs)
	}
	if got := alpha.waitForCalls(); len(got) != 0 {
		t.Errorf("shim waits = %v, want none (bound host owns dispatch)", got)
	}

	sess := &Session{Name: "noop.alpha", Adapter: "noop", ScopeInstanceID: "scope-a/1"}
	fresh, err := m.resolveAdapterForRespawn(ctx, sess, nil)
	if err != nil || fresh == nil {
		t.Fatalf("resolveAdapterForRespawn = %v, %v; want handle, nil", fresh, err)
	}
	ctl := spy.controls()
	if len(ctl) != 1 || ctl[0].Kind != envhost.ControlEnsureFreshHandle || ctl[0].Scope.Key() != "scope-a/1" || ctl[0].Adapter != "noop.alpha" {
		t.Errorf("respawn control = %+v, want one fresh-wait arm for scope-a/1 noop.alpha", ctl)
	}

	// Ensure failures propagate verbatim out of the dispatch branch.
	errEnsure := errors.New("ensure failed verbatim")
	m2 := NewSessionManager(&cri269Loader{})
	m2.SetGraph(cri293TwoEnvGraph())
	spy2 := &seamSpyHost{ensureErr: errEnsure}
	m2.SetEnvHostForEnv("remote.alpha", NewShimHost(alpha))
	m2.SetEnvHostForEnv("remote.alpha", spy2)
	if _, err := m2.resolveAdapterHandle(ctx, "noop.alpha", "noop", "scope-a/1", nil); !errors.Is(err, errEnsure) {
		t.Errorf("dispatch ensure error = %v, want verbatim ensure error", err)
	}
}
