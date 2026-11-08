package engine

// subworkflow_fate_test.go — the engine's session-fate policy at subworkflow
// body entries and exits (ADR-0014 D3): an instance the body graph DECLARES
// continuation = "fresh" releases the engagement it inherits at its entry
// (provisioning a new conversation) and is released at the body exit (close
// + unregister + tombstone, for that instance only), so every re-entry opens
// a new conversation; an instance declared "reuse" — and every undeclared
// instance — keeps the churn-kill detach path byte-for-byte. The hooks run
// in both hosting modes: the remote per-scope harness below and the
// nil-lifecycle local-mode tests exercise the same declared code path.

import (
	"context"
	"strings"
	"testing"

	"github.com/brokenbots/criteria/internal/adapterhost"
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

// continuationLocalGraph returns a plain local (non-remote) graph with one
// adapter instance carrying the given continuation fate: no environment
// block, so no remote shim and no per-scope records are involved.
func continuationLocalGraph(t *testing.T, continuation string) *workflow.FSMGraph {
	t.Helper()
	g := compile(t, `
workflow {
  name = "continuation-local-test"
  version = "0.1"
  initial_state = "start"
  target_state  = "done"
}

adapter "noop" "default" {
  config {
    model = "local"
  }
}

step "start" {
  target = adapter.noop.default
  outcome "success" { next = step.done }
  outcome "error" { next = step.done }
}

state "done" {
  terminal = true
  success  = true
}`)
	ad := g.Adapters["noop.default"]
	if ad == nil {
		t.Fatal("local graph lost its noop.default adapter")
	}
	ad.Continuation = continuation
	return g
}

// fateTestAdapterName is the adapter instance every fate-test graph
// declares and every lifecycle assertion keys on.
const fateTestAdapterName = "noop.default"

// countStatusSuffixes counts the sink's string-form lifecycle lines ending
// in ":<adapter>:<status>" ("<scope>:<adapter>:<status>" overall).
func countStatusSuffixes(sink *eventTrackingSink, status string) int {
	suffix := ":" + fateTestAdapterName + ":" + status
	n := 0
	for _, line := range sink.lifecycleStatusesLog() {
		if strings.HasSuffix(line, suffix) {
			n++
		}
	}
	return n
}

// countFateEvents totals the structured lifecycle events with the given
// status (provision_wanted / detached / released).
func countFateEvents(sink *eventTrackingSink, status string) int {
	n := 0
	events := sink.provisionEventsLog()
	for i := range events {
		if events[i].Status == status {
			n++
		}
	}
	return n
}

// TestSubworkflowReEntry_DeclaredFreshReplacesInheritedSession is the fate
// policy's acceptance scenario: the PARENT graph provisions instance X (the
// conversation a parent step is using), the subworkflow body re-declares X
// with continuation = "fresh" on BOTH sides, and the body's re-entry must
// NOT inherit the parent's live conversation — the inherited engagement is
// released at the body's ENTRY and the body provisions its own; the body's
// exits release its own records; a second re-entry provisions yet another
// new conversation.
func TestSubworkflowReEntry_DeclaredFreshReplacesInheritedSession(t *testing.T) {
	ctx := context.Background()
	parent := continuationPerScopeGraph(t, workflow.ContinuationFresh)
	body := continuationPerScopeGraph(t, workflow.ContinuationFresh)
	dataDir := t.TempDir()

	sink, shim, rlc, deps := newPerScopeTestHarness(t, parent, dataDir)

	// The parent scope provisions X for its own step usage.
	rootOrder, err := initScopeAdapters(ctx, parent, deps, nil, dataDir, "", nil, rlc)
	if err != nil {
		t.Fatalf("root init: %v", err)
	}
	if len(rootOrder) != 1 {
		t.Fatalf("root provisioned %d adapters, want 1", len(rootOrder))
	}
	first, ok := sink.firstStatus("provision_wanted")
	if !ok {
		t.Fatal("root emitted no provision_wanted")
	}
	rootKey := first.ScopeName + "/" + first.ScopeInstanceID
	if rec := rlc.scopeLifecycle.get("", "noop.default"); rec == nil || rec.scopeInstanceID != first.ScopeInstanceID {
		t.Fatalf("root record missing after parent provisioning")
	}

	// Body entry: the declared-fate hook must release the inherited root
	// engagement and provision the body's own conversation.
	bodyOrder1, err := initScopeAdapters(ctx, body, deps, nil, dataDir, "body_1", nil, rlc)
	if err != nil {
		t.Fatalf("body_1 init (declared fresh re-entry): %v", err)
	}
	if len(bodyOrder1) != 1 {
		t.Fatalf("body_1 provisioned %d adapters, want 1 (fresh re-entry opens a new conversation)", len(bodyOrder1))
	}
	second, ok := sink.nthStatus("provision_wanted", 2)
	if !ok {
		t.Fatal("body_1 emitted no second provision_wanted")
	}
	if second.ScopeName != "body_1" || second.ScopeInstanceID == first.ScopeInstanceID {
		t.Fatalf("body_1 reused the inherited scope instance %q; the fresh re-entry must provision its own", first.ScopeInstanceID)
	}
	bodyScopeKey1 := second.ScopeName + "/" + second.ScopeInstanceID
	if got := countFateEvents(sink, "released"); got != 1 {
		t.Errorf("released events after body entry = %d, want 1 (the inherited root engagement)", got)
	}
	releaseFirst, _ := sink.nthStatus("released", 1)
	if releaseFirst.ScopeName != "" || releaseFirst.ScopeInstanceID != first.ScopeInstanceID {
		t.Errorf("entry release targeted scope %q/%q, want the inherited root instance %q", releaseFirst.ScopeName, releaseFirst.ScopeInstanceID, first.ScopeInstanceID)
	}
	if !containsString(shim.unregistered, rootKey) {
		t.Errorf("entry hook did not UnregisterScope %q, got %v", rootKey, shim.unregistered)
	}
	if wantClose := "noop.default" + ":" + rootKey; !containsString(shim.closed, wantClose) {
		t.Errorf("entry hook did not close the definition-addressed handle %q, got %v", wantClose, shim.closed)
	}
	if rec := rlc.scopeLifecycle.get("", "noop.default"); rec != nil {
		t.Errorf("root record still registered after the entry release; the hook must take it out of the owner scope map")
	}
	if rec := rlc.scopeLifecycle.get("body_1", "noop.default"); rec == nil || rec.scopeInstanceID != second.ScopeInstanceID {
		t.Errorf("body_1 record missing after the fresh re-entry provisioning")
	}
	if got := len(rlc.detachedRecords()); got != 0 {
		t.Errorf("detachment ledger = %d records after the entry hook, want 0 (the hook releases, never detaches)", got)
	}
	if !deps.Sessions.SessionOpen("noop.default") {
		t.Error("no live session after the body's fresh provisioning; the body cannot run its steps")
	}

	// Body exit 1: the body's own record releases (close + unregister +
	// tombstone) for that instance only — the parent's record was already
	// consumed at entry.
	exitBodyScopeAdapters(ctx, body, "body_1", bodyOrder1, deps, rlc)
	if !containsString(shim.unregistered, bodyScopeKey1) {
		t.Errorf("body exit did not UnregisterScope %q, got %v", bodyScopeKey1, shim.unregistered)
	}
	if wantClose := "noop.default" + ":" + bodyScopeKey1; !containsString(shim.closed, wantClose) {
		t.Errorf("body exit did not close the definition-addressed handle %q, got %v", wantClose, shim.closed)
	}
	if rec := rlc.scopeLifecycle.get("body_1", "noop.default"); rec != nil {
		t.Errorf("body_1 record still registered after the fresh body exit; the release must tombstone it")
	}
	if deps.Sessions.SessionOpen("noop.default") {
		t.Error("engine session still open after the body-exit release; the next entry must open a new conversation")
	}

	// Re-entry 2: yet another new conversation.
	bodyOrder2, err := initScopeAdapters(ctx, body, deps, nil, dataDir, "body_1", nil, rlc)
	if err != nil {
		t.Fatalf("body_1 re-entry init: %v", err)
	}
	if len(bodyOrder2) != 1 {
		t.Fatalf("re-entry provisioned %d adapters, want 1 (fresh fate: every re-entry opens a new conversation)", len(bodyOrder2))
	}
	third, ok := sink.nthStatus("provision_wanted", 3)
	if !ok {
		t.Fatal("re-entry emitted no third provision_wanted")
	}
	if third.ScopeName != "body_1" || third.ScopeInstanceID == second.ScopeInstanceID {
		t.Fatalf("re-entry reused scope instance %q; the fresh release must tombstone it so the re-entry rotates a new one", second.ScopeInstanceID)
	}
	exitBodyScopeAdapters(ctx, body, "body_1", bodyOrder2, deps, rlc)

	// The parent's run-final teardown may still carry X in its root order —
	// its per-scope record was consumed at the body entry, so the release
	// degrades to the idempotent Sessions.Close (no resurrect, no panic).
	tearDownScopeAdapters(ctx, "", rootOrder, deps, rlc)
	if got := len(rlc.detachedRecords()); got != 0 {
		t.Errorf("detachment ledger = %d records at teardown, want 0 (fresh never detaches)", got)
	}

	// Scenario totals: three distinct conversations (parent A, body B, body
	// C), every one released at its body boundary, nothing detached.
	if provisions, detaches, releases := countFateEvents(sink, "provision_wanted"), countFateEvents(sink, "detached"), countFateEvents(sink, "released"); provisions != 3 || detaches != 0 || releases != 3 {
		t.Errorf("events = (provision_wanted %d, detached %d, released %d), want (3, 0, 3): three conversations across one parent provisioning and two body re-entries", provisions, detaches, releases)
	}
}

// TestDeclaredFreshReentry_ConcurrentSiblingFailsLoud pins the fail-loud
// branch of the declared-fate entry hook: a fresh re-entry cannot release a
// record another LIVE body scope (concurrent parallel siblings) owns, so the
// body init fails naming that scope instead of racing it.
func TestDeclaredFreshReentry_ConcurrentSiblingFailsLoud(t *testing.T) {
	ctx := context.Background()
	g := continuationPerScopeGraph(t, workflow.ContinuationFresh)
	dataDir := t.TempDir()

	sink, _, rlc, deps := newPerScopeTestHarness(t, g, dataDir)

	// A sibling scope holds the live engagement (as a running parallel
	// iteration would).
	if _, err := initScopeAdapters(ctx, g, deps, nil, dataDir, "body_0", nil, rlc); err != nil {
		t.Fatalf("body_0 init: %v", err)
	}
	if rec := rlc.scopeLifecycle.get("body_0", "noop.default"); rec == nil {
		t.Fatal("body_0 record missing after provisioning")
	}
	if !deps.Sessions.SessionOpen("noop.default") {
		t.Fatal("body_0 session not open; setup broken")
	}

	_, err := initScopeAdapters(ctx, g, deps, nil, dataDir, "body_1", nil, rlc)
	if err == nil {
		t.Fatal("declared-fresh re-entry alongside a live sibling scope succeeded; it must fail loud")
	}
	if !strings.Contains(err.Error(), `body_0`) {
		t.Errorf("error %v does not name the conflicting sibling scope body_0", err)
	}
	if !strings.Contains(err.Error(), "continuation") {
		t.Errorf("error %v does not mention the declared continuation", err)
	}
	// The sibling's engagement must be untouched — the hook never releases
	// a concurrently-owned record.
	if rec := rlc.scopeLifecycle.get("body_0", "noop.default"); rec == nil {
		t.Error("body_0 record released by the conflicting re-entry; a live sibling's engagement must survive the failure")
	}
	if !deps.Sessions.SessionOpen("noop.default") {
		t.Error("sibling session closed by the conflicting re-entry; the running body would lose its session")
	}
	if got := countStatusSuffixes(sink, "init_failed"); got == 0 {
		t.Error("no init_failed event emitted for the unresolvable declared-fate re-entry")
	}
}

// TestSubworkflowFate_LocalMode_ReleasesAndReprovisions exercises the same
// declared code path with a NIL remote lifecycle (plain local adapters, no
// shim, no per-scope records): the entry hook closes the inherited session
// via Sessions.Close only, the body exits release their sessions the same
// way, and every re-entry provisions a new session instead of skipping.
func TestSubworkflowFate_LocalMode_ReleasesAndReprovisions(t *testing.T) {
	ctx := context.Background()
	parent := continuationLocalGraph(t, workflow.ContinuationFresh)
	body := continuationLocalGraph(t, workflow.ContinuationFresh)

	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"noop": &fakeAdapter{name: "noop", outcome: "success"},
	}}
	sessions := adapterhost.NewSessionManager(loader)
	sessions.SetGraph(parent)
	sink := &eventTrackingSink{}
	deps := Deps{Sessions: sessions, Sink: sink}
	dataDir := t.TempDir()

	// Parent provisioning.
	rootOrder, err := initScopeAdapters(ctx, parent, deps, nil, dataDir, "", nil, nil)
	if err != nil {
		t.Fatalf("root init: %v", err)
	}
	if len(rootOrder) != 1 {
		t.Fatalf("root provisioned %d adapters, want 1", len(rootOrder))
	}
	if !deps.Sessions.SessionOpen("noop.default") {
		t.Fatal("root session not open after local provisioning")
	}

	// Body entry: the hook closes the inherited local session (no records
	// exist in this mode) and the fall-through provisions the body's own.
	bodyOrder1, err := initScopeAdapters(ctx, body, deps, nil, dataDir, "body_1", nil, nil)
	if err != nil {
		t.Fatalf("body_1 init (declared fresh re-entry): %v", err)
	}
	if len(bodyOrder1) != 1 {
		t.Fatalf("body_1 provisioned %d adapters, want 1 (fresh re-entry opens a new conversation)", len(bodyOrder1))
	}
	if !deps.Sessions.SessionOpen("noop.default") {
		t.Error("no live session after the body's fresh provisioning; the body cannot run its steps")
	}
	if got := countStatusSuffixes(sink, "verified"); got != 2 {
		t.Errorf("verified sessions after body entry = %d, want 2 (the parent conversation, then the body's own)", got)
	}
	if got := countStatusSuffixes(sink, "closed"); got != 1 {
		t.Errorf("closed events after body entry = %d, want 1 (the inherited session the hook released)", got)
	}

	// Body exit (fresh): the local branch of releaseScopeAdapters —
	// Sessions.Close only, no per-scope record path.
	exitBodyScopeAdapters(ctx, body, "body_1", bodyOrder1, deps, nil)
	if deps.Sessions.SessionOpen("noop.default") {
		t.Error("engine session still open after the fresh body exit; the next entry must open a new conversation")
	}

	// Re-entry: provisions again.
	bodyOrder2, err := initScopeAdapters(ctx, body, deps, nil, dataDir, "body_1", nil, nil)
	if err != nil {
		t.Fatalf("body_1 re-entry init: %v", err)
	}
	if len(bodyOrder2) != 1 {
		t.Fatalf("re-entry provisioned %d adapters, want 1 (fresh fate: every re-entry opens a new conversation)", len(bodyOrder2))
	}
	exitBodyScopeAdapters(ctx, body, "body_1", bodyOrder2, deps, nil)

	if verified, closed := countStatusSuffixes(sink, "verified"), countStatusSuffixes(sink, "closed"); verified != 3 || closed != 3 {
		t.Errorf("local-mode events = (verified %d, closed %d), want (3, 3): three conversations, each released at its boundary", verified, closed)
	}

	// The parent's run-final teardown degrades to the idempotent close —
	// its order still names an instance whose sessions were all released.
	tearDownScopeAdapters(ctx, "", rootOrder, deps, nil)
	if deps.Sessions.SessionOpen("noop.default") {
		t.Error("root session resurrected by the run-final teardown")
	}
}

