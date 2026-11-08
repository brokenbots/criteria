package engine

// subworkflow_fate_test.go — the engine's session-fate policy at subworkflow
// body exits (ADR-0014 D3): an instance the body graph DECLARES
// continuation = "fresh" is released at the body exit (close + unregister +
// tombstone, for that instance only), so the next re-entry opens a new
// conversation; an instance declared "reuse" — and every undeclared
// instance — keeps the churn-kill detach path byte-for-byte.

import (
	"context"
	"testing"

	"github.com/brokenbots/criteria/workflow"
)

// continuationPerScopeGraph returns the per-scope remote graph with one
// declared continuation fate on its single adapter instance.
func continuationPerScopeGraph(t *testing.T, continuation string) *workflow.FSMGraph {
	t.Helper()
	g := perScopeRemoteGraph(t)
	ad := g.Adapters["noop.default"]
	if ad == nil {
		t.Fatal("perScopeRemoteGraph lost its noop.default adapter")
	}
	ad.Continuation = continuation
	return g
}

// TestSubworkflowBodyExit_DeclaredFreshReleasesInstance pins the fresh fate:
// the declared instance releases at the body exit, the detachment ledger
// never learns about it, and the next body entry rotates a NEW provision (a
// new conversation) instead of re-opening the released one.
func TestSubworkflowBodyExit_DeclaredFreshReleasesInstance(t *testing.T) {
	ctx := context.Background()
	g := continuationPerScopeGraph(t, workflow.ContinuationFresh)
	dataDir := t.TempDir()

	sink, shim, rlc, deps := newPerScopeTestHarness(t, g, dataDir)

	order1, err := initScopeAdapters(ctx, g, deps, nil, dataDir, "entry_one", nil, rlc)
	if err != nil {
		t.Fatalf("entry_one init: %v", err)
	}
	first, ok := sink.firstStatus("provision_wanted")
	if !ok {
		t.Fatal("entry_one emitted no provision_wanted")
	}
	scopeKeyOne := first.ScopeName + "/" + first.ScopeInstanceID

	exitBodyScopeAdapters(ctx, g, "entry_one", order1, deps, rlc)

	if got := len(rlc.detachedRecords()); got != 0 {
		t.Errorf("detachment ledger = %d records after a fresh exit, want 0 (fresh releases, never detaches)", got)
	}
	// The body-exit release must unregister the scope token (teardown-order
	// semantics) so a released pod cannot reconnect with the old token, and
	// close the definition-addressed handle.
	if !containsString(shim.unregistered, scopeKeyOne) {
		t.Errorf("fresh exit did not UnregisterScope %q, got %v", scopeKeyOne, shim.unregistered)
	}
	if wantClose := "noop.default" + ":" + scopeKeyOne; !containsString(shim.closed, wantClose) {
		t.Errorf("fresh exit did not close the definition-addressed handle %q, got %v", wantClose, shim.closed)
	}
	tombstonedRecord := rlc.scopeLifecycle.get("entry_one", "noop.default")
	if tombstonedRecord != nil {
		t.Errorf("entry_one record still registered after the fresh release; the release must tombstone it")
	}
	if deps.Sessions.SessionOpen("noop.default") {
		t.Error("engine session still open after the fresh release; the next entry must open a new conversation")
	}

	// The next entry must NOT hit the already-provisioned skip: it rotates
	// its own provision (new conversation, per the declared fate).
	order2, err := initScopeAdapters(ctx, g, deps, nil, dataDir, "entry_two", nil, rlc)
	if err != nil {
		t.Fatalf("entry_two init: %v", err)
	}
	if len(order2) != 1 {
		t.Fatalf("entry_two provisioned %d adapters, want 1 (fresh fate: the re-entry opens a new conversation)", len(order2))
	}
	second, ok := sink.nthStatus("provision_wanted", 2)
	if !ok {
		t.Fatal("entry_two emitted no second provision_wanted")
	}
	if second.ScopeInstanceID == first.ScopeInstanceID {
		t.Errorf("entry_two reused scope instance %q; the fresh release must tombstone it so the re-entry rotates a new one", first.ScopeInstanceID)
	}

	// Run-final teardown releases the entry-two record (production drives it
	// with the owning scope name and populated order); nothing was ever
	// detached, so the ledger drain is empty.
	tearDownScopeAdapters(ctx, "entry_two", order2, deps, rlc)

	provisions, detaches, releases := 0, 0, 0
	for _, ev := range sink.provisionEventsLog() {
		switch ev.Status {
		case "provision_wanted":
			provisions++
		case "detached":
			detaches++
		case "released":
			releases++
		}
	}
	if provisions != 2 {
		t.Errorf("provision_wanted events = %d, want 2 (one per re-entry under fresh)", provisions)
	}
	if detaches != 0 {
		t.Errorf("detached events = %d, want 0 (fresh never detaches)", detaches)
	}
	if releases != 2 {
		t.Errorf("released events = %d, want 2 (one per body-exit release, one per run-final release)", releases)
	}
}

