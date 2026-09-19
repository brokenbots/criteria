package engine

// cri269_repro_test.go — CRI-269: subworkflow adapters bound to a per-scope
// remote environment were dispatched LOCALLY inside the runner instead of in
// their per-scope remote adapter pods. The deferred-remote walk in initAdapters
// and the isRemoteAdapter lookup only consulted the ROOT graph's adapter map,
// so adapters declared in subworkflow bodies were never marked deferred-remote
// and resolved to local OCI-cache handles, while the provisioning path
// (initScopeAdapters at subworkflow entry) is subworkflow-aware and had already
// emitted provision_wanted for them. The provision path and the bind path
// disagreed about which adapters are remote.
//
// These tests pin the fix: VerifyGraph must not run an eager local Info
// handshake for such adapters, and a bind attempt must dispatch over the shim
// (WaitForHandle) rather than resolving the lockfile-pinned OCI binary
// locally. The loader below fails every local resolution loudly, so any local
// dispatch attempt for the subworkflow adapter is a hard failure.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow"
)

// cri269Loader records and rejects every local adapter resolution. A remote
// per-scope adapter must never reach the loader: dispatch goes over the shim.
type cri269Loader struct {
	mu       sync.Mutex
	resolved []string
}

func (l *cri269Loader) Resolve(_ context.Context, name string) (adapterhost.Handle, error) {
	l.mu.Lock()
	l.resolved = append(l.resolved, name)
	l.mu.Unlock()
	return nil, fmt.Errorf("local adapter dispatch attempted for %q (CRI-269)", name)
}

func (l *cri269Loader) Shutdown(context.Context) error { return nil }

func (l *cri269Loader) resolutionCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.resolved)
}

// noopEventSink is a no-op adapter.EventSink for Execute calls.
type noopEventSink struct{}

func (noopEventSink) Log(string, []byte)  {}
func (noopEventSink) Adapter(string, any) {}

// compileCRI269Graph compiles a parent workflow whose subworkflow body declares
// an adapter bound to a per-scope remote environment — the shape of the
// CRI-261 dev-leg run (pair_programming_loop declaring copilot adapters in a
// per-scope remote env) reduced to a single noop adapter.
func compileCRI269Graph(t *testing.T) *workflow.FSMGraph {
	t.Helper()
	root := t.TempDir()
	pairDir := filepath.Join(root, "pair")
	if err := os.MkdirAll(pairDir, 0o755); err != nil {
		t.Fatalf("mkdir pair dir: %v", err)
	}
	writeFile(t, filepath.Join(root, "parent.hcl"), `
workflow {
  name = "cri269-parent"
  version = "0.1"
  initial_state = "enter"
  target_state  = "done"
}

subworkflow "pair" {
  source = "./pair"
}

step "enter" {
  target = subworkflow.pair
  outcome "success" { next = step.done }
  outcome "failure" { next = state.done }
}

state "done" {
  terminal = true
  success  = true
}`)
	writeFile(t, filepath.Join(pairDir, "linear_wf.hcl"), `
workflow {
  name = "cri269-pair"
  version = "0.1"
  initial_state = "work"
  target_state  = "done"
}

environment "remote" "prod" {
  listen_address     = "127.0.0.1:0"
  per_scope_sessions = true
}

adapter "noop" "developer" {
  environment = remote.prod
}

step "work" {
  target = adapter.noop.developer
  outcome "success" { next = state.done }
  outcome "failure" { next = state.done }
}

state "done" {
  terminal = true
  success  = true
}`)
	return compileWorkflowDir(t, root)
}

