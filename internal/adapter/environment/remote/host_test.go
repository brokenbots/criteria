package remote

// host_test.go — pins the remote environment's envhost.Host binding
// (ADR-0014 D1 stage 1): the control arms route through the environment's
// provider into the shim's token store and session waits exactly where the
// pre-carve session-manager knobs did, the release lanes match the declared
// fate, and the journal reports unavailable so ledger consumers fall back to
// the engine-owned surfaces.

import (
	"context"
	"errors"
	"testing"

	"github.com/brokenbots/criteria/internal/envhost"
)

var _ envhost.Host = NewHost(nil, nil)

// newHostEnvHarness builds the production wiring shape — the shim, a peer
// session provider wired as its acceptor (the engine's arrangement), and the
// host binding over both — without starting a listener, so the token-store
// and arm-routing arms can be asserted in isolation.
func newHostEnvHarness(t *testing.T, perScope bool) (envhost.Host, *Server, *peerSessionProvider) {
	t.Helper()
	shim, err := NewShim(nil, &fixedDigestVerifier{allowed: map[string]string{}})
	if err != nil {
		t.Fatalf("NewShim: %v", err)
	}
	prov := NewPeerSessionProvider(shim, perScope)
	shim.SetPeerAcceptor(prov)
	return NewHost(shim, prov), shim, prov
}

func TestHostEnv_ControlScopeArms(t *testing.T) {
	host, shim, prov := newHostEnvHarness(t, true)

	if _, err := host.Control(context.Background(), &envhost.ControlRequest{
		Scope: envhost.ScopeRef{Scope: "alpha", Instance: "uuid-1"},
		Kind:  envhost.ControlRegisterScope,
		Token: "tok-1",
	}); err != nil {
		t.Fatalf("register arm: %v", err)
	}
	if got := shim.scopeTokens["alpha/uuid-1"]; got != "tok-1" {
		t.Errorf("shim token store = %q, want tok-1", got)
	}
	// The provider's per-scope key shaping must agree with the shim's
	// snapshot: the acceptor contract requires the two configurations to
	// match (see NewPeerSessionProvider).
	if got, want := prov.key("noop", "alpha/uuid-1"), shim.sessionKey("noop", "alpha/uuid-1"); got != want {
		t.Errorf("provider key %q != shim key %q (perScopeSessions snapshot mismatch)", got, want)
	}
	if prov == nil {
		t.Fatal("per-scope provider must be wired")
	}

	if _, err := host.Control(context.Background(), &envhost.ControlRequest{
		Scope: envhost.ScopeRef{Scope: "alpha", Instance: "uuid-1"},
		Kind:  envhost.ControlUnregisterScope,
	}); err != nil {
		t.Fatalf("unregister arm: %v", err)
	}
	if got, ok := shim.scopeTokens["alpha/uuid-1"]; ok {
		t.Errorf("shim token store after unregister = %q, want absent", got)
	}

	// The root scope shape rides through the same store byte-for-byte.
	if _, err := host.Control(context.Background(), &envhost.ControlRequest{
		Scope: envhost.ScopeRef{Instance: "uuid-2"},
		Kind:  envhost.ControlRegisterScope,
		Token: "tok-2",
	}); err != nil {
		t.Fatalf("root scope register arm: %v", err)
	}
	if got := shim.scopeTokens["/uuid-2"]; got != "tok-2" {
		t.Errorf("root scope token = %q, want tok-2", got)
	}
}

func TestHostEnv_ListenAddressAndCloseArms(t *testing.T) {
	host, _, _ := newHostEnvHarness(t, false)

	resp, err := host.Control(context.Background(), &envhost.ControlRequest{Kind: envhost.ControlListenAddress})
	if err != nil || resp.Address != "" {
		t.Errorf("listen address arm on an unstarted host = %+v, %v; want empty address, nil", resp, err)
	}

	// Close with no stored session is a no-op, matching the pre-carve knob.
	resp, err = host.Control(context.Background(), &envhost.ControlRequest{
		Scope:   envhost.ScopeRef{Scope: "alpha", Instance: "uuid-1"},
		Adapter: "noop.alpha",
		Kind:    envhost.ControlCloseHandle,
	})
	if err != nil || resp != (envhost.ControlResponse{}) {
		t.Errorf("close arm on an empty host = %+v, %v; want zero response, nil", resp, err)
	}
}

