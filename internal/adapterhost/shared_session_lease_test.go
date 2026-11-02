package adapterhost

// kb156_shared_session_lease_test.go — KB-156: lease mechanics for shared
// tool-resource sessions. The engine-level regressions (engine/kb156_...
// tests) pin the end-to-end seam; these unit tests pin the bookkeeping:
// eligibility skips, owner refcounting, the close-refusal guard, release
// idempotency, SessionOpen lease reporting, and delegation routing.

import (
	"context"
	"errors"
	"testing"

	"github.com/brokenbots/criteria/workflow"
)

// kb156LeaseSetup builds an owner manager hosting one tool-resource session
// on the shared stub handle, plus a loader able to resolve the same handle
// so tests can stand up additional children against the same environment.
func kb156LeaseSetup(t *testing.T) (owner, child *SessionManager, stub *kb95WorkflowStub, ctx context.Context) {
	t.Helper()
	ctx = context.Background()
	stub = &kb95WorkflowStub{caps: []string{"execute", concurrentExecuteCapability}}
	loader := NewLoaderWithDiscovery(func(string) (string, error) { return "", nil })
	loader.RegisterBuiltin("mcp", func() Handle { return stub })

	owner = NewSessionManager(loader)
	if err := owner.Open(ctx, "mcp.probe", "mcp", "", nil, nil); err != nil {
		t.Fatalf("owner.Open: %v", err)
	}
	t.Cleanup(func() { _ = owner.Close(context.WithoutCancel(ctx), "mcp.probe") })

	child = NewSessionManager(loader)
	return owner, child, stub, ctx
}

func kb156ExtraChild(t *testing.T) *SessionManager {
	t.Helper()
	return NewSessionManager(nil)
}

func kb156CalleeStep() *workflow.StepNode {
	return &workflow.StepNode{
		Name:       "probe",
		AdapterRef: "mcp.probe",
		Outcomes:   map[string]*workflow.CompiledOutcome{"success": {Name: "success"}},
	}
}

// TestKB156_LeasedNameDelegatesToOwnerSession: a nested execute for a leased
// tool resource never binds on the child — it routes to the owner's shared
// session, which executes and stays the environment's one binding.
func TestKB156_LeasedNameDelegatesToOwnerSession(t *testing.T) {
	owner, child, stub, ctx := kb156LeaseSetup(t)

	if leased := child.LeaseToolResourcesFrom(owner, []string{"mcp.probe"}); len(leased) != 1 || leased[0] != "mcp.probe" {
		t.Fatalf("LeaseToolResourcesFrom = %v; want [mcp.probe]", leased)
	}

	res, err := child.execute(ctx, "mcp.probe", kb156CalleeStep(), nil, toolCallNesting{}, nil)
	if err != nil {
		t.Fatalf("child.execute on leased name: %v", err)
	}
	if res.Outcome != "success" {
		t.Errorf("outcome = %q; want success", res.Outcome)
	}
	if got := stub.executedCount(); got != 1 {
		t.Errorf("stub executes = %d; want 1", got)
	}
	if child.SessionBound("mcp.probe") {
		t.Errorf("child bound its own session for a leased name; want delegation to the owner only")
	}
	if !owner.SessionBound("mcp.probe") {
		t.Errorf("owner's shared session is not bound")
	}
}

