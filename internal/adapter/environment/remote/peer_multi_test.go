package remote

// peer_multi_test.go — KB-213 multi-adapter peer hosting: ONE phone-home
// connection carrying several hosted children of one environment. Covers
// supervision-event attribution routing, the routed accept wake, route
// header dispatch, fail-closed child-set coverage at accept, the wait-time
// fail-fast for undeclared adapters, and close-scoped teardown that keeps
// the shared conn alive for the other hosted children.

import (
	"context"
	"errors"
	"testing"
	"time"

	criteriav1 "github.com/brokenbots/criteria/sdk/pb/criteria/v1"

	"github.com/brokenbots/criteria/internal/adapterhost"
)

// multiAdapterFake returns a fake peer dialing as `dialName` and advertising
// the two-child hosted set (the KB-213 one-pod shape: dial + copilot).
func multiAdapterFake(dialName string) *fakePeer {
	fp := newFakePeer("")
	fp.adapters = []PeerAdapterIdentity{
		{Name: dialName, Version: "1.0.0", Digest: "sha256:abcd1234"},
		{Name: "copilot", Version: "0.5.7", Digest: "sha256:0000ffff"},
	}
	return fp
}

// startMultiPeerFixture builds a fixture shim whose digest verifier pins
// both hosted children (the KB-213 lockfile shape) and wires the provider
// as the shim's peer acceptor.
func startMultiPeerFixture(t *testing.T) (*peerSessionProvider, string) {
	t.Helper()
	shim, err := NewShim(&Config{ListenAddress: "127.0.0.1:0"}, &fixedDigestVerifier{allowed: multiDigests})
	if err != nil {
		t.Fatalf("NewShim: %v", err)
	}
	shimCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	if err := shim.Start(shimCtx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = shim.Stop(context.Background()) })
	provider := NewPeerSessionProvider(shim, false)
	shim.SetPeerAcceptor(provider)
	return provider, shim.listener.Addr().String()
}

