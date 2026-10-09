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
	"io"
	"testing"
	"time"

	"github.com/brokenbots/criteria/internal/adapterhost"
)

// kb232DialRefused reports whether the shim refused the dial. A refusal
// closes the connection after the handshake frame; an accepted dial keeps
// the connection open (the bridge is pending the engine's UDS traffic), so
// "not closed within the window" is the acceptance evidence.
func kb232DialRefused(t *testing.T, shim *Shim, hs *handshakeMessage, window time.Duration) bool {
	t.Helper()
	conn := dialRawHandshake(t, shim.listener.Addr().String(), hs)
	defer conn.Close()
	if err := conn.SetReadDeadline(time.Now().Add(window)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	buf := make([]byte, 256)
	if _, readErr := conn.Read(buf); readErr == io.EOF {
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

func TestShim_RefusesRedialOfLiveSessionBridge(t *testing.T) {
	shim, _ := newKB153TestShim(t, 4*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	hs := kb153Handshake()

	if err := dialFakeAdapter(shim.listener.Addr().String(), hs, nil); err != nil {
		t.Fatalf("first dial: %v", err)
	}
	h1, err := shim.WaitForHandle(ctx, "noop", "")
	if err != nil {
		t.Fatalf("WaitForHandle: %v", err)
	}
	if _, err := h1.Info(ctx); err != nil {
		t.Fatalf("first bridge not live after its dial: %v", err)
	}
	key := shim.sessionKey("noop", "")
	if first, ok := kb232SessionEntry(shim, key); !ok || first.handle != h1 {
		t.Fatal("established session missing from the shim registry after the first dial")
	}

	// A re-dial while the bridge is live must be refused: the pod-side
	// runner reconnects with backoff, so a dial storm must not displace the
	// session the engine's verify/bind handshakes and running steps use.
	if !kb232DialRefused(t, shim, hs, 2*time.Second) {
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
	shim, _ := newKB153TestShim(t, 4*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	hs := kb153Handshake()

	if err := dialFakeAdapter(shim.listener.Addr().String(), hs, nil); err != nil {
		t.Fatalf("first dial: %v", err)
	}
	h1, err := shim.WaitForHandle(ctx, "noop", "")
	if err != nil {
		t.Fatalf("WaitForHandle: %v", err)
	}
	key := shim.sessionKey("noop", "")

	// Crash respawn: the engine re-waits with the crashed handle in hand.
	fresh := make(chan adapterhost.Handle, 1)
	errCh := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		h2, err := shim.WaitForFreshHandle(ctx, "noop", "", h1)
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
		if sess, ok := kb232SessionEntry(shim, key); ok && sess.handle == h1 && sess.retired {
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
	shim, _ := newKB153TestShim(t, 4*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	hs := kb153Handshake()

	if err := dialFakeAdapter(shim.listener.Addr().String(), hs, nil); err != nil {
		t.Fatalf("first dial: %v", err)
	}
	h1, err := shim.WaitForHandle(ctx, "noop", "")
	if err != nil {
		t.Fatalf("WaitForHandle: %v", err)
	}
	key := shim.sessionKey("noop", "")

	// A resolve that passes no stale handle (plain verify/bind wait) must
	// receive the current bridge immediately and must NOT arm displacement:
	// afterwards the live session is still defended by the refusal.
	h2, err := shim.WaitForFreshHandle(ctx, "noop", "", nil)
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

	if !kb232DialRefused(t, shim, hs, 2*time.Second) {
		t.Fatal("re-dial was accepted although no respawn re-wait armed displacement")
	}

	// The bind phase still resolves the surviving bridge.
	h3, err := shim.WaitForFreshHandle(ctx, "noop", "", nil)
	if err != nil {
		t.Fatalf("WaitForFreshHandle after refusal: %v", err)
	}
	if h3 != h1 {
		t.Fatal("refusal removed the live session the bind phase must still resolve")
	}
}
