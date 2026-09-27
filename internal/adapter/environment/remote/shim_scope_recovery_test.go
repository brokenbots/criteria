package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- KB-25: unregistered-scope dial must recover or fail with a typed error ---

// recordingRegistrar is a test ScopeRegistrar: it mirrors the host-side
// registrar contract — consult the persisted token map, re-register only on
// an exact presented-token match, and decline everything else.
type recordingRegistrar struct {
	mu      sync.Mutex
	shim    *Shim
	allowed map[string]string // scope → persisted token
	decline bool
	calls   []string
}

func (r *recordingRegistrar) RegisterScopeOnDial(adapterType, scope, presentedToken string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, adapterType+"\x00"+scope+"\x00"+presentedToken)
	if r.decline {
		return fmt.Errorf("declined: no surviving token for scope %q", scope)
	}
	want, ok := r.allowed[scope]
	if !ok || presentedToken != want {
		return fmt.Errorf("declined: presented token does not match a surviving token for scope %q", scope)
	}
	r.shim.RegisterScope(scope, want)
	return nil
}

func (r *recordingRegistrar) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

// newRecoveryTestShim starts a per-scope shim whose listener is live.
func newRecoveryTestShim(t *testing.T) (*Shim, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	verifier := &fixedDigestVerifier{allowed: map[string]string{"noop": "sha256:abcd1234"}}
	shim, err := NewShim(&Config{ListenAddress: "127.0.0.1:0", AcceptToken: "legacy-token"}, verifier)
	if err != nil {
		t.Fatalf("NewShim: %v", err)
	}
	if err := shim.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = shim.Stop(ctx) })

	shim.SetPerScopeSessions(true)
	return shim, shim.listener.Addr().String()
}

// acceptHandshake dials addr, presents hs, and returns the shim-side accept
// error by serving a minimal adapter on the same connection.
func acceptHandshake(addr string, hs *handshakeMessage) error {
	return dialFakeAdapter(addr, hs, nil)
}

// acceptHandshakeDirect presents hs to shim.Accept over a pipe and returns
// the shim-side accept error deterministically (no listener race, no wait
// budget).
func acceptHandshakeDirect(t *testing.T, shim *Shim, hs *handshakeMessage) error {
	t.Helper()
	client, server := net.Pipe()
	defer client.Close()
	go func() {
		hsBytes, _ := json.Marshal(hs)
		_, _ = client.Write(append(hsBytes, '\n'))
	}()
	acceptErr := make(chan error, 1)
	go func() { acceptErr <- shim.Accept(context.Background(), server) }()
	select {
	case err := <-acceptErr:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("Accept did not return")
		return nil
	}
}