// TestKB156_DelegatedExecuteMirrorsTeardownWindow pins the CRI-287
// failure-domain re-derivation for shared sessions: the engine stamps the
// step-timeout teardown window on the manager that ran the canceled step (a
// leasing child), but delegated executes crash-classify on the OWNER's
// window. The delegation must mirror the child's mark onto the owner so a
// teardown cascade observed on the shared session routes as a step timeout,
// not as a crash that would respawn the session out from under the remaining
// callers. Mirroring is monotone: it only opens a closed owner window.
func TestKB156_DelegatedExecuteMirrorsTeardownWindow(t *testing.T) {
	owner, child, _, ctx := kb156LeaseSetup(t)

	if leased := child.LeaseToolResourcesFrom(owner, []string{"mcp.probe"}); len(leased) != 1 {
		t.Fatalf("LeaseToolResourcesFrom = %v; want [mcp.probe]", leased)
	}

	// No mark on the child: the owner's window must stay closed.
	if _, err := child.execute(ctx, "mcp.probe", kb156CalleeStep(), nil, toolCallNesting{}, nil); err != nil {
		t.Fatalf("delegated execute: %v", err)
	}
	if owner.StepTimeoutTeardownWindowOpen() {
		t.Errorf("owner teardown window open with no child mark; want closed")
	}

	// Mark the child (engine canceled a step on the leasing scope) and
	// delegate again: the owner's window must open, mirroring the child's
	// exact mark time so the remaining window is preserved.
	child.MarkEngineStepTimeoutTeardown()
	wantMark := child.engineStepTimeoutTeardownAt.Load()
	if _, err := child.execute(ctx, "mcp.probe", kb156CalleeStep(), nil, toolCallNesting{}, nil); err != nil {
		t.Fatalf("delegated execute after child mark: %v", err)
	}
	if !owner.StepTimeoutTeardownWindowOpen() {
		t.Errorf("owner teardown window closed after mirroring the child's mark; want open")
	}
	if got := owner.engineStepTimeoutTeardownAt.Load(); got != wantMark {
		t.Errorf("owner mark = %d; want the child's %d (remaining window preserved)", got, wantMark)
	}

	// Monotone: with the owner's window already open (its own earlier mark),
	// a delegated execute carrying a fresh child mark must NOT replace it.
	owner.MarkEngineStepTimeoutTeardown()
	ownerMark := owner.engineStepTimeoutTeardownAt.Load()
	child.MarkEngineStepTimeoutTeardown()
	if _, err := child.execute(ctx, "mcp.probe", kb156CalleeStep(), nil, toolCallNesting{}, nil); err != nil {
		t.Fatalf("delegated execute for monotone check: %v", err)
	}
	if got := owner.engineStepTimeoutTeardownAt.Load(); got != ownerMark {
		t.Errorf("owner mark replaced with %d; want the owner's own %d preserved", got, ownerMark)
	}

	// Release so the setup cleanup does not observe a refused close for a
	// session that is still (correctly) lease-anchored.
	child.ReleaseSharedToolResources()
}

// TestKB156_SharedSessionCloseRefusedUntilLastLeaseReleases: one caller's
// close must not tear the shared session out from under other callers. The
// close is refused (typed SessionSharedError) while leases are outstanding,
// becomes a real teardown once the last lease drops, and the session then
// closes exactly once.
func TestKB156_SharedSessionCloseRefusedUntilLastLeaseReleases(t *testing.T) {
	owner, child, _, ctx := kb156LeaseSetup(t)

	if leased := child.LeaseToolResourcesFrom(owner, []string{"mcp.probe"}); len(leased) != 1 {
		t.Fatalf("LeaseToolResourcesFrom = %v; want [mcp.probe]", leased)
	}

	err := owner.Close(ctx, "mcp.probe")
	var sharedErr *SessionSharedError
	if !errors.As(err, &sharedErr) {
		t.Fatalf("owner.Close with an outstanding lease: err = %v; want SessionSharedError", err)
	}
	if got := sharedErr.Leases; got != 1 {
		t.Errorf("SessionSharedError.Leases = %d; want 1", got)
	}
	if !owner.SessionBound("mcp.probe") {
		t.Errorf("shared session was torn down despite the refusal")
	}

	child.ReleaseSharedToolResources()

	if err := owner.Close(ctx, "mcp.probe"); err != nil {
		t.Fatalf("owner.Close after the last lease released: %v", err)
	}
	if owner.SessionBound("mcp.probe") {
		t.Errorf("shared session still bound after the owner-scope close")
	}
}

// TestKB156_LeaseRefcountAcrossTwoLessees: two leasing children each count
// against the owner; the shared session survives one child's release, and
// only the LAST release unlocks the owner-scope teardown.
func TestKB156_LeaseRefcountAcrossTwoLessees(t *testing.T) {
	owner, child1, _, ctx := kb156LeaseSetup(t)
	child2 := kb156ExtraChild(t)

	for _, child := range []*SessionManager{child1, child2} {
		if leased := child.LeaseToolResourcesFrom(owner, []string{"mcp.probe"}); len(leased) != 1 {
			t.Fatalf("LeaseToolResourcesFrom = %v; want [mcp.probe]", leased)
		}
	}

	err := owner.Close(ctx, "mcp.probe")
	var sharedErr *SessionSharedError
	if !errors.As(err, &sharedErr) || sharedErr.Leases != 2 {
		t.Fatalf("owner.Close with two leases: err = %v, Leases; want refused with Leases 2", err)
	}

	child1.ReleaseSharedToolResources()

	err = owner.Close(ctx, "mcp.probe")
	if !errors.As(err, &sharedErr) || sharedErr.Leases != 1 {
		t.Fatalf("owner.Close after one release: err = %v; want still refused with Leases 1", err)
	}

	child2.ReleaseSharedToolResources()

	if err := owner.Close(ctx, "mcp.probe"); err != nil {
		t.Fatalf("owner.Close after both leases released: %v", err)
	}
}