// TestSubworkflowBodyExit_DeclaredReuseMatchesKB257Detach pins the reuse
// fate as byte-identical to the churn-kill re-entry reuse: the body exit
// detaches (the engagement survives), the re-entry hits the
// already-provisioned skip with no second provision, and only the
// run-final teardown releases.
func TestSubworkflowBodyExit_DeclaredReuseMatchesKB257Detach(t *testing.T) {
	ctx := context.Background()
	g := continuationPerScopeGraph(t, "reuse")
	dataDir := t.TempDir()

	sink, shim, rlc, deps := newPerScopeTestHarness(t, g, dataDir)

	order1, err := initScopeAdapters(ctx, g, deps, nil, dataDir, "entry_one", nil, rlc)
	if err != nil {
		t.Fatalf("entry_one init: %v", err)
	}
	first, ok := sink.firstStatus("provision_wanted")
	if !ok {
		t.Fatal("entry_one emitted no provision_wanted")
	}
	scopeKeyOne := first.ScopeName + "/" + first.ScopeInstanceID

	exitBodyScopeAdapters(ctx, g, "entry_one", order1, deps, rlc)

	if got := len(rlc.detachedRecords()); got != 0 {
		t.Errorf("detachment ledger = %d records after a reuse exit, want 0 (the engine path detaches without ledgering; the ledger variant is the exported DetachBodyScope)", got)
	}
	if tok := shim.registeredToken(scopeKeyOne); tok == "" {
		t.Fatalf("scope %q token unregistered after body exit; the detached session's shim registration must survive", scopeKeyOne)
	}

	// Re-entry: the already-provisioned skip engages — no second provision.
	order2, err := initScopeAdapters(ctx, g, deps, nil, dataDir, "entry_two", nil, rlc)
	if err != nil {
		t.Fatalf("entry_two init: %v", err)
	}
	if len(order2) != 0 {
		t.Fatalf("entry_two provisioned %d adapters, want 0 (reuse re-entry must reuse, not rotate)", len(order2))
	}
	if second, ok := sink.nthStatus("provision_wanted", 2); ok {
		t.Fatalf("entry_two emitted a SECOND provision_wanted (instance %q) — reuse re-entry must reuse", second.ScopeInstanceID)
	}

	// Run-final teardown: the engine path never ledgers (only the exported
	// DetachBodyScope pins that for the ledger variant), so the drain is
	// empty and the detached record gets no second "released" event.
	tearDownScopeAdapters(ctx, "", nil, deps, rlc)

	provisions, detaches, releases := 0, 0, 0
	for _, ev := range sink.provisionEventsLog() {
		switch ev.Status {
		case "provision_wanted":
			provisions++
		case "detached":
			detaches++
		case "released":
			releases++
		}
	}
	if provisions != 1 {
		t.Errorf("provision_wanted events = %d, want 1", provisions)
	}
	if detaches != 1 {
		t.Errorf("detached events = %d, want 1", detaches)
	}
	if releases != 0 {
		t.Errorf("released events = %d, want 0 (the engine-path detach leaves the release to SessionManager.Shutdown)", releases)
	}
}