// TestSubworkflowFate_LocalMode_UndeclaredKeepsDetach pins the non-fresh
// posture in local mode: an undeclared instance keeps today's engine
// behavior — the body reuses the already-open session and the body exit
// does NOT close it (the churn-kill detach path), so no universal release
// can sneak back in.
func TestSubworkflowFate_LocalMode_UndeclaredKeepsDetach(t *testing.T) {
	ctx := context.Background()
	parent := continuationLocalGraph(t, "")
	body := continuationLocalGraph(t, "")

	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"noop": &fakeAdapter{name: "noop", outcome: "success"},
	}}
	sessions := adapterhost.NewSessionManager(loader)
	sessions.SetGraph(parent)
	sink := &eventTrackingSink{}
	deps := Deps{Sessions: sessions, Sink: sink}
	dataDir := t.TempDir()

	rootOrder, err := initScopeAdapters(ctx, parent, deps, nil, dataDir, "", nil, nil)
	if err != nil {
		t.Fatalf("root init: %v", err)
	}
	bodyOrder, err := initScopeAdapters(ctx, body, deps, nil, dataDir, "body_1", nil, nil)
	if err != nil {
		t.Fatalf("body_1 init: %v", err)
	}
	if len(bodyOrder) != 0 {
		t.Fatalf("body_1 provisioned %d adapters, want 0 (undeclared re-entry must reuse the open session)", len(bodyOrder))
	}
	exitBodyScopeAdapters(ctx, body, "body_1", bodyOrder, deps, nil)
	if !deps.Sessions.SessionOpen("noop.default") {
		t.Error("undeclared body exit closed the shared session; the churn-kill detach path must keep it open")
	}
	if got := countStatusSuffixes(sink, "closed"); got != 0 {
		t.Errorf("closed events after an undeclared body exit = %d, want 0", got)
	}
	_ = rootOrder
}
