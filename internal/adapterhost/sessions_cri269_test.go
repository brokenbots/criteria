package adapterhost

// sessions_cri269_test.go — CRI-269: subworkflow adapters bound to per-scope
// remote environments were dispatched locally at VerifyGraph and bind time.
// The root graph's adapter map never contains subworkflow declarations, so
// isRemoteAdapter missed them and resolveAdapterHandle fell through to the
// local OCI-cache loader, while the provisioning path (initScopeAdapters) is
// subworkflow-aware and had already emitted provision_wanted for the same
// adapters. These tests pin the sessions-level contract: the per-instance
// cache populated by VerifyGraph carries the DECLARING graph, isRemoteAdapter
// consults it, deferred adapters are not eagerly handshaken, and an adapter
// not bound to a remote env stays local.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/workflow"
)

// cri269Loader records every local resolution and returns a stub handle.
// Assertions count resolutions per adapter name: "noop" is the remote
// subworkflow adapter and must never be resolved locally, while "shell" is a
// genuinely local adapter whose eager handshake is expected to use the loader.
type cri269Loader struct {
	mu       sync.Mutex
	resolved []string
}

func (l *cri269Loader) Resolve(_ context.Context, name string) (Handle, error) {
	l.mu.Lock()
	l.resolved = append(l.resolved, name)
	l.mu.Unlock()
	return cri269Handle{}, nil
}

func (l *cri269Loader) Shutdown(context.Context) error { return nil }

func (l *cri269Loader) resolutionsOf(name string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, resolved := range l.resolved {
		if resolved == name {
			n++
		}
	}
	return n
}

// cri269Handle is a stub Handle returned by the fake shim: enough for the
// Info handshake, panics on any other use so unexpected local paths surface.
type cri269Handle struct{}

func (cri269Handle) Info(context.Context) (Info, error) { return Info{}, nil }
func (cri269Handle) OpenSession(context.Context, string, map[string]string, map[string]string) error {
	return nil
}
func (cri269Handle) Execute(context.Context, string, *workflow.StepNode, adapter.EventSink) (adapter.Result, error) {
	panic("cri269Handle.Execute must not be called in these tests")
}
func (cri269Handle) CloseSession(context.Context, string) error { return nil }
func (cri269Handle) Kill()                                      {}
func (cri269Handle) Pause(context.Context, string) error        { return nil }
func (cri269Handle) Resume(context.Context, string) error       { return nil }
func (cri269Handle) Inspect(context.Context, string) (*v2.InspectResponse, error) {
	panic("cri269Handle.Inspect must not be called in these tests")
}
func (cri269Handle) Snapshot(context.Context, string) (*v2.SnapshotResponse, error) {
	panic("cri269Handle.Snapshot must not be called in these tests")
}
func (cri269Handle) Restore(context.Context, string, []byte, uint32) error {
	panic("cri269Handle.Restore must not be called in these tests")
}

// cri269Shim records WaitForHandle calls and hands back stub handles.
type cri269Shim struct {
	mu    sync.Mutex
	calls []string
}

func (s *cri269Shim) WaitForHandle(_ context.Context, adapterType, scope string) (Handle, error) {
	s.mu.Lock()
	s.calls = append(s.calls, "WaitForHandle:"+adapterType+":"+scope)
	s.mu.Unlock()
	return cri269Handle{}, nil
}

func (s *cri269Shim) WaitForFreshHandle(context.Context, string, string, Handle) (Handle, error) {
	return cri269Handle{}, nil
}

func (s *cri269Shim) RegisterScope(string, string)                      {}
func (s *cri269Shim) UnregisterScope(string)                            {}
func (s *cri269Shim) CloseHandle(context.Context, string, string) error { return nil }
func (s *cri269Shim) ListenAddr() string                                { return "127.0.0.1:1" }
func (s *cri269Shim) Stop(context.Context) error                        { return nil }

func (s *cri269Shim) waitForHandleCalls(prefix string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, call := range s.calls {
		if len(call) >= len(prefix) && call[:len(prefix)] == prefix {
			n++
		}
	}
	return n
}

// compileCRI269SessionsGraph compiles a parent workflow whose subworkflow body
// declares a noop adapter bound to a remote environment (per-scope when
// perScope is set) plus a second adapter with no environment (must stay local).
func compileCRI269SessionsGraph(t *testing.T, perScope bool) (root, body *workflow.FSMGraph) {
	t.Helper()
	dir := t.TempDir()
	pairDir := filepath.Join(dir, "pair")
	if err := os.MkdirAll(pairDir, 0o755); err != nil {
		t.Fatalf("mkdir pair dir: %v", err)
	}
	writeTestFile(t, filepath.Join(dir, "parent.hcl"), `
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
  outcome "failure" { next = step.done }
}

state "done" {
  terminal = true
  success  = true
}`)
	perScopeAttr := ""
	if perScope {
		perScopeAttr = "\n  per_scope_sessions = true"
	}
	writeTestFile(t, filepath.Join(pairDir, "linear_wf.hcl"), fmt.Sprintf(`
workflow {
  name = "cri269-pair"
  version = "0.1"
  initial_state = "work"
  target_state  = "done"
  environment = shell.dev
}

environment "remote" "prod" {
  listen_address = "127.0.0.1:0"%s
}

environment "shell" "dev" {}

adapter "noop" "developer" {
  environment = remote.prod
}

adapter "shell" "local" {}

step "work" {
  target = adapter.noop.developer
  outcome "success" { next = state.done }
  outcome "failure" { next = state.done }
}

state "done" {
  terminal = true
  success  = true
}`, perScopeAttr))

	spec, diags := workflow.ParseDir(dir)
	if diags.HasErrors() {
		t.Fatalf("parse %q: %v", dir, diags)
	}
	root, diags = workflow.CompileWithContext(context.Background(), spec, nil, workflow.CompileOpts{
		WorkflowDir:         dir,
		SubWorkflowResolver: &workflow.LocalSubWorkflowResolver{},
	})
	if diags.HasErrors() {
		t.Fatalf("compile %q: %v", dir, diags)
	}
	return root, root.Subworkflows["pair"].Body
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %q: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %q: %v", path, err)
	}
}

