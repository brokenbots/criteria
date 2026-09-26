package remote

// peer_lifecycle_test.go — T-07 requirement 4 (verify-only): the per-scope
// lifecycle primitives the engine drives (CRI-137 scope reuse, CRI-304
// adoption, rotateFreshRemoteScope's WaitForFreshHandle rotation) keep
// working across peer reconnects. The peer re-handshakes with the SAME
// persisted scope token after its connection dies, and the fresh handle
// that wakes the waiter is bound to the new connection.
//
// lifecycle.go itself is intentionally untouched; this test exercises the
// exact provider surface rotateFreshRemoteScope and the adoption paths call.

import (
	"context"
	"testing"
	"time"
)

// TestPeerScopeTokenRehandshakeAcrossReconnect pins the reconnect half of
// the per-scope lifecycle: the persisted scope token stays valid after a
// peer connection loss, the re-handshake with that token is accepted, and
// WaitForFreshHandle — the primitive rotateFreshRemoteScope (CRI-137 reuse,
// CRI-304 adoption) blocks on — returns a fresh handle for the new
// connection while rejecting the stale one.
func TestPeerScopeTokenRehandshakeAcrossReconnect(t *testing.T) {
	provider, addr := startPeerFixture(t, &Config{ListenAddress: "127.0.0.1:0", PerScopeSessions: true})

	// The scope token the engine persists at provision time (provision_wanted
	// carries digest/listen-address/token; operator launches PEER pods that
	// dial in with it).
	provider.RegisterScope("alpha/s1", "tok-1")

	firstPeer := newFakePeer("alpha/s1")
	firstPeer.token = "tok-1"
	firstPeer.connect(t, addr)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	first, err := provider.WaitForHandle(ctx, "noop", "alpha/s1")
	if err != nil {
		t.Fatalf("WaitForHandle after first handshake: %v", err)
	}

	// Connection loss: the supervision consumer evicts the dead handle, but
	// the scope registration (and its token) persists.
	firstPeer.drop()
	waitFor(t, "registry eviction of stale scoped peer", func() bool {
		_, ok := peerHandleFrom(provider, "alpha/s1")
		return !ok
	})

	// The replacement PEER pod dials in with the same persisted token.
	secondPeer := newFakePeer("alpha/s1")
	secondPeer.token = "tok-1"
	secondPeer.connect(t, addr)
	defer secondPeer.drop()

	fresh, err := provider.WaitForFreshHandle(ctx, "noop", "alpha/s1", first)
	if err != nil {
		t.Fatalf("WaitForFreshHandle after re-handshake: %v", err)
	}
	if fresh == first {
		t.Fatal("WaitForFreshHandle returned the stale handle")
	}
	if _, ok := fresh.(*peerHandle); !ok {
		t.Fatalf("fresh handle type %T, want *peerHandle", fresh)
	}
	info, err := fresh.Info(ctx)
	if err != nil {
		t.Fatalf("fresh handle Info: %v", err)
	}
	if info.Name != "noop" {
		t.Fatalf("fresh handle Info name = %q", info.Name)
	}
}