// TestKB156_LeaseSkipsIneligibleNames pins the skip paths: a name the child
// already owns, a name whose declaration resolves to a remote environment,
// and an unknown name. None register a lease, so a no-op lease window never
// blocks the owner-scope teardown.
func TestKB156_LeaseSkipsIneligibleNames(t *testing.T) {
	owner, child, _, ctx := kb156LeaseSetup(t)

	// The child binds its private "mcp.probe" — the child is now the owner of
	// record for that name, so leasing it back from the host-of-record is
	// skipped. Release with no leases is a no-op.
	if err := child.Open(ctx, "mcp.probe", "mcp", "", nil, nil); err != nil {
		t.Fatalf("child.Open: %v", err)
	}
	child.ReleaseSharedToolResources()

	// The remote-declared name is cached on the child like VerifyGraph does;
	// leases must not cross isolation boundaries (ADR-0008/0009).
	remoteGraph := &workflow.FSMGraph{
		WorkflowDir:        "kb156-remote",
		DefaultEnvironment: "remote.edge",
		Environments: map[string]*workflow.EnvironmentNode{
			"remote.edge": {Type: "remote", Name: "edge"},
		},
		Adapters: map[string]*workflow.AdapterNode{
			"mcp.edge": {Type: "mcp", Name: "edge", Environment: "remote.edge"},
		},
		AdapterOrder: []string{"mcp.edge"},
	}
	child.SetGraph(remoteGraph)
	child.cacheGraphAdapterRef(graphAdapterRef{
		instanceID: "mcp.edge",
		node:       remoteGraph.Adapters["mcp.edge"],
		graph:      remoteGraph,
	})

	leased := child.LeaseToolResourcesFrom(owner, []string{"mcp.probe", "mcp.edge", "mcp.ghost"})
	if len(leased) != 0 {
		t.Fatalf("leased = %v; want none (self-owned, remote-declared, and unknown names)", leased)
	}
	if err := owner.Close(ctx, "mcp.probe"); err != nil {
		t.Fatalf("owner.Close after only skipped lease attempts: %v", err)
	}
}

// TestKB156_SessionOpenReportsLeasedNames: CRI-145 re-declaration skip
// parity with the old borrow — while a lease is outstanding the child
// reports the name as open so a re-declaring caller does not bind a second
// session; after the release the child no longer reports it.
func TestKB156_SessionOpenReportsLeasedNames(t *testing.T) {
	owner, child, _, _ := kb156LeaseSetup(t)

	if child.SessionOpen("mcp.probe") {
		t.Fatalf("SessionOpen before the lease = true; want false")
	}
	if leased := child.LeaseToolResourcesFrom(owner, []string{"mcp.probe"}); len(leased) != 1 {
		t.Fatalf("LeaseToolResourcesFrom = %v; want [mcp.probe]", leased)
	}
	if !child.SessionOpen("mcp.probe") {
		t.Errorf("SessionOpen during the lease = false; want true (lease reporting)")
	}
	child.ReleaseSharedToolResources()
	if child.SessionOpen("mcp.probe") {
		t.Errorf("SessionOpen after the release = true; want false")
	}
	if !owner.SessionBound("mcp.probe") {
		t.Errorf("release reporting must not touch the owner's actual binding")
	}
}