// TestSessions_CRI269_SubworkflowDeferredRemoteAdapter pins the deferred path:
// VerifyGraph must not handshake the subworkflow adapter locally, the cache
// must record the declaring body graph, and isRemoteAdapter must resolve the
// environment against that body graph. The local adapter stays local.
func TestSessions_CRI269_SubworkflowDeferredRemoteAdapter(t *testing.T) {
	ctx := context.Background()
	g, body := compileCRI269SessionsGraph(t, true)
	const developer = "noop.developer"
	const local = "shell.local"

	loader := &cri269Loader{}
	m := NewSessionManager(loader)
	m.SetGraph(g)
	shim := &cri269Shim{}
	m.SetRemoteShim(shim)
	m.SetDeferredRemoteAdapters([]string{developer})

	if err := m.VerifyGraph(ctx, g, nil); err != nil {
		t.Fatalf("VerifyGraph: %v", err)
	}

	if _, ok := m.deferredRemoteAdapters[developer]; !ok {
		t.Fatalf("deferredRemoteAdapters must contain subworkflow instance %q", developer)
	}
	if n := loader.resolutionsOf("noop"); n != 0 {
		t.Fatalf("VerifyGraph resolved the remote subworkflow adapter locally %d time(s); a deferred adapter must not be handshaken at VerifyGraph (CRI-269)", n)
	}
	if n := shim.waitForHandleCalls("WaitForHandle:noop:"); n != 0 {
		t.Fatalf("VerifyGraph ran %d remote handshake(s) for the deferred adapter; the per-scope token is not rotated until scope entry", n)
	}

	ref, ok := m.graphAdapters[developer]
	if !ok {
		t.Fatalf("graph adapter cache must contain %q after VerifyGraph", developer)
	}
	if ref.graph != body {
		t.Fatalf("cached graph for %q must be the subworkflow body, got %v", developer, ref.graph)
	}
	if ref.node != body.Adapters[developer] {
		t.Fatalf("cached node for %q must be the body's adapter node", developer)
	}

	if !m.isRemoteAdapter(developer) {
		t.Fatalf("isRemoteAdapter(%q) = false, want true via the cached subworkflow declaration (CRI-269)", developer)
	}
	if m.isRemoteAdapter(local) {
		t.Fatalf("isRemoteAdapter(%q) = true, want false: an adapter not bound to a remote env stays local", local)
	}
	if m.isRemoteAdapter("noop.never-declared") {
		t.Fatalf("isRemoteAdapter on an unknown instance must fail closed")
	}
	if m.isOCIAdapter(developer) {
		t.Fatalf("isOCIAdapter(%q) = true, want false for a source-less noop adapter", developer)
	}
}

// TestSessions_CRI269_EagerVerifyOfRemoteSubworkflowAdapter pins the eager
// path: a subworkflow adapter bound to a remote env WITHOUT per_scope_sessions
// is verified at VerifyGraph, and that handshake must dispatch over the shim
// rather than resolving the OCI-cache binary locally.
func TestSessions_CRI269_EagerVerifyOfRemoteSubworkflowAdapter(t *testing.T) {
	ctx := context.Background()
	g, _ := compileCRI269SessionsGraph(t, false)
	const developer = "noop.developer"

	loader := &cri269Loader{}
	m := NewSessionManager(loader)
	m.SetGraph(g)
	shim := &cri269Shim{}
	m.SetRemoteShim(shim)

	if err := m.VerifyGraph(ctx, g, nil); err != nil {
		t.Fatalf("VerifyGraph: %v", err)
	}

	if n := shim.waitForHandleCalls("WaitForHandle:noop:"); n != 1 {
		t.Fatalf("eager VerifyGraph handshake must dispatch over the shim exactly once, got %d call(s)", n)
	}
	if n := loader.resolutionsOf("noop"); n != 0 {
		t.Fatalf("eager VerifyGraph handshake resolved the remote subworkflow adapter locally %d time(s) (CRI-269)", n)
	}
	if !m.isRemoteAdapter(developer) {
		t.Fatalf("isRemoteAdapter(%q) = false, want true via the cached subworkflow declaration (CRI-269)", developer)
	}
}