func TestPeerSessionMultiAdapterOneConn(t *testing.T) {
	provider, addr := startMultiPeerFixture(t)
	provider.SetDeclaredAdapters([]string{"noop", "copilot"})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// The copilot waiter registers before the connect so the routed wake
	// (AcceptPeer draining the waits of every hosted non-dial adapter) is
	// the path that satisfies it.
	var copHandle *peerHandle
	copilotReady := make(chan error, 1)
	go func() {
		h, err := provider.WaitForHandle(ctx, "copilot", "")
		if err == nil {
			var ok bool
			copHandle, ok = h.(*peerHandle)
			if !ok {
				err = errors.New("copilot wait returned handle type " + handleTypeName(h))
			}
		}
		copilotReady <- err
	}()

	fp := multiAdapterFake("noop")
	fp.connect(t, addr)
	select {
	case err := <-copilotReady:
		if err != nil {
			t.Fatalf("routed wake for copilot: %v", err)
		}
	case <-ctx.Done():
		t.Fatalf("copilot waiter never woken by the multi-adapter accept")
	}
	noopHandle, err := provider.WaitForHandle(ctx, "noop", "")
	if err != nil {
		t.Fatalf("WaitForHandle(noop): %v", err)
	}
	if _, ok := noopHandle.(*peerHandle); !ok {
		t.Fatalf("noop handle type %T, want *peerHandle", noopHandle)
	}

	// Route dispatch: the copilot handle's calls carry the
	// x-criteria-adapter header; the fake's dispatch simulation echoes the
	// routed adapter, so a name of copilot proves the header round-trips.
	copInfo, err := copHandle.Info(ctx)
	if err != nil {
		t.Fatalf("Info via copilot handle: %v", err)
	}
	if copInfo.Name != "copilot" {
		t.Fatalf("Info via copilot handle = %q, want the routed copilot child", copInfo.Name)
	}
	routes := fp.routeSnapshot()
	if len(routes) == 0 || routes[len(routes)-1] != "copilot" {
		t.Fatalf("Info routes = %v, want the copilot route header observed", routes)
	}
	noopInfo, err := noopHandle.Info(ctx)
	if err != nil || noopInfo.Name != "noop" {
		t.Fatalf("Info via dial handle = (%q, %v), want noop", noopInfo.Name, err)
	}

	// Attribution: a ChildRunStarted arm carrying adapter_type=copilot lands
	// in the copilot journal; an unattributed arm lands on the dial
	// adapter's journal. Both under one conn.
	fp.appendEventFor("copilot", childRunStartedArm("run-b"))
	fp.appendEvent(childRunStartedArm("run-a"))
	waitFor(t, "attributed arm routed to the copilot journal", func() bool {
		ps := mustPeerSession(t, provider)
		ps.mu.Lock()
		defer ps.mu.Unlock()
		return ps.journals.trackFor("copilot").childRuns["run-b"] != nil
	})
	ps := mustPeerSession(t, provider)
	ps.mu.Lock()
	dialBogus := ps.journals.trackFor("noop").childRuns["run-b"]
	dialOwn := ps.journals.trackFor("noop").childRuns["run-a"]
	ps.mu.Unlock()
	if dialBogus != nil {
		t.Fatalf("copilot run leaked onto the noop dial journal")
	}
	if dialOwn == nil {
		t.Fatalf("unattributed run missing from the noop dial journal")
	}

	// A wait for an adapter the environment does not declare can never be
	// satisfied: fail fast with the typed error, no scheduling budget spent.
	start := time.Now()
	if _, err := provider.WaitForHandle(ctx, "mystic", ""); !errors.Is(err, ErrPeerChildSetMissing) {
		t.Fatalf("WaitForHandle(undeclared) = %v, want PeerChildSetMissing", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("WaitForHandle(undeclared) blocked %s, want a fail-fast typed rejection", elapsed)
	}

	// Close-scoped teardown on a hosted non-dial adapter must leave the
	// shared conn alive for the other hosted children.
	if err := provider.CloseHandle(ctx, "copilot", ""); err != nil {
		t.Fatalf("CloseHandle(copilot): %v", err)
	}
	if _, err := noopHandle.Info(context.Background()); err != nil {
		t.Fatalf("dial handle dead after a copilot-only close: %v", err)
	}

	// The dial adapter's own close still releases the transport.
	if err := provider.CloseHandle(ctx, "noop", ""); err != nil {
		t.Fatalf("CloseHandle(noop): %v", err)
	}
	if _, err := noopHandle.Info(context.Background()); err == nil {
		t.Fatalf("noop handle alive after its conn was released")
	}
}

func TestPeerSessionChildSetFailClosedAtAccept(t *testing.T) {
	provider, addr := startMultiPeerFixture(t)
	provider.SetDeclaredAdapters([]string{"noop", "copilot"})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Unit-level: the coverage predicate itself rejects exactly the missing
	// declared adapter and reports the hosted set.
	if missing := provider.missingDeclared([]string{"noop"}); len(missing) != 1 || missing[0] != "copilot" {
		t.Fatalf("missingDeclared([noop]) = %v, want [copilot]", missing)
	}
	if missing := provider.missingDeclared([]string{"noop", "copilot"}); len(missing) != 0 {
		t.Fatalf("missingDeclared(full set) = %v, want none", missing)
	}

	// End-to-end: a pod that self-consistently advertises only noop while
	// the environment declares copilot too cannot host the declared adapter
	// — accepting it would strand copilot sessions on every scheduling
	// budget. The dial is rejected and nothing is registered; repeating
	// mis-declared dials fail the same way (the typed accept-class signal
	// the shim-side classifier recognizes), while a re-created pod with the
	// full declared set is accepted.
	for i := 0; i < 2; i++ {
		bad := multiAdapterFake("noop")
		bad.adapters = bad.adapters[:1]
		bad.connect(t, addr)
	}
	if _, ok := peerHandleFrom(provider, ""); ok {
		t.Fatalf("a rejected dial registered a session")
	}
	good := multiAdapterFake("noop")
	good.connect(t, addr)
	handle, err := provider.WaitForHandle(ctx, "noop", "")
	if err != nil {
		t.Fatalf("WaitForHandle after a fully-covered re-dial: %v", err)
	}
	if _, ok := handle.(*peerHandle); !ok {
		t.Fatalf("recovered dial handle type %T, want *peerHandle", handle)
	}

	// The copilot session on the accepted pod materializes without a new
	// conn: the routed-registry scan finds the conn by its hosted set.
	if _, err := provider.WaitForHandle(ctx, "copilot", ""); err != nil {
		t.Fatalf("WaitForHandle(copilot) over the accepted multi-adapter conn: %v", err)
	}
}

// TestPeerSessionDeclaredAdapterWaitsFailFast pins the wait-time leg of the
// fail-closed contract on its own: without a declared set, an unmatched wait
// burns its scheduling budget like every legacy shim wait; with one
// installed, a wait for an undeclared adapter is rejected immediately so the
// caller surfaces the mis-declared environment instead of a timeout.
func TestPeerSessionDeclaredAdapterWaitsFailFast(t *testing.T) {
	provider, _ := startMultiPeerFixture(t)

	// No declared set: the wait stays open-budget (mixed-fleet escape).
	ctxOpen, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := provider.WaitForHandle(ctxOpen, "mystic", ""); err == nil {
		t.Fatalf("WaitForHandle(mystic) with no declared set returned a handle")
	} else if errors.Is(err, ErrPeerChildSetMissing) {
		t.Fatalf("undeclared wait rejected as child-set without a declared set: %v", err)
	}

	provider.SetDeclaredAdapters([]string{"noop"})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	_, err := provider.WaitForHandle(ctx, "mystic", "")
	if !errors.Is(err, ErrPeerChildSetMissing) {
		t.Fatalf("WaitForHandle(mystic) with declared set = %v, want PeerChildSetMissing", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("fail-fast wait blocked %s", elapsed)
	}
}

// TestPeerSessionMixedFleetLegacyDialUnchanged (mixed fleet): a legacy
// single-adapter peer dial — no child set, unattributed supervision events —
// keeps landing everything on the dial adapter's journal and its session on
// the dial's registry key, exactly as before KB-213.
func TestPeerSessionMixedFleetLegacyDialUnchanged(t *testing.T) {
	provider, addr := startMultiPeerFixture(t)
	provider.SetDeclaredAdapters([]string{"noop"})

	fp := newFakePeer("")
	fp.appendEvent(&criteriav1.SupervisionEvent{
		Kind: &criteriav1.SupervisionEvent_Exited{
			Exited: &criteriav1.ProcessExited{ExitCode: 0, IdleMs: 3},
		},
	})
	fp.connect(t, addr)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	handle, err := provider.WaitForHandle(ctx, "noop", "")
	if err != nil {
		t.Fatalf("WaitForHandle: %v", err)
	}
	reporter := handle.(adapterhost.ProcessExitReporter)
	waitFor(t, "process exit attributed to the dial adapter", reporter.ProcessExited)
	if _, ok := peerHandleFrom(provider, ""); !ok {
		t.Fatalf("legacy dial did not register under its registry key")
	}
	waitFor(t, "dial journal holds the terminal", func() bool {
		ps := mustPeerSession(t, provider)
		ps.mu.Lock()
		defer ps.mu.Unlock()
		return ps.journals.trackFor("noop").exited
	})
}
