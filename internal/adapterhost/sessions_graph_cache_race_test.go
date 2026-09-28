package adapterhost

// sessions_graph_cache_race_test.go — CRI-50: cacheGraphAdapterRef wrote
// SessionManager.graphAdapters/adapterDirs without holding SessionManager.mu,
// while adapterDeclaration (via remoteEnvForAdapter/isOCIAdapter), adapterDir,
// and BorrowRemoteProvisioningFrom read the same maps. The production call
// graph keeps the two sides serialized today (VerifyGraph runs in initAdapters
// strictly before the run loop; parallel subworkflow iterations get their own
// SessionManager and borrow the caches through mu-protected
// BorrowRemoteProvisioningFrom), but nothing at the API boundary prevented a
// future caller from overlapping them — the maps are plain, unlocked state on
// a shared struct.
//
// This test pins the contract after the fix: graph verification (the cache
// writer) and adapter resolution (the cache readers) may overlap freely on one
// SessionManager, and the race detector must stay clean. Before the fix it
// reports a data race on m.graphAdapters/adapterDirs; it doubles as the
// regression test for the synchronization contract and as the pin that the
// cache keeps resolving subworkflow declarations after concurrent
// verification.

import (
	"context"
	"sync"
	"testing"
)

func TestSessions_GraphAdapterCache_ConcurrentVerifyAndResolve(t *testing.T) {
	ctx := context.Background()
	g, body := compileCRI269SessionsGraph(t, true)
	const developer = "noop.developer" // subworkflow-declared: resolvable only via the per-instance cache
	const local = "shell.local"

	m := NewSessionManager(&cri269Loader{})
	m.SetGraph(g)
	// Defer the subworkflow adapter's handshake so VerifyGraph exercises only
	// the cache write path (no binary resolution, no process launch): the
	// overlap under test is exactly the graph-adapter cache, nothing else.
	m.SetDeferredRemoteAdapters([]string{developer})

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Reader: the adapter-resolution read paths (adapterDeclaration via
	// isRemoteAdapter/isOCIAdapter, and adapterDir) hammering the cache while
	// VerifyGraph populates it.
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
	// (runParallelSubworkflowIteration) that clones the parent's caches under
	// the parent's mu while VerifyGraph-style population may be in flight.
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
			child.BorrowRemoteProvisioningFrom(m)
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

	// Pin the cache contract the synchronization protects: after VerifyGraph,
	// the subworkflow declaration must resolve from the per-instance cache
	// (the root graph never contains it), pointing at the declaring body.
	node, graph := m.adapterDeclaration(developer)
	if node == nil || graph != body {
		t.Fatalf("adapterDeclaration(%q) after VerifyGraph = (%v, %v), want the subworkflow body's declaration", developer, node, graph)
	}
	if node != body.Adapters[developer] {
		t.Fatalf("cached node for %q must be the declaring body's adapter node", developer)
	}
	if dir := m.adapterDir(developer); dir != body.WorkflowDir {
		t.Fatalf("adapterDir(%q) = %q, want the declaring body's directory %q", developer, dir, body.WorkflowDir)
	}
	if dir := m.adapterDir(local); dir != body.WorkflowDir {
		t.Fatalf("adapterDir(%q) = %q, want the declaring body's directory %q", local, dir, body.WorkflowDir)
	}
}