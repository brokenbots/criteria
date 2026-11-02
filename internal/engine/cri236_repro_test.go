package engine

// Regression tests for CRI-236: the per-scope adapter accept token must ride
// the wire inside the provision/accept handshake (the already-authenticated
// runner↔adapter channel) instead of only the filesystem. token_ref stays as
// a transition-window compatibility surface (removed operator-side in
// CRI-237). Replay-after-restart re-handshakes the same token, eliminating
// the replay-token desync failure class recorded in CRI-253
// (CRI-223/224/252) by construction.

import (
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// requireWireTokenShape asserts the CRI-236 token contract on a provision
// event: a non-empty 32-byte hex token (the same material generateAcceptToken
// mints).
func requireWireTokenShape(t *testing.T, ev *AdapterLifecycleEvent) {
	t.Helper()

	if ev.Token == "" {
		t.Fatalf("provision_wanted for %q/%q carries no wire token", ev.ScopeName, ev.ScopeInstanceID)
	}
	raw, err := hex.DecodeString(ev.Token)
	if err != nil {
		t.Fatalf("wire token %q is not hex: %v", ev.Token, err)
	}
	if len(raw) != 32 {
		t.Fatalf("wire token decodes to %d bytes, want 32", len(raw))
	}
}

// requireShimRegisteredWireToken asserts the token the engine put on the wire
// is the exact token it registered with the shim for the scope, so the wire
// handoff and the shim's accept check can never desync.
func requireShimRegisteredWireToken(t *testing.T, shim *fakeRemoteShim, ev *AdapterLifecycleEvent) {
	t.Helper()

	requireWireTokenShape(t, ev)
	scopeKey := ev.ScopeName + "/" + ev.ScopeInstanceID
	if registered := shim.registeredToken(scopeKey); registered != ev.Token {
		t.Fatalf("shim registered token %q for scope %q, want wire token %q", registered, scopeKey, ev.Token)
	}
}

// requireTokenFileAgreement pins the transition-window invariant (CRI-236):
// while token_ref still exists, the wire token and the token file must carry
// the same material, so file-based operators (CRI-237 removes them later)
// and wire-based operators observe the same secret.
func requireTokenFileAgreement(t *testing.T, ev *AdapterLifecycleEvent) {
	t.Helper()

	fileBytes, err := os.ReadFile(ev.TokenRef)
	if err != nil {
		t.Fatalf("read token file %s: %v", ev.TokenRef, err)
	}
	if string(fileBytes) != ev.Token {
		t.Fatalf("token file %s holds %q, want the wire token %q", ev.TokenRef, string(fileBytes), ev.Token)
	}
}

// TestInitScopeAdapters_PerScope_ProvisionCarriesWireToken pins the core
// CRI-236 contract on a fresh rotation: the provision_wanted event carries
// the raw accept token, and a pod dialing the real shim with ONLY that wire
// token (the token file is never read by the dialer) completes a verified
// handshake through the shim's pre-gRPC accept path.
func TestInitScopeAdapters_PerScope_ProvisionCarriesWireToken(t *testing.T) {
	ctx := context.Background()
	g := perScopeRemoteGraph(t)
	dataDir := t.TempDir()

	sink, shim, rlc, deps := newPerScopeTestHarness(t, g, dataDir)
	if _, err := initScopeAdapters(ctx, g, deps, nil, dataDir, "", nil, rlc); err != nil {
		t.Fatalf("initScopeAdapters: %v", err)
	}

	ev, ok := sink.firstStatus("provision_wanted")
	if !ok {
		t.Fatal("init emitted no provision_wanted event")
	}
	requireShimRegisteredWireToken(t, shim, &ev)
	// Transition-window consistency: the wire token matches the persisted file.
	requireTokenFileAgreement(t, &ev)
}

// TestInitScopeAdapters_PerScope_ReplayAfterRestartRehandshakesWireToken is
// the CRI-236 replay-desync regression: after a runner restart, the
// provision/accept handshake re-delivers the SAME accept token on the wire,
// so a surviving pod holding the pre-restart wire token re-handshakes safely
// (the CRI-253 crash-class failure mode is eliminated by construction).
func TestInitScopeAdapters_PerScope_ReplayAfterRestartRehandshakesWireToken(t *testing.T) {
	ctx := context.Background()
	g := perScopeRemoteGraph(t)
	dataDir := t.TempDir()

	// First engine instance: fresh rotation emits the wire token.
	sink1, shim1, rlc1, deps1 := newPerScopeTestHarness(t, g, dataDir)
	if _, err := initScopeAdapters(ctx, g, deps1, nil, dataDir, "", nil, rlc1); err != nil {
		t.Fatalf("first initScopeAdapters: %v", err)
	}
	first, ok := sink1.firstStatus("provision_wanted")
	if !ok {
		t.Fatal("first init emitted no provision_wanted event")
	}
	requireShimRegisteredWireToken(t, shim1, &first)
	wireToken := first.Token
	if wireToken == "" {
		t.Fatal("pre-restart provision event carries no wire token")
	}

	// Simulated runner restart: fresh in-memory state, same dataDir.
	sink2, shim2, rlc2, deps2 := newPerScopeTestHarness(t, g, dataDir)
	if _, err := initScopeAdapters(ctx, g, deps2, nil, dataDir, "", nil, rlc2); err != nil {
		t.Fatalf("post-restart initScopeAdapters: %v", err)
	}
	second, ok := sink2.firstStatus("provision_wanted")
	if !ok {
		t.Fatal("post-restart init emitted no provision_wanted event")
	}
	if second.Token != wireToken {
		t.Fatalf("post-restart wire token = %q, want the same pre-restart token %q (replay desync)", second.Token, wireToken)
	}
	if second.ScopeInstanceID != first.ScopeInstanceID {
		t.Fatalf("post-restart ScopeInstanceID = %q, want reused %q", second.ScopeInstanceID, first.ScopeInstanceID)
	}
	requireShimRegisteredWireToken(t, shim2, &second)
	requireTokenFileAgreement(t, &second)
}

// TestInitScopeAdapters_PerScope_ScanReuseCarriesWireToken covers the scan
// reuse path (first restart after deploy: no current/ record, only token
// files): the wire token must still match the reused token file and the
// token registered with the shim.
func TestInitScopeAdapters_PerScope_ScanReuseCarriesWireToken(t *testing.T) {
	ctx := context.Background()
	g := perScopeRemoteGraph(t)
	dataDir := t.TempDir()

	sink1, _, rlc1, deps1 := newPerScopeTestHarness(t, g, dataDir)
	if _, err := initScopeAdapters(ctx, g, deps1, nil, dataDir, "", nil, rlc1); err != nil {
		t.Fatalf("first initScopeAdapters: %v", err)
	}
	first, ok := sink1.firstStatus("provision_wanted")
	if !ok {
		t.Fatal("first init emitted no provision_wanted event")
	}
	wireToken := first.Token
	if wireToken == "" {
		t.Fatal("pre-restart provision event carries no wire token")
	}

	// First-restart-after-deploy shape: the current-instance record is gone,
	// only the rotated token file survives.
	if err := os.RemoveAll(filepath.Join(dataDir, "remote-tokens", "current")); err != nil {
		t.Fatalf("remove current record dir: %v", err)
	}

	sink2, shim2, rlc2, deps2 := newPerScopeTestHarness(t, g, dataDir)
	if _, err := initScopeAdapters(ctx, g, deps2, nil, dataDir, "", nil, rlc2); err != nil {
		t.Fatalf("post-restart initScopeAdapters: %v", err)
	}
	second, ok := sink2.firstStatus("provision_wanted")
	if !ok {
		t.Fatal("post-restart init emitted no provision_wanted event")
	}
	if second.ScopeInstanceID != first.ScopeInstanceID {
		t.Fatalf("post-restart ScopeInstanceID = %q, want scan-reused %q", second.ScopeInstanceID, first.ScopeInstanceID)
	}
	if second.Token != wireToken {
		t.Fatalf("scan-reuse wire token = %q, want %q", second.Token, wireToken)
	}
	requireShimRegisteredWireToken(t, shim2, &second)
	requireTokenFileAgreement(t, &second)
}

// TestTearDownScopeAdapters_PerScope_ReleasedEventCarriesNoToken pins the
// event shape: released lifecycle events never carry a token (the CRI-236
// wire token is provision-time only).
func TestTearDownScopeAdapters_PerScope_ReleasedEventCarriesNoToken(t *testing.T) {
	ctx := context.Background()
	g := perScopeRemoteGraph(t)
	dataDir := t.TempDir()

	sink, _, rlc, deps := newPerScopeTestHarness(t, g, dataDir)
	order, err := initScopeAdapters(ctx, g, deps, nil, dataDir, "", nil, rlc)
	if err != nil {
		t.Fatalf("initScopeAdapters: %v", err)
	}
	tearDownScopeAdapters(ctx, order, deps, rlc)

	var released *AdapterLifecycleEvent
	for i := range sink.provisionEvents {
		if sink.provisionEvents[i].Status == "released" {
			released = &sink.provisionEvents[i]
			break
		}
	}
	if released == nil {
		t.Fatal("teardown emitted no released event")
	}
	if released.Token != "" {
		t.Fatalf("released event carries wire token %q, want empty (provision-time only)", released.Token)
	}
	if released.TokenRef == "" {
		t.Fatal("released event must keep its TokenRef (forensic trail / transition window)")
	}
}
