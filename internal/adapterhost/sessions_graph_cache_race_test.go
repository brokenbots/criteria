package adapterhost

// sessions_graph_cache_race_test.go — CRI-50 re-home: adapter declaration
// resolution is a read-only walk over the compiled graph tree, and the only
// cross-manager state a parallel iteration shares (the parent's bound
// envhost.Host entries via ShareEnvHostsFrom) moves under SessionManager.mu.
// Before the deletion this test pinned an unlocked cache writer
// (graphAdapters/adapterDirs/deferredRemoteAdapters) against concurrent
// resolution readers and BorrowRemoteProvisioningFrom; those caches die with
// the compensation machinery, so the remaining overlap is verification
// (read-only walks) against resolution and Share clones.
//
// This test pins the contract after the deletion: graph verification and
// adapter resolution may overlap freely on one SessionManager while a sharing
// iteration clones the hosts, and the race detector must stay clean; it also
// doubles as the pin that the walk keeps resolving subworkflow declarations
// after concurrent verification.

import (
	"context"
	"sync"
	"testing"
)

func TestSessions_GraphAdapterCache_ConcurrentVerifyAndResolve(t *testing.T) {
	ctx := context.Background()
	g, body := compileCRI269SessionsGraph(t, true)
	const developer = "noop.developer" // subworkflow-declared: resolvable only via the per-instance walk
	const local = "shell.local"

	m := NewSessionManager(&cri269Loader{})
	m.SetGraph(g)
	// Defer the subworkflow adapter's blocking handshake through its host of
	// record's DefersVerify capability so VerifyGraph exercises only the
	// (read-only) walk path: no binary resolution, no process launch. The
	// overlap under test is exactly the shared host clone, nothing else.
	m.SetEnvHostForEnv("remote.prod", &seamSpyHost{defersVerify: true})

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Reader: the adapter-resolution read paths (adapterDeclaration via
	// isRemoteAdapter/isOCIAdapter, and adapterDir) hammering the walk while
	// VerifyGraph rounds run.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			node, graph := m.adapterDeclaration(developer)
			_, _ = node, graph
			_ = m.adapterDir(developer)
			_ = m.isOCIAdapter(developer)
			_ = m.isRemoteAdapter(developer)
			_ = m.adapterDir(local)
			_ = m.isOCIAdapter(local)
		}
	}()

	// Borrower: the production parallel-subworkflow-iteration path
	// (runParallelSubworkflowIteration) that clones the parent's bound hosts
	// under the parent's mu while verification may be in flight.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			child := NewSessionManager(&cri269Loader{})
			child.ShareEnvHostsFrom(m)
		}
	}()

	const rounds = 25
	for i := 0; i < rounds; i++ {
		if err := m.VerifyGraph(ctx, g, nil); err != nil {
			t.Fatalf("VerifyGraph round %d: %v", i, err)
		}
	}
	close(stop)
	wg.Wait()

	// Pin the resolution contract the synchronization protects: after
	// VerifyGraph rounds, the subworkflow declaration must resolve from the
	// compiled tree walk (the root graph never contains it), pointing at the
	// declaring body.
	node, graph := m.adapterDeclaration(developer)
	if node == nil || graph != body {
		t.Fatalf("adapterDeclaration(%q) after VerifyGraph = (%v, %v), want the subworkflow body's declaration", developer, node, graph)
	}
	if node != body.Adapters[developer] {
		t.Fatalf("declared node for %q must be the declaring body's adapter node", developer)
	}
	if dir := m.adapterDir(developer); dir != body.WorkflowDir {
		t.Fatalf("adapterDir(%q) = %q, want the declaring body's directory %q", developer, dir, body.WorkflowDir)
	}
	if dir := m.adapterDir(local); dir != body.WorkflowDir {
		t.Fatalf("adapterDir(%q) = %q, want the declaring body's directory %q", local, dir, body.WorkflowDir)
	}
}
