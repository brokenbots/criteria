// KB-232 regression tests: a re-dial that arrives while a scope's session
// bridge is live must be refused — NOT accepted and used to displace the
// in-use bridge. Dial storms (several per-scope pod dials landing in the
// same tick after a sub-workflow's identities register) used to kill the
// just-established bridge on every new dial, racing the engine's verify and
// bind handshakes into Unavailable/EOF so the step burnt its whole deadline
// with a "not established" session. The pod-side runner retries with
// backoff, so a refusal converges as soon as the dial storms stop (the
// bridge the engine holds stays live). Displacement stays reserved for
// crash respawn, where the engine re-waits with the dead handle (retired
// armed through WaitForFreshHandle).

package remote

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/brokenbots/criteria/internal/adapterhost"
)

// kb232DialRefused reports whether the shim refused the dial. A refusal
// closes the connection after the handshake frame; an accepted dial keeps
// the connection open (the bridge is pending the engine's UDS traffic), so
// "not closed within the window" is the acceptance evidence.
func kb232DialRefused(t *testing.T, shim *Shim, hs *handshakeMessage) bool {
	t.Helper()
	const window = 2 * time.Second
	conn := dialRawHandshake(t, shim.listener.Addr().String(), hs)
	defer conn.Close()
	if err := conn.SetReadDeadline(time.Now().Add(window)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	buf := make([]byte, 256)
	if _, readErr := conn.Read(buf); errors.Is(readErr, io.EOF) {
		return true
	}
	return false
}

// kb232SessionEntry reads the shim's session entry for key under the shim
// lock.
func kb232SessionEntry(shim *Shim, key string) (*session, bool) {
	shim.mu.Lock()
	defer shim.mu.Unlock()
	sess, ok := shim.sessions[key]
	return sess, ok
}

// kb232SessionRetired reports whether the shim's entry for key is the
// stale h1 with retired armed. Reads happen under the shim lock so the
// armed-retired poll cannot race WaitForFreshHandle's retired store.
func kb232SessionRetired(shim *Shim, key string, h1 adapterhost.Handle) bool {
	shim.mu.Lock()
	defer shim.mu.Unlock()
	sess, ok := shim.sessions[key]
	return ok && sess.handle == h1 && sess.retired
}

// kb232Scope is the single scope newKB232TestShim registers.
const kb232Scope = "root/scope-1"

// newKB232TestShim starts a per-scope shim with one registered scope and
// returns the scope's handshake. The refusal/displacement protocol under
// test only applies to per-scope sessions (legacy replace+kill semantics
// stay in place for scope-less dials).
func newKB232TestShim(t *testing.T) (shim *Shim, hs *handshakeMessage, key string) {
	t.Helper()
	verifier := &fixedDigestVerifier{allowed: map[string]string{"noop": "sha256:abcd1234"}}
	shim, err := NewShim(&Config{ListenAddress: "127.0.0.1:0"}, verifier)
	if err != nil {
		t.Fatalf("NewShim: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	if err := shim.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = shim.Stop(context.Background()) })
	shim.SetPerScopeSessions(true)
	const scope = "root/scope-1"
	const token = "scope-1-token"
	shim.RegisterScope(scope, token)
	key = shim.sessionKey("noop", scope)
	hs = &handshakeMessage{Name: "noop", Version: "1.0.0", Digest: "sha256:abcd1234", Scope: scope, Token: token}
	return shim, hs, key
}

func TestShim_RefusesRedialOfLiveSessionBridge(t *testing.T) {
	shim, hs, key := newKB232TestShim(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := dialFakeAdapter(shim.listener.Addr().String(), hs, nil); err != nil {
		t.Fatalf("first dial: %v", err)
	}
	h1, err := shim.WaitForHandle(ctx, "noop", kb232Scope)
	if err != nil {
		t.Fatalf("WaitForHandle: %v", err)
	}
	if _, err := h1.Info(ctx); err != nil {
		t.Fatalf("first bridge not live after its dial: %v", err)
	}
	if first, ok := kb232SessionEntry(shim, key); !ok || first.handle != h1 {
		t.Fatal("established session missing from the shim registry after the first dial")
	}

	// A re-dial while the bridge is live must be refused: the pod-side
	// runner reconnects with backoff, so a dial storm must not displace the
	// session the engine's verify/bind handshakes and running steps use.
	if !kb232DialRefused(t, shim, hs) {
		t.Fatal("re-dial of the live session was accepted; want refusal (KB-232)")
	}

	// The refusal must be non-destructive: the established bridge is still
	// registered, still resolvable for the bind phase, and still serves RPCs.
	if after, ok := kb232SessionEntry(shim, key); !ok || after.handle != h1 {
		t.Fatal("refused re-dial displaced or removed the live session bridge (KB-232)")
	}
	if _, err := h1.Info(ctx); err != nil {
		t.Fatalf("live bridge stopped serving after a refused re-dial: %v", err)
	}
}

func TestShim_WaitForFreshHandleArmsRetiredAndDisplacesOnNextDial(t *testing.T) {
	shim, hs, key := newKB232TestShim(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := dialFakeAdapter(shim.listener.Addr().String(), hs, nil); err != nil {
		t.Fatalf("first dial: %v", err)
	}
	h1, err := shim.WaitForHandle(ctx, "noop", kb232Scope)
	if err != nil {
		t.Fatalf("WaitForHandle: %v", err)
	}

	// Crash respawn: the engine re-waits with the crashed handle in hand.
	fresh := make(chan adapterhost.Handle, 1)
	errCh := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		h2, err := shim.WaitForFreshHandle(ctx, "noop", kb232Scope, h1)
		if err != nil {
			errCh <- err
			return
		}
		fresh <- h2
	}()

	// WaitForFreshHandle arms the retired displacement synchronously while
	// the stale session is still the registry entry; wait briefly for it.
	deadline := time.Now().Add(2 * time.Second)
	armed := false
	for time.Now().Before(deadline) {
		select {
		case <-done:
			t.Fatal("WaitForFreshHandle resolved before a fresh dial arrived")
		default:
		}
		if kb232SessionRetired(shim, key, h1) {
			armed = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !armed {
		t.Fatal("WaitForFreshHandle did not arm the retired displacement after the stale re-wait")
	}

	// The respawned pod's dial must DISPLACE the retired session and resolve
	// the wait with a genuinely new bridge. Use a real fake-adapter dial
	// (serving gRPC on the connection): a refused dial would never resolve
	// the re-wait.
	if err := dialFakeAdapter(shim.listener.Addr().String(), hs, nil); err != nil {
		t.Fatalf("respawn dial: %v", err)
	}
	var h2 adapterhost.Handle
	select {
	case h2 = <-fresh:
	case err := <-errCh:
		t.Fatalf("WaitForFreshHandle after the respawn dial: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("re-dial against a retired (respawned) session did not displace and resolve the wait; want displacement")
	}
	if h2 == h1 {
		t.Fatal("respawn re-wait resolved the crashed handle instead of the displaced bridge")
	}
	if _, err := h2.Info(ctx); err != nil {
		t.Fatalf("displaced bridge not live after respawn dial: %v", err)
	}
	sess, ok := kb232SessionEntry(shim, key)
	if !ok || sess.handle != h2 {
		t.Fatal("shim registry did not hold the displaced bridge after the respawn dial")
	}
	if sess.retired {
		t.Fatal("displaced session inherited the retired flag; the takeover must clear it")
	}
}

func TestShim_FreshHandleWaiterGetsCurrentHandleAndDoesNotArm(t *testing.T) {
	shim, hs, key := newKB232TestShim(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := dialFakeAdapter(shim.listener.Addr().String(), hs, nil); err != nil {
		t.Fatalf("first dial: %v", err)
	}
	h1, err := shim.WaitForHandle(ctx, "noop", kb232Scope)
	if err != nil {
		t.Fatalf("WaitForHandle: %v", err)
	}

	// A resolve that passes no stale handle (plain verify/bind wait) must
	// receive the current bridge immediately and must NOT arm displacement:
	// afterwards the live session is still defended by the refusal.
	h2, err := shim.WaitForFreshHandle(ctx, "noop", kb232Scope, nil)
	if err != nil {
		t.Fatalf("WaitForFreshHandle(nil): %v", err)
	}
	if h2 != h1 {
		t.Fatal("stale-less wait returned a different handle than the live bridge")
	}
	sess, ok := kb232SessionEntry(shim, key)
	if !ok {
		t.Fatal("live session missing after the stale-less wait")
	}
	if sess.retired {
		t.Fatal("stale-less wait armed displaced-on-next-dial; only the crash-respawn re-wait may")
	}

	if !kb232DialRefused(t, shim, hs) {
		t.Fatal("re-dial was accepted although no respawn re-wait armed displacement")
	}

	// The bind phase still resolves the surviving bridge.
	h3, err := shim.WaitForFreshHandle(ctx, "noop", kb232Scope, nil)
	if err != nil {
		t.Fatalf("WaitForFreshHandle after refusal: %v", err)
	}
	if h3 != h1 {
		t.Fatal("refusal removed the live session the bind phase must still resolve")
	}
}

// TestShim_ReserveScopeSessionRefusesSiblingsUntilReleased exercises the
// placeholder reservation protocol the store-storm fix is built on (KB-232):
// a dial flight claims the slot before any bridge resource exists; sibling
// dials are refused with zero teardown while it runs, and a failed flight
// releases the slot so later dials converge instead of wedging the scope.
func TestShim_ReserveScopeSessionRefusesSiblingsUntilReleased(t *testing.T) {
	shim, hs, key := newKB232TestShim(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ph, err := shim.reserveScopeSession("noop", kb232Scope)
	if err != nil {
		t.Fatalf("reserveScopeSession: %v", err)
	}
	if ph == nil {
		t.Fatal("reserveScopeSession returned no placeholder for a per-scope dial")
	}
	if _, ok := kb232SessionEntry(shim, key); !ok {
		t.Fatal("placeholder reservation missing from the session registry")
	}

	// A sibling dial while the flight is establishing must be refused. The
	// refusal is a close-without-teardown: no bridge, no kill, no stall.
	if !kb232DialRefused(t, shim, hs) {
		t.Fatal("sibling dial was accepted while a placeholder reservation held the scope")
	}

	// A concurrent second reserve must not steal the slot.
	if ph2, rerr := shim.reserveScopeSession("noop", kb232Scope); rerr == nil {
		t.Fatal("second reserve claimed the scope while a flight already held it")
	} else if ph2 != nil {
		t.Fatal("second reserve returned a placeholder")
	}

	// Waiters registered while a placeholder holds the slot must fall
	// through to the waiter path (never return a nil placeholder handle or
	// arm the placeholder for displacement) and resolve when the flight
	// stores the real session.
	resolve := make(chan adapterhost.Handle, 1)
	go func() {
		h, werr := shim.WaitForFreshHandle(ctx, "noop", kb232Scope, nil)
		if werr == nil {
			resolve <- h
		}
	}()
	time.Sleep(50 * time.Millisecond)
	select {
	case h := <-resolve:
		if h == nil {
			t.Fatal("wait resolved a nil placeholder handle")
		}
		t.Fatal("WaitForFreshHandle resolved during establishing without a real session")
	default:
	}

	// The flight's bridge setup fails: release must unblock later dials.
	shim.releaseScopeSession("noop", kb232Scope, ph)
	if _, ok := kb232SessionEntry(shim, key); ok {
		t.Fatal("released placeholder still holds the session slot")
	}
	if err := dialFakeAdapter(shim.listener.Addr().String(), hs, nil); err != nil {
		t.Fatalf("post-release dial: %v", err)
	}
	select {
	case h := <-resolve:
		if _, err := h.Info(ctx); err != nil {
			t.Fatalf("post-release bridge not live: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter registered during establishing did not resolve after the session stored")
	}
	sess, ok := kb232SessionEntry(shim, key)
	if !ok || sess.handle == nil || sess.establishing {
		t.Fatal("stored session is not a real established entry after the placeholder release")
	}
}

// TestShim_ConcurrentSameScopeDialsEstablishExactlyOneSession is the storm
// regression: two same-scope dials racing into the shim must converge to
// exactly ONE established session, and the losing dial must be refused
// instead of being accepted and used to kill the winner's bridge (the
// original KB-232 failure mode) or requiring a loser teardown at the store.
func TestShim_ConcurrentSameScopeDialsEstablishExactlyOneSession(t *testing.T) {
	shim, hs, key := newKB232TestShim(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Barrier so both dials land in the same accept window.
	start := make(chan struct{})
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			if err := dialFakeAdapter(shim.listener.Addr().String(), hs, nil); err != nil {
				t.Errorf("storm dial: %v", err)
			}
		}()
	}
	close(start)

	deadline := time.Now().Add(5 * time.Second)
	for {
		sess, ok := kb232SessionEntry(shim, key)
		if ok && sess.handle != nil && !sess.establishing {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no same-scope session established from the concurrent dials")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Exactly one real session must exist and it must serve RPCs.
	sess, ok := kb232SessionEntry(shim, key)
	if !ok || sess.handle == nil || sess.establishing {
		t.Fatal("storm winner is not a real established session")
	}
	if _, err := sess.handle.Info(ctx); err != nil {
		t.Fatalf("winning bridge not live after the storm: %v", err)
	}

	// The losing dial must be refused once the winner established: a
	// follow-up re-dial (the pods' retry-with-backoff shape) must not
	// displace the session that resolved, and the winner must keep serving.
	if !kb232DialRefused(t, shim, hs) {
		t.Fatal("follow-up re-dial after the storm was accepted; want refusal")
	}
	if again, _ := kb232SessionEntry(shim, key); again.handle != sess.handle {
		t.Fatal("follow-up re-dial displaced the winning bridge")
	}
	if _, err := sess.handle.Info(ctx); err != nil {
		t.Fatalf("winning bridge stopped serving after the follow-up refusal: %v", err)
	}
}
