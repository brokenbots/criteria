package engine

import (
	"context"
	"testing"
)

// TestKB257_ReentryReusesDetachedSession pins the churn kill: the SAME
// per-scope remote scope entered twice in ONE run (body exit between the
// entries) reuses the entry-one session on entry two. PINS:
//  1. entry two emits NO second provision_wanted (the CRI-145 skip engages —
//     the detached session stays bound, so SessionOpen is true);
//  2. each body exit emits "detached" — never "released" — so the live pod
//     connection survives the boundary;
//  3. the shim token for entry_one's scope key stays registered across the
//     body boundary (the reuse arm needs it);
//  4. the run-final teardown releases exactly once.
func TestKB257_ReentryReusesDetachedSession(t *testing.T) {
	ctx := context.Background()
	g := perScopeRemoteGraph(t)
	dataDir := t.TempDir()

	sink, shim, rlc, deps := newPerScopeTestHarness(t, g, dataDir)

	// Body one: own record map (the parallel-iteration shape mirrors how
	// runWorkflowBody gives a body its own lifecycle), init provisions.
	order1, err := initScopeAdapters(ctx, g, deps, nil, dataDir, "entry_one", nil, rlc)
	if err != nil {
		t.Fatalf("entry_one init: %v", err)
	}
	first, ok := sink.firstStatus("provision_wanted")
	if !ok {
		t.Fatal("entry_one emitted no provision_wanted")
	}
	scopeOne := first.ScopeInstanceID
	scopeKeyOne := first.ScopeName + "/" + scopeOne

	// Body-one exit: DETACH, not release.
	detached := rlc.DetachBodyScope(order1, deps)
	if len(detached) != 1 {
		t.Fatalf("entry_one detach moved %d records, want 1", len(detached))
	}

	// Body two starts and exits. initScopeAdapters must SKIP the re-declared
	// adapter entirely: the session is still bound (SessionOpen true), so
	// CRI-145 re-opens the SAME engagement with zero rotation. No new
	// provision_wanted may appear.
	order2, err := initScopeAdapters(ctx, g, deps, nil, dataDir, "entry_two", nil, rlc)
	if err != nil {
		t.Fatalf("entry_two init: %v", err)
	}
	if len(order2) != 0 {
		t.Fatalf("entry_two provisioned %d adapters, want 0 (CRI-145 skip must engage on re-entry)", len(order2))
	}
	second, ok := sink.nthStatus("provision_wanted", 2)
	if ok {
		t.Fatalf("entry_two emitted a SECOND provision_wanted (instance %q) — re-entry must reuse, not rotate", second.ScopeInstanceID)
	}

	// The detached token must still be registered on the shim across the
	// boundary (the pod keeps its live phone-home line).
	if tok := shim.registeredToken(scopeKeyOne); tok == "" {
		t.Errorf("scope %q token unregistered after body exit; the detached session's shim registration must survive (the pod keeps dialing with it)", scopeKeyOne)
	}

	// Run-final teardown releases the ledger exactly once. The owned order
	// is empty, so the ledger handback in the real Run defers into this
	// same function on a later call; drive it here explicitly: move the
	// ledger's record into the scope map and tear it down.
	tearDownScopeAdapters(ctx, nil, deps, rlc)
	rlc.detachMu.Lock()
	remaining := rlc.detached
	rlc.detached = nil
	rlc.detachMu.Unlock()
	flushOrder := make([]string, 0, len(remaining))
	for id, rec := range remaining {
		flushOrder = append(flushOrder, id)
		rlc.scopeLifecycle.add(rec)
	}
	if len(flushOrder) > 0 {
		tearDownScopeAdapters(ctx, flushOrder, deps, rlc)
	}

	provisions, detaches, releases := 0, 0, 0
	var seen []string
	for _, ev := range sink.provisionEventsLog() {
		seen = append(seen, ev.AdapterName+":"+ev.Status)
		switch ev.Status {
		case "provision_wanted":
			provisions++
		case "detached":
			detaches++
		case "released":
			releases++
		}
	}
	if releases != 1 {
		t.Fatalf("released events = %d, want 1 (run-final teardown only); full event trail: %v", releases, seen)
	}
	if provisions != 1 {
		t.Errorf("provision_wanted events = %d, want 1 (one provision total; re-entry reuses)", provisions)
	}
	if detaches != 1 {
		t.Errorf("detached events = %d, want 1 (body-one exit; body-two never owned the record)", detaches)
	}
	if releases != 1 {
		t.Errorf("released events = %d, want 1 (run-final teardown only)", releases)
	}
}