func TestShim_UnregisteredScopeDial_TypedRejectionWithoutRegistrar(t *testing.T) {
	verifier := &fixedDigestVerifier{allowed: map[string]string{"noop": "sha256:abcd1234"}}
	shim, err := NewShim(&Config{ListenAddress: "127.0.0.1:0", AcceptToken: "legacy-token"}, verifier)
	if err != nil {
		t.Fatalf("NewShim: %v", err)
	}
	shim.SetPerScopeSessions(true)
	// No registrar: reject-only behavior must be preserved exactly.

	// Drive Accept directly on a pipe so the typed dial error (not the
	// budgeted-wait terminal error) is observable.
	client, server := net.Pipe()
	defer client.Close()
	go func() {
		hsBytes, _ := json.Marshal(&handshakeMessage{
			Name: "noop", Version: "1.0.0", Digest: "sha256:abcd1234",
			Scope: "root/kb25-unregistered", Token: "survivor-token",
		})
		_, _ = client.Write(append(hsBytes, '\n'))
	}()

	acceptErr := make(chan error, 1)
	go func() { acceptErr <- shim.Accept(context.Background(), server) }()

	select {
	case err := <-acceptErr:
		if err == nil {
			t.Fatal("expected the unregistered-scope dial to be rejected")
		}
		if !errors.Is(err, ErrScopeNotRegistered) {
			t.Errorf("unregistered-scope dial must match the typed sentinel, got: %v", err)
		}
		var typed *ScopeNotRegisteredError
		if !errors.As(err, &typed) {
			t.Fatalf("rejection must be a *ScopeNotRegisteredError, got %T: %v", err, err)
		}
		if typed.Scope != "root/kb25-unregistered" || typed.AdapterType != "noop" {
			t.Errorf("typed rejection must carry the dial identity, got adapter=%q scope=%q", typed.AdapterType, typed.Scope)
		}
		// Pre-KB-25 log signature must stay byte-identical (CRI-137
		// diagnostics and operator dashboards key on it).
		if want := `scope "root/kb25-unregistered" is not registered`; typed.Error() != want {
			t.Errorf("rejection message changed: got %q, want %q", typed.Error(), want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Accept did not return; the unregistered-scope dial is neither rejected nor recovered")
	}
}

func TestShim_StaleTokenDial_DoesNotTriggerRegistrar(t *testing.T) {
	// A registered scope whose presented token no longer matches is a
	// deliberate rotation: the registrar must never be consulted and the
	// rejection must stay a bad-token rejection, not a typed
	// unregistered-scope error.
	shim, _ := newRecoveryTestShim(t)
	shim.RegisterScope("root/kb25-stale/inst-current", "current-token")
	registrar := &recordingRegistrar{shim: shim, allowed: map[string]string{
		"root/kb25-stale/inst-current": "current-token",
	}}
	shim.SetScopeRegistrar(registrar)

	err := acceptHandshakeDirect(t, shim, &handshakeMessage{
		Name: "noop", Version: "1.0.0", Digest: "sha256:abcd1234",
		Scope: "root/kb25-stale/inst-current", Token: "stale-token",
	})
	if err == nil {
		t.Fatal("expected the stale-token dial to be rejected")
	}
	if errors.Is(err, ErrScopeNotRegistered) {
		t.Errorf("stale-token dial must not be classified as an unregistered scope, got: %v", err)
	}
	if !strings.Contains(err.Error(), "accept_token verification failed for scope") {
		t.Errorf("stale-token dial must keep the bad-token diagnosis, got: %v", err)
	}
	if n := registrar.callCount(); n != 0 {
		t.Errorf("registrar must not be consulted for a stale-token dial, got %d calls", n)
	}
}

func TestShim_UnregisteredScopeDial_RecoveredByRegistrar(t *testing.T) {
	// The KB-25 reproduction: a pod re-dials a rotated scope key the shim
	// has no token for. The registrar re-registers the surviving token and
	// the same dial completes its handshake instead of looping on
	// accept-fail rejections.
	shim, addr := newRecoveryTestShim(t)
	scope := "run_pr_reviewer_loop/2f0e9d4c-0000-4000-8000-000000000001"
	registrar := &recordingRegistrar{shim: shim, allowed: map[string]string{scope: "survivor-token"}}
	shim.SetScopeRegistrar(registrar)

	go acceptHandshake(addr, &handshakeMessage{
		Name: "noop", Version: "1.0.0", Digest: "sha256:abcd1234",
		Scope: scope, Token: "survivor-token",
	})

	handle, err := shim.WaitForHandle(context.Background(), "noop", scope)
	if err != nil {
		t.Fatalf("dial must be accepted after registrar recovery, got: %v", err)
	}
	defer handle.Kill()
	if n := registrar.callCount(); n != 1 {
		t.Errorf("expected exactly one registrar consult, got %d", n)
	}
	registrar.mu.Lock()
	call := registrar.calls[0]
	registrar.mu.Unlock()
	if want := "noop\x00" + scope + "\x00survivor-token"; call != want {
		t.Errorf("registrar saw wrong dial identity: got %q, want %q", call, want)
	}
}

func TestShim_UnregisteredScopeDial_WrongTokenIsNotRecovered(t *testing.T) {
	// Security shape: a dial whose token matches no surviving persisted
	// token must not be registered, and the rejection stays typed.
	shim, _ := newRecoveryTestShim(t)
	scope := "root/kb25-guess/inst-1"
	registrar := &recordingRegistrar{shim: shim, allowed: map[string]string{scope: "survivor-token"}}
	shim.SetScopeRegistrar(registrar)

	err := acceptHandshakeDirect(t, shim, &handshakeMessage{
		Name: "noop", Version: "1.0.0", Digest: "sha256:abcd1234",
		Scope: scope, Token: "guessed-token",
	})
	if err == nil {
		t.Fatal("expected the unauthenticated dial to be rejected")
	}
	if !strings.Contains(err.Error(), `scope "root/kb25-guess/inst-1" is not registered`) {
		t.Errorf("declined dial must keep the typed rejection message, got: %v", err)
	}
	if n := registrar.callCount(); n != 1 {
		t.Errorf("expected exactly one registrar consult, got %d", n)
	}
}

func TestShim_UnregisteredScopeDial_RegistrarDecline_KeepsTypedRejection(t *testing.T) {
	shim, _ := newRecoveryTestShim(t)
	scope := "root/kb25-declined/inst-1"
	registrar := &recordingRegistrar{shim: shim, decline: true}
	shim.SetScopeRegistrar(registrar)

	err := acceptHandshakeDirect(t, shim, &handshakeMessage{
		Name: "noop", Version: "1.0.0", Digest: "sha256:abcd1234",
		Scope: scope, Token: "survivor-token",
	})
	if err == nil {
		t.Fatal("expected the declined dial to stay rejected")
	}
	if !strings.Contains(err.Error(), `scope "root/kb25-declined/inst-1" is not registered`) {
		t.Errorf("declined dial must keep the unregistered-scope rejection, got: %v", err)
	}
	if n := registrar.callCount(); n != 1 {
		t.Errorf("expected the registrar to be consulted once per dial, got %d", n)
	}
}