// TestKB156_NestedDispatchAfterLeaseReleaseReportsUnknownAdapter: after the
// releasing scope unwinds, the child no longer routes the name — the
// dispatch falls back to local resolution and reports the same typed
// unknown-session failure surface the KB-58 seam produced.
func TestKB156_NestedDispatchAfterLeaseReleaseReportsUnknownAdapter(t *testing.T) {
	owner, child, _, ctx := kb156LeaseSetup(t)

	if leased := child.LeaseToolResourcesFrom(owner, []string{"mcp.probe"}); len(leased) != 1 {
		t.Fatalf("LeaseToolResourcesFrom = %v; want [mcp.probe]", leased)
	}
	if _, err := child.execute(ctx, "mcp.probe", kb156CalleeStep(), nil, toolCallNesting{}, nil); err != nil {
		t.Fatalf("delegated execute: %v", err)
	}
	child.ReleaseSharedToolResources()

	_, err := child.execute(ctx, "mcp.probe", kb156CalleeStep(), nil, toolCallNesting{}, nil)
	if !errors.Is(err, ErrUnknownSession) {
		t.Fatalf("execute for a released name: err = %v; want ErrUnknownSession", err)
	}
}

// TestKB156_LeaseIsIdempotent: a repeated lease attempt for the same name
// must not bump the owner's refcount twice — otherwise a single release
// would underflow and anchor the owner's teardown on a ghost entry.
func TestKB156_LeaseIsIdempotent(t *testing.T) {
	owner, child, _, ctx := kb156LeaseSetup(t)

	if leased := child.LeaseToolResourcesFrom(owner, []string{"mcp.probe"}); len(leased) != 1 {
		t.Fatalf("first lease = %v; want [mcp.probe]", leased)
	}
	if leased := child.LeaseToolResourcesFrom(owner, []string{"mcp.probe"}); len(leased) != 0 {
		t.Errorf("second lease = %v; want none (already leased)", leased)
	}

	err := owner.Close(ctx, "mcp.probe")
	var sharedErr *SessionSharedError
	if !errors.As(err, &sharedErr) || sharedErr.Leases != 1 {
		t.Fatalf("owner.Close after a duplicated lease attempt: err = %v; want refused with Leases 1", err)
	}

	child.ReleaseSharedToolResources()
	if err := owner.Close(ctx, "mcp.probe"); err != nil {
		t.Fatalf("owner.Close after one release: %v", err)
	}
}

// TestKB156_PassThroughLeaseInstallsCalleeInfo: a grandchild leasing through
// an intermediate child of the owner still gets the shared session's adapter
// info surface — the intermediate carries the ultimate owner's info, and the
// skip guard must not filter it out. Without the info the grandchild cannot
// resolve the callee's declaration/capability surface for the delegated
// calls.
func TestKB156_PassThroughLeaseInstallsCalleeInfo(t *testing.T) {
	owner, mid, _, ctx := kb156LeaseSetup(t)
	grandchild := kb156ExtraChild(t)

	// Owner hosts the info surface VerifyGraph would have installed.
	ownerInfo := &workflow.AdapterInfo{
		SupportedFeatures: []string{"probe"},
	}
	owner.adapterInfos = map[string]*workflow.AdapterInfo{"mcp.probe": ownerInfo}

	if leased := mid.LeaseToolResourcesFrom(owner, []string{"mcp.probe"}); len(leased) != 1 {
		t.Fatalf("mid lease = %v; want [mcp.probe]", leased)
	}
	if leased := grandchild.LeaseToolResourcesFrom(mid, []string{"mcp.probe"}); len(leased) != 1 {
		t.Fatalf("grandchild lease = %v; want [mcp.probe]", leased)
	}
	if got := grandchild.adapterInfos["mcp.probe"]; got == nil {
		t.Fatalf("grandchild missing the callee info surface after a pass-through lease")
	} else if got == ownerInfo {
		t.Errorf("grandchild shares the owner's info pointer; want an owned shallow copy")
	}

	// The leased surface still routes through the one shared session.
	res, err := grandchild.execute(ctx, "mcp.probe", kb156CalleeStep(), nil, toolCallNesting{}, nil)
	if err != nil {
		t.Fatalf("grandchild delegated execute: %v", err)
	}
	if res.Outcome != "success" {
		t.Errorf("outcome = %q; want success", res.Outcome)
	}
	grandchild.ReleaseSharedToolResources()
	mid.ReleaseSharedToolResources()
	if err := owner.Close(ctx, "mcp.probe"); err != nil {
		t.Fatalf("owner.Close after both pass-through releases: %v", err)
	}
}