// TestEngine_CRI269_SubworkflowPerScopeRemoteAdapterIsDeferredAndRemote walks
// the full divergent path: VerifyGraph must defer the subworkflow adapter, the
// subworkflow scope entry must provision it remotely, and a bind attempt must
// dispatch over the shim rather than resolving the OCI-cache binary locally.
func TestEngine_CRI269_SubworkflowPerScopeRemoteAdapterIsDeferredAndRemote(t *testing.T) {
	ctx := context.Background()
	g := compileCRI269Graph(t)
	body := g.Subworkflows["pair"].Body
	const adapterID = "noop.developer"
	if body == nil || body.Adapters[adapterID] == nil {
		t.Fatalf("fixture must declare %q in the subworkflow body", adapterID)
	}

	sink := &eventTrackingSink{}
	loader := &cri269Loader{}
	eng := New(g, loader, sink, WithDataDir(t.TempDir()), WithRunID("run-269"))

	sessions := adapterhost.NewSessionManager(loader)
	sessions.SetGraph(g)
	shim := newFakeRemoteShim(&fakeRemoteHandle{})
	sessions.SetRemoteShim(shim)
	t.Cleanup(func() { _ = sessions.Shutdown(context.Background()) })

	// Phase 1: VerifyGraph + root-scope provisioning. The subworkflow adapter
	// must be deferred-remote: no eager local Info handshake (which would exec
	// the OCI-cache binary inside the runner) and no shim handshake yet.
	deps, rootOrder, rlc, err := eng.initAdapters(ctx, sessions, sink, nil, "")
	if err != nil {
		t.Fatalf("initAdapters: %v", err)
	}
	defer tearDownScopeAdapters(ctx, rootOrder, deps, rlc)

	if n := loader.resolutionCount(); n != 0 {
		t.Fatalf("VerifyGraph resolved an adapter binary locally %d time(s); the subworkflow per-scope remote adapter must be deferred at VerifyGraph (CRI-269)", n)
	}
	if n := shimWaitForHandleCount(shim); n != 0 {
		t.Fatalf("VerifyGraph ran %d shim handshake(s); a deferred adapter must not handshake before scope entry", n)
	}

	// Phase 2: subworkflow scope entry. The provisioning path already emitted
	// provision_wanted before the fix; with the fix the Verify that follows it
	// must dispatch remotely too, keeping the two paths in agreement.
	scopeName := "pair"
	bodyOrder, err := initScopeAdapters(ctx, body, deps, nil, g.WorkflowDir, scopeName, nil, rlc)
	if err != nil {
		t.Fatalf("initScopeAdapters (subworkflow scope): %v", err)
	}
	defer tearDownScopeAdapters(ctx, bodyOrder, deps, rlc)

	event, ok := sink.firstStatus("provision_wanted")
	if !ok {
		t.Fatal("expected provision_wanted for the subworkflow adapter")
	}
	if event.ScopeName != scopeName {
		t.Errorf("provision_wanted ScopeName = %q, want %q", event.ScopeName, scopeName)
	}
	if event.AdapterName != "developer" {
		t.Errorf("provision_wanted AdapterName = %q, want developer", event.AdapterName)
	}
	scopeKey := event.ScopeName + "/" + event.ScopeInstanceID
	if n := shimWaitForHandleCount(shim, "WaitForHandle:noop:"+scopeKey); n != 1 {
		t.Fatalf("subworkflow Verify must dispatch remotely exactly once (WaitForHandle:noop:%s), got %d call(s)", scopeKey, n)
	}

	// Phase 3: bind attempt. Execute must promote the verified record over the
	// shim handshake — never by resolving the OCI-cache binary locally.
	result, err := sessions.Execute(ctx, adapterID, body.Steps["work"], noopEventSink{})
	if err != nil {
		t.Fatalf("bind/execute %q: %v", adapterID, err)
	}
	if result.Outcome != "success" {
		t.Fatalf("execute outcome = %q, want success", result.Outcome)
	}
	if n := shimWaitForHandleCount(shim, "WaitForHandle:noop:"+scopeKey); n != 2 {
		t.Fatalf("bind must dispatch over the shim (second WaitForHandle:noop:%s), got %d total call(s)", scopeKey, n)
	}

	// Divergence gone: the local OCI-cache dispatch must be unreachable for an
	// adapter whose provision_wanted was emitted.
	if n := loader.resolutionCount(); n != 0 {
		t.Fatalf("local adapter dispatch attempted %d time(s) for an adapter whose provision_wanted was emitted (CRI-269)", n)
	}
}

func shimWaitForHandleCount(shim *fakeRemoteShim, prefixes ...string) int {
	shim.mu.Lock()
	defer shim.mu.Unlock()
	if len(prefixes) == 0 {
		return len(shim.calls)
	}
	n := 0
	for _, call := range shim.calls {
		for _, prefix := range prefixes {
			if strings.HasPrefix(call, prefix) {
				n++
				break
			}
		}
	}
	return n
}
