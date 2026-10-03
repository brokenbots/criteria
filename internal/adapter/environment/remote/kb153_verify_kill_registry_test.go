// KB-153 regression tests: the remote shim's session registry used to keep a
// torn-down bridge session visible until its asynchronous teardown goroutine
// got around to deleting it. The engine's verify phase kills its throwaway
// handshake handle synchronously, so a bind-phase wait that raced the
// teardown resolved the dead handle and its OpenSession failed with
// "rpc error: code = Canceled desc = grpc: the client connection is closing"
// — a hard step failure instead of the handshake budget's wait for the
// adapter pod's reconnect. These tests pin the post-kill invariant: once Kill
// returns, the registry must never serve the dead session to a fresh wait,
// which must either block for the pod's re-dial or fail at the budget.

package remote

import (
	"context"
	"strings"
	"testing"
	"time"
)

// newKB153TestShim starts a legacy-mode shim with tight wait budgets so the
// tests finish inside their wall-clock deadlines while still exercising the
// budgeted-wait paths.
func newKB153TestShim(t *testing.T, waitBudget time.Duration) (shim *Shim, addr string) {
	t.Helper()
	verifier := &fixedDigestVerifier{allowed: map[string]string{"noop": "sha256:abcd1234"}}
	shim, err := NewShim(&Config{
		ListenAddress:           "127.0.0.1:0",
		AcceptToken:             "legacy-token",
		SessionHandshakeBudget:  waitBudget,
		SessionSchedulingBudget: waitBudget,
	}, verifier)
	if err != nil {
		t.Fatalf("NewShim: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	if err := shim.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = shim.Stop(context.Background()) })
	return shim, shim.listener.Addr().String()
}

func kb153Handshake() *handshakeMessage {
	return &handshakeMessage{Name: "noop", Version: "1.0.0", Digest: "sha256:abcd1234", Token: "legacy-token"}
}

func TestWaitForHandle_AfterVerifyStyleKill_WaitsForRedialNotDeadHandle(t *testing.T) {
	shim, addr := newKB153TestShim(t, 4*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for i := 0; i < 3; i++ {
		// Verify-phase equivalent: wait for the handshake, then kill the
		// resolved handle exactly as verifyAdapterInfo tears its throwaway
		// handshake down before the bind phase waits.
		if err := dialFakeAdapter(addr, kb153Handshake(), nil); err != nil {
			t.Fatalf("iteration %d: dial shim: %v", i, err)
		}
		h1, err := shim.WaitForHandle(ctx, "noop", "")
		if err != nil {
			t.Fatalf("iteration %d: WaitForHandle: %v", i, err)
		}

		h1.Kill()

		// Deterministic invariant: the torn-down session must already be
		// deregistered by the time Kill returns. With the pre-KB-153 shim the
		// entry lingered until the async teardown goroutine ran, and a
		// concurrent fresh wait could resolve the dead bridge.
		key := shim.sessionKey("noop", "")
		shim.mu.Lock()
		_, stillRegistered := shim.sessions[key]
		shim.mu.Unlock()
		if stillRegistered {
			t.Errorf("iteration %d: torn-down verify handle is still registered immediately after Kill", i)
		}

		// Behavioral envelope: a fresh wait must never resolve the dead
		// handle. The redial is deferred so an unfixed shim — whose registry
		// still serves the killed session — hands it back at once, while the
		// fixed shim blocks until the pod re-dials and returns a live handle.
		redialed := make(chan struct{})
		go func() {
			time.Sleep(250 * time.Millisecond)
			_ = dialFakeAdapter(addr, kb153Handshake(), nil)
			close(redialed)
		}()
		h2, err := shim.WaitForHandle(ctx, "noop", "")
		if err != nil {
			t.Fatalf("iteration %d: WaitForHandle after verify-style kill: %v", i, err)
		}
		if h2 == h1 {
			t.Errorf("iteration %d: fresh wait resolved the killed handle (KB-153 race)", i)
		}
		if _, err := h2.Info(ctx); err != nil {
			t.Errorf("iteration %d: bind resolved a dead handle: %v (KB-153 race: torn-down verify handle leaked to a fresh wait)", i, err)
		}
		h2.Kill()
		<-redialed
	}
}

func TestWaitForHandle_DeadAdapterAfterVerifyKill_FailsAtHandshakeBudget(t *testing.T) {
	shim, addr := newKB153TestShim(t, 400*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := dialFakeAdapter(addr, kb153Handshake(), nil); err != nil {
		t.Fatalf("dial shim: %v", err)
	}
	h1, err := shim.WaitForHandle(ctx, "noop", "")
	if err != nil {
		t.Fatalf("WaitForHandle: %v", err)
	}
	h1.Kill()

	// No adapter re-dials: a genuinely dead adapter must fail the fresh wait
	// at the handshake budget with the CRI-137 diagnosis, not resolve the
	// dead session (the pre-KB-153 behavior, which hard-failed the bind with
	// "grpc: the client connection is closing").
	start := time.Now()
	h2, err := shim.WaitForHandle(ctx, "noop", "")
	elapsed := time.Since(start)
	if err == nil {
		h2.Kill()
		t.Fatalf("wait returned a handle although no adapter ever re-dialed: the registry served the torn-down verify handle (KB-153 race)")
	}
	if strings.Contains(err.Error(), "connection is closing") {
		t.Errorf("wait failure must be the budgeted handshake diagnosis, got the torn-down transport error: %v", err)
	}
	if !strings.Contains(err.Error(), "identity handshake") {
		t.Errorf("error %q must name the bounded identity-handshake wait", err.Error())
	}
	if elapsed < 350*time.Millisecond {
		t.Errorf("wait returned after %v; the dead session must not fail a fresh wait instantly", elapsed)
	}
}