func TestHostEnv_EnsurePropagatesContextError(t *testing.T) {
	host, _, _ := newHostEnvHarness(t, false)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	handles, err := host.Ensure(ctx, envhost.ScopeRef{Scope: "alpha", Instance: "uuid-1"}, []envhost.Definition{{Name: "noop.alpha"}})
	// The binding's ensure error is the wait error verbatim, and the
	// partial table stays populated so callers see what settled.
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Ensure on canceled context = %v, want context.Canceled verbatim", err)
	}
	if handles == nil {
		t.Error("Ensure on canceled context returned a nil table; want the (empty) partial table")
	}
}

func TestHostEnv_StaleIdentityAndUnknownArmGuards(t *testing.T) {
	host, _, _ := newHostEnvHarness(t, false)

	resp, err := host.Control(context.Background(), &envhost.ControlRequest{
		Scope:   envhost.ScopeRef{Scope: "alpha", Instance: "uuid-1"},
		Adapter: "noop.alpha",
		Kind:    envhost.ControlEnsureFreshHandle,
		Stale:   "not-a-handle",
	})
	if !errors.Is(err, errStaleHandleIdentity) || resp != (envhost.ControlResponse{}) {
		t.Errorf("stale identity guard = %+v, %v; want zero response, errStaleHandleIdentity", resp, err)
	}

	resp, err = host.Control(context.Background(), &envhost.ControlRequest{Kind: envhost.ControlKind(99)})
	if !errors.Is(err, errUnknownControlArm) || resp != (envhost.ControlResponse{}) {
		t.Errorf("unknown arm guard = %+v, %v; want zero response, errUnknownControlArm", resp, err)
	}
}

func TestHostEnv_ReleaseLanes(t *testing.T) {
	host, shim, _ := newHostEnvHarness(t, false)
	scope := envhost.ScopeRef{Scope: "alpha", Instance: "uuid-1"}

	if _, err := host.Control(context.Background(), &envhost.ControlRequest{
		Scope: scope,
		Kind:  envhost.ControlRegisterScope,
		Token: "tok-1",
	}); err != nil {
		t.Fatalf("register arm: %v", err)
	}

	// Detach (and an undeclared fate) keep the engagement alive: the accept
	// token stays and nothing is closed.
	for _, fate := range []envhost.Fate{envhost.FateDetach, envhost.Fate("")} {
		if err := host.Release(context.Background(), scope, envhost.FatePolicy{Fate: fate}); err != nil {
			t.Fatalf("Release(%q): %v", fate, err)
		}
	}
	if got, ok := shim.scopeTokens["alpha/uuid-1"]; !ok || got != "tok-1" {
		t.Errorf("token after detach lanes = %q (present: %v), want tok-1 kept", got, ok)
	}

	// Release withdraws the scope's accept token — the same unregister the
	// run-final teardown performs.
	if err := host.Release(context.Background(), scope, envhost.FatePolicy{Fate: envhost.FateRelease}); err != nil {
		t.Fatalf("Release(release): %v", err)
	}
	if got, ok := shim.scopeTokens["alpha/uuid-1"]; ok {
		t.Errorf("token after release = %q (present: %v), want absent", got, ok)
	}
}

func TestHostEnv_JournalUnavailable(t *testing.T) {
	host, _, _ := newHostEnvHarness(t, false)

	entries, err := host.Journal(envhost.ScopeRef{Scope: "alpha", Instance: "uuid-1"}, "noop.alpha").Entries(context.Background())
	if !errors.Is(err, envhost.ErrJournalUnavailable) {
		t.Errorf("journal entries error = %v, want ErrJournalUnavailable", err)
	}
	if entries != nil {
		t.Errorf("journal entries = %v, want nil", entries)
	}
}
