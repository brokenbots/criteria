package engine

// remote_env_dispatch_test.go — regression and dispatch tests for remote environments:
//
// 1. CRI-269: Subworkflow per-scope remote adapter deferred-remote resolution.
//    Adapters declared in subworkflow bodies and bound to per-scope remote environments
//    are recognized as deferred-remote, bypassing local OCI-cache resolution.
//
// 2. CRI-270: Environment working_directory injection.
//    Per-scope remote adapter sessions receive the environment's resolved working_directory
//    via step input key "working_directory" when declared on the adapter's input schema
//    or when using a shell-compatible adapter, without overriding per-step inputs.
//
// 3. CRI-293: Remote shim address collision and local isolation.
//    Local execution with WithLocalShimIsolation assigns auto-chosen loopback ports
//    when multiple environments share a listen_address, preventing EADDRINUSE collisions.
//
// 4. KB-238: Subworkflow-declared remote environment shims and composition.
//    Subworkflow-declared remote environments receive their own distinct shims and
//    addresses, composing cleanly across parent and child scopes.

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclparse"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow"
	"github.com/brokenbots/criteria/workflow/lockfile"
)

// -----------------------------------------------------------------------------
// Shared Scaffolding
// -----------------------------------------------------------------------------

// noopEventSink is a no-op adapter.EventSink for Execute calls.
type noopEventSink struct{}

func (noopEventSink) Log(string, []byte)  {}
func (noopEventSink) Adapter(string, any) {}

// -----------------------------------------------------------------------------
// CRI-269: Subworkflow Deferred-Remote Resolution
// -----------------------------------------------------------------------------

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

	deps, rootOrder, rlc, err := eng.initAdapters(ctx, sessions, sink, nil, "")
	if err != nil {
		t.Fatalf("initAdapters: %v", err)
	}
	defer tearDownScopeAdapters(ctx, "", rootOrder, deps, rlc)

	if n := loader.resolutionCount(); n != 0 {
		t.Fatalf("VerifyGraph resolved an adapter binary locally %d time(s); the subworkflow per-scope remote adapter must be deferred at VerifyGraph (CRI-269)", n)
	}
	if n := shimWaitForHandleCount(shim); n != 0 {
		t.Fatalf("VerifyGraph ran %d shim handshake(s); a deferred adapter must not handshake before scope entry", n)
	}

	scopeName := "pair"
	bodyOrder, err := initScopeAdapters(ctx, body, deps, nil, g.WorkflowDir, scopeName, nil, rlc)
	if err != nil {
		t.Fatalf("initScopeAdapters (subworkflow scope): %v", err)
	}
	defer tearDownScopeAdapters(ctx, scopeName, bodyOrder, deps, rlc)

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
	if n := shimWaitForHandleCount(shim, "WaitForHandle:noop.developer:"+scopeKey); n != 1 {
		t.Fatalf("subworkflow Verify must dispatch remotely exactly once (WaitForHandle:noop.developer:%s), got %d call(s)", scopeKey, n)
	}

	result, err := sessions.Execute(ctx, adapterID, body.Steps["work"], noopEventSink{}, nil)
	if err != nil {
		t.Fatalf("bind/execute %q: %v", adapterID, err)
	}
	if result.Outcome != "success" {
		t.Fatalf("execute outcome = %q, want success", result.Outcome)
	}
	if n := shimWaitForHandleCount(shim, "WaitForHandle:noop.developer:"+scopeKey); n != 2 {
		t.Fatalf("bind must dispatch over the shim (second WaitForHandle:noop.developer:%s), got %d total call(s)", scopeKey, n)
	}

	if n := loader.resolutionCount(); n != 0 {
		t.Fatalf("local adapter dispatch attempted %d time(s) for an adapter whose provision_wanted was emitted (CRI-269)", n)
	}
}

// -----------------------------------------------------------------------------
// CRI-270: Environment Working Directory Injection
// -----------------------------------------------------------------------------

type recordingHandle struct {
	mu     sync.Mutex
	inputs []map[string]string
	schema *workflow.AdapterInfo
}

func (h *recordingHandle) record(step *workflow.StepNode) {
	h.mu.Lock()
	defer h.mu.Unlock()
	cp := make(map[string]string, len(step.Input))
	for k, v := range step.Input {
		cp[k] = v
	}
	h.inputs = append(h.inputs, cp)
}

func (h *recordingHandle) inputCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.inputs)
}

func (h *recordingHandle) inputAt(i int) map[string]string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if i < 0 || i >= len(h.inputs) {
		return nil
	}
	return h.inputs[i]
}

func (h *recordingHandle) Info(context.Context) (adapterhost.Info, error) {
	info := adapterhost.Info{Name: "noop", Version: "test"}
	if h.schema != nil {
		info.AdapterInfo = *h.schema
	}
	return info, nil
}
func (h *recordingHandle) OpenSession(context.Context, string, map[string]string, map[string]string) error {
	return nil
}
func (h *recordingHandle) Execute(_ context.Context, _ string, step *workflow.StepNode, _ adapter.EventSink, rejection *v2.ExecutionRejection) (adapter.Result, error) {
	h.record(step)
	return adapter.Result{Outcome: "success"}, nil
}
func (h *recordingHandle) CloseSession(context.Context, string) error { return nil }
func (h *recordingHandle) Kill()                                      {}
func (h *recordingHandle) Pause(context.Context, string) error        { return nil }
func (h *recordingHandle) Resume(context.Context, string) error       { return nil }
func (h *recordingHandle) Inspect(context.Context, string) (*v2.InspectResponse, error) {
	return nil, nil
}
func (h *recordingHandle) Snapshot(context.Context, string) (*v2.SnapshotResponse, error) {
	return nil, nil
}
func (h *recordingHandle) Restore(context.Context, string, []byte, uint32) error { return nil }

func compileCRI270RemoteGraph(t *testing.T, envWorkingDir string) *workflow.FSMGraph {
	t.Helper()
	return compile(t, fmt.Sprintf(`
workflow {
  name = "cri270-remote"
  version = "0.1"
  initial_state = "work"
  target_state  = "done"
}

environment "remote" "prod" {
  listen_address     = "127.0.0.1:0"
  per_scope_sessions = true
  working_directory  = %q
}

adapter "noop" "default" {
  environment = remote.prod
}

step "work" {
  target = adapter.noop.default
  outcome "success" { next = step.override }
  outcome "error" { next = step.override }
}

step "override" {
  target = adapter.noop.default
  input {
    working_directory = "/cri270/step-own-cwd"
  }
  outcome "success" { next = state.done }
  outcome "error" { next = state.done }
}

state "done" {
  terminal = true
  success  = true
}`, envWorkingDir))
}

func initCRI270Scope(t *testing.T, g *workflow.FSMGraph, sessions *adapterhost.SessionManager, wantInstance string) {
	t.Helper()
	ctx := context.Background()
	dataDir := t.TempDir()
	lifecycle := newScopeLifecycleState(dataDir)
	lifecycle.setRunID("run-270")
	sink := &eventTrackingSink{}
	rlc := &remoteLifecycleContext{scopeLifecycle: lifecycle}
	deps := Deps{Sessions: sessions, Sink: sink}
	order, err := initScopeAdapters(ctx, g, deps, nil, dataDir, "", nil, rlc)
	if err != nil {
		t.Fatalf("initScopeAdapters: %v", err)
	}
	if len(order) != 1 || order[0] != wantInstance {
		t.Fatalf("expected order [%s], got %v", wantInstance, order)
	}
	t.Cleanup(func() { _ = sessions.Shutdown(context.Background()) })
}

func TestEngine_CRI270_RemoteAdapterReceivesEnvironmentWorkingDir(t *testing.T) {
	ctx := context.Background()
	envWorkingDir := t.TempDir()
	g := compileCRI270RemoteGraph(t, envWorkingDir)

	handle := &recordingHandle{schema: &workflow.AdapterInfo{
		InputSchema: map[string]workflow.ConfigField{
			"working_directory": {Type: workflow.ConfigFieldString},
		},
	}}
	sessions := adapterhost.NewSessionManager(&fakeLoader{})
	sessions.SetGraph(g)
	sessions.SetRemoteShim(newFakeRemoteShim(handle))
	initCRI270Scope(t, g, sessions, "noop.default")

	work := g.Steps["work"]
	if work == nil {
		t.Fatal("fixture must declare step work")
	}
	if got, ok := work.Input["working_directory"]; ok {
		t.Fatalf("compiled step must not pre-carry working_directory, got %q", got)
	}
	if _, err := sessions.Execute(ctx, "noop.default", work, noopEventSink{}, nil); err != nil {
		t.Fatalf("execute work: %v", err)
	}
	if n := handle.inputCount(); n != 1 {
		t.Fatalf("adapter executed %d time(s), want 1", n)
	}
	if got := handle.inputAt(0)["working_directory"]; got != envWorkingDir {
		t.Fatalf("adapter input working_directory = %q, want the resolved environment working_directory %q (CRI-270)", got, envWorkingDir)
	}
	if _, ok := work.Input["working_directory"]; ok {
		t.Fatal("compiled step Input was mutated by the injection")
	}

	override := g.Steps["override"]
	if override == nil {
		t.Fatal("fixture must declare step override")
	}
	if _, err := sessions.Execute(ctx, "noop.default", override, noopEventSink{}, nil); err != nil {
		t.Fatalf("execute override: %v", err)
	}
	if n := handle.inputCount(); n != 2 {
		t.Fatalf("adapter executed %d time(s), want 2", n)
	}
	if got := handle.inputAt(1)["working_directory"]; got != "/cri270/step-own-cwd" {
		t.Fatalf("per-step working_directory was overridden: got %q, want /cri270/step-own-cwd", got)
	}
}

func TestEngine_CRI270_RemoteWithoutWorkingDirInjectsNothing(t *testing.T) {
	ctx := context.Background()
	g := perScopeRemoteGraph(t)

	handle := &recordingHandle{}
	sessions := adapterhost.NewSessionManager(&fakeLoader{})
	sessions.SetGraph(g)
	sessions.SetRemoteShim(newFakeRemoteShim(handle))
	initCRI270Scope(t, g, sessions, "noop.default")

	work := g.Steps["start"]
	if work == nil {
		t.Fatal("fixture must declare step start")
	}
	if _, err := sessions.Execute(ctx, "noop.default", work, noopEventSink{}, nil); err != nil {
		t.Fatalf("execute start: %v", err)
	}
	if n := handle.inputCount(); n != 1 {
		t.Fatalf("adapter executed %d time(s), want 1", n)
	}
	if got, ok := handle.inputAt(0)["working_directory"]; ok {
		t.Fatalf("remote session without a working_directory must not inject one, got %q", got)
	}
}

func TestEngine_CRI270_LocalAdapterGetsNoInjection(t *testing.T) {
	ctx := context.Background()
	localWorkingDir := t.TempDir()
	g := compile(t, fmt.Sprintf(`
workflow {
  name = "cri270-local"
  version = "0.1"
  initial_state = "work"
  target_state  = "done"
}

environment "shell" "dev" {
  working_directory = %q
}

adapter "noop" "default" {
  environment = shell.dev
}

step "work" {
  target = adapter.noop.default
  outcome "success" { next = state.done }
  outcome "error" { next = state.done }
}

state "done" {
  terminal = true
  success  = true
}`, localWorkingDir))

	handle := &recordingHandle{}
	sessions := adapterhost.NewSessionManager(&fakeLoader{adapters: map[string]adapterhost.Handle{"noop": handle}})
	sessions.SetGraph(g)
	t.Cleanup(func() { _ = sessions.Shutdown(context.Background()) })

	if err := sessions.OpenWithOriginRefs(ctx, "noop.default", "noop", "", nil, nil, nil, localWorkingDir); err != nil {
		t.Fatalf("open local session: %v", err)
	}

	work := g.Steps["work"]
	if work == nil {
		t.Fatal("fixture must declare step work")
	}
	if _, err := sessions.Execute(ctx, "noop.default", work, noopEventSink{}, nil); err != nil {
		t.Fatalf("execute work: %v", err)
	}
	if n := handle.inputCount(); n != 1 {
		t.Fatalf("adapter executed %d time(s), want 1", n)
	}
	if got, ok := handle.inputAt(0)["working_directory"]; ok {
		t.Fatalf("local session must receive no injected working_directory, got %q", got)
	}
}

func compileCRI270RemoteSchemaGraph(t *testing.T, envWorkingDir string) *workflow.FSMGraph {
	t.Helper()
	return compile(t, fmt.Sprintf(`
workflow {
  name = "cri270-remote-schema"
  version = "0.1"
  initial_state = "work"
  target_state  = "done"
}

environment "remote" "prod" {
  listen_address     = "127.0.0.1:0"
  per_scope_sessions = true
  working_directory  = %q
}

adapter "noop" "default" {
  environment = remote.prod
}

step "work" {
  target = adapter.noop.default
  input {
    task = "run-intake"
  }
  outcome "success" { next = state.done }
  outcome "error" { next = state.done }
}

state "done" {
  terminal = true
  success  = true
}`, envWorkingDir))
}

func TestEngine_CRI270_InjectionGatedOnDeclaredInputSchema(t *testing.T) {
	ctx := context.Background()
	envWorkingDir := t.TempDir()
	g := compileCRI270RemoteSchemaGraph(t, envWorkingDir)

	handle := &recordingHandle{schema: &workflow.AdapterInfo{
		InputSchema: map[string]workflow.ConfigField{
			"task": {Type: workflow.ConfigFieldString},
		},
	}}
	sessions := adapterhost.NewSessionManager(&fakeLoader{})
	sessions.SetGraph(g)
	sessions.SetRemoteShim(newFakeRemoteShim(handle))
	initCRI270Scope(t, g, sessions, "noop.default")

	work := g.Steps["work"]
	if work == nil {
		t.Fatal("fixture must declare step work")
	}
	if _, err := sessions.Execute(ctx, "noop.default", work, noopEventSink{}, nil); err != nil {
		t.Fatalf("execute work: %v", err)
	}
	if n := handle.inputCount(); n != 1 {
		t.Fatalf("adapter executed %d time(s), want 1", n)
	}
	got := handle.inputAt(0)
	if _, ok := got["working_directory"]; ok {
		t.Fatal("adapter with a declared input surface lacking working_directory must not receive the engine-injected key (CRI-270 review gate)")
	}
	if got["task"] != "run-intake" {
		t.Fatalf("adapter input task = %q, want run-intake (the step input must reach the adapter verbatim)", got["task"])
	}
	if _, ok := work.Input["working_directory"]; ok {
		t.Fatal("compiled step Input was mutated by the gated injection")
	}
}

func TestEngine_CRI270_InjectionOnDeclaredWorkingDirKey(t *testing.T) {
	ctx := context.Background()
	envWorkingDir := t.TempDir()
	g := compileCRI270RemoteSchemaGraph(t, envWorkingDir)

	handle := &recordingHandle{schema: &workflow.AdapterInfo{
		InputSchema: map[string]workflow.ConfigField{
			"task":              {Type: workflow.ConfigFieldString},
			"working_directory": {Type: workflow.ConfigFieldString},
		},
	}}
	sessions := adapterhost.NewSessionManager(&fakeLoader{})
	sessions.SetGraph(g)
	sessions.SetRemoteShim(newFakeRemoteShim(handle))
	initCRI270Scope(t, g, sessions, "noop.default")

	work := g.Steps["work"]
	if work == nil {
		t.Fatal("fixture must declare step work")
	}
	if _, err := sessions.Execute(ctx, "noop.default", work, noopEventSink{}, nil); err != nil {
		t.Fatalf("execute work: %v", err)
	}
	if n := handle.inputCount(); n != 1 {
		t.Fatalf("adapter executed %d time(s), want 1", n)
	}
	got := handle.inputAt(0)
	if got["working_directory"] != envWorkingDir {
		t.Fatalf("adapter input working_directory = %q, want the injected environment working_directory %q", got["working_directory"], envWorkingDir)
	}
	if got["task"] != "run-intake" {
		t.Fatalf("adapter input task = %q, want run-intake (the injection must not displace step input)", got["task"])
	}
	if _, ok := work.Input["working_directory"]; ok {
		t.Fatal("compiled step Input was mutated by the injection")
	}
}

func compileCRI270RemoteAdapterTypeGraph(t *testing.T, adapterType, envWorkingDir string) *workflow.FSMGraph {
	t.Helper()
	return compile(t, fmt.Sprintf(`
workflow {
  name = "cri270-remote-%s"
  version = "0.1"
  initial_state = "work"
  target_state  = "done"
}

environment "remote" "prod" {
  listen_address     = "127.0.0.1:0"
  per_scope_sessions = true
  working_directory  = %q
}

adapter "%s" "default" {
  environment = remote.prod
}

step "work" {
  target = adapter.%s.default
  input {
    task = "run-intake"
  }
  outcome "success" { next = state.done }
  outcome "error" { next = state.done }
}

state "done" {
  terminal = true
  success  = true
}`, adapterType, envWorkingDir, adapterType, adapterType))
}

func TestEngine_CRI270_UndeclaredSurfaceGetsNoInjection(t *testing.T) {
	ctx := context.Background()
	envWorkingDir := t.TempDir()
	g := compileCRI270RemoteAdapterTypeGraph(t, "mcp", envWorkingDir)

	handle := &recordingHandle{}
	sessions := adapterhost.NewSessionManager(&fakeLoader{})
	sessions.SetGraph(g)
	sessions.SetRemoteShim(newFakeRemoteShim(handle))
	initCRI270Scope(t, g, sessions, "mcp.default")

	work := g.Steps["work"]
	if work == nil {
		t.Fatal("fixture must declare step work")
	}
	if _, err := sessions.Execute(ctx, "mcp.default", work, noopEventSink{}, nil); err != nil {
		t.Fatalf("execute work: %v", err)
	}
	if n := handle.inputCount(); n != 1 {
		t.Fatalf("adapter executed %d time(s), want 1", n)
	}
	got := handle.inputAt(0)
	if _, ok := got["working_directory"]; ok {
		t.Fatal("remote adapter with an undeclared input surface must not receive the engine-authored working_directory (CRI-270 review gate)")
	}
	if got["task"] != "run-intake" {
		t.Fatalf("adapter input task = %q, want run-intake (the step input must reach the adapter verbatim)", got["task"])
	}
	if _, ok := work.Input["working_directory"]; ok {
		t.Fatal("compiled step Input was mutated by the gated injection")
	}
}

func TestEngine_CRI270_ShellTypeWithoutDeclaredSurfaceStillInjects(t *testing.T) {
	ctx := context.Background()
	envWorkingDir := t.TempDir()
	g := compileCRI270RemoteAdapterTypeGraph(t, "shell", envWorkingDir)

	handle := &recordingHandle{}
	sessions := adapterhost.NewSessionManager(&fakeLoader{})
	sessions.SetGraph(g)
	sessions.SetRemoteShim(newFakeRemoteShim(handle))
	initCRI270Scope(t, g, sessions, "shell.default")

	work := g.Steps["work"]
	if work == nil {
		t.Fatal("fixture must declare step work")
	}
	if _, err := sessions.Execute(ctx, "shell.default", work, noopEventSink{}, nil); err != nil {
		t.Fatalf("execute work: %v", err)
	}
	if n := handle.inputCount(); n != 1 {
		t.Fatalf("adapter executed %d time(s), want 1", n)
	}
	got := handle.inputAt(0)
	if got["working_directory"] != envWorkingDir {
		t.Fatalf("adapter input working_directory = %q, want the injected environment working_directory %q (shell adapters honor the input contract)", got["working_directory"], envWorkingDir)
	}
	if got["task"] != "run-intake" {
		t.Fatalf("adapter input task = %q, want run-intake (the injection must not displace step input)", got["task"])
	}
	if _, ok := work.Input["working_directory"]; ok {
		t.Fatal("compiled step Input was mutated by the injection")
	}
}

func TestEngine_CRI270_RestoredRemoteSessionStillInjectsDeclaredKey(t *testing.T) {
	ctx := context.Background()
	envWorkingDir := t.TempDir()
	g := compileCRI270RemoteSchemaGraph(t, envWorkingDir)

	handle := &recordingHandle{schema: &workflow.AdapterInfo{
		InputSchema: map[string]workflow.ConfigField{
			"task":              {Type: workflow.ConfigFieldString},
			"working_directory": {Type: workflow.ConfigFieldString},
		},
	}}
	sessions := adapterhost.NewSessionManager(&fakeLoader{})
	sessions.SetGraph(g)
	sessions.SetRemoteShim(newFakeRemoteShim(handle))
	t.Cleanup(func() { _ = sessions.Shutdown(context.Background()) })

	snap := &adapterhost.SessionSnapshot{
		SchemaVersion:   1,
		HostArch:        runtime.GOOS + "/" + runtime.GOARCH,
		WorkingDir:      envWorkingDir,
		ScopeInstanceID: "scope-270",
	}
	if _, err := sessions.Restore(ctx, "noop.default", "noop", "", nil, nil, snap); err != nil {
		t.Fatalf("restore: %v", err)
	}

	work := g.Steps["work"]
	if work == nil {
		t.Fatal("fixture must declare step work")
	}
	if _, err := sessions.Execute(ctx, "noop.default", work, noopEventSink{}, nil); err != nil {
		t.Fatalf("execute work: %v", err)
	}
	if n := handle.inputCount(); n != 1 {
		t.Fatalf("adapter executed %d time(s), want 1", n)
	}
	got := handle.inputAt(0)
	if got["working_directory"] != envWorkingDir {
		t.Fatalf("adapter input working_directory = %q, want the injected environment working_directory %q (the restored session must keep its declared surface)", got["working_directory"], envWorkingDir)
	}
	if got["task"] != "run-intake" {
		t.Fatalf("adapter input task = %q, want run-intake (the injection must not displace step input)", got["task"])
	}
	if _, ok := work.Input["working_directory"]; ok {
		t.Fatal("compiled step Input was mutated by the injection")
	}
}

// -----------------------------------------------------------------------------
// CRI-293: Remote Shim Address Isolation
// -----------------------------------------------------------------------------

func cri293RemoteBody(t *testing.T, listen string) hcl.Body {
	t.Helper()
	parser := hclparse.NewParser()
	file, diags := parser.ParseHCL([]byte(fmt.Sprintf(`
listen_address = %q
insecure = true
`, listen)), "remote.hcl")
	if diags.HasErrors() {
		t.Fatalf("parse remote body: %s", diags)
	}
	return file.Body
}

func cri293Env(t *testing.T, name, listen string) *workflow.EnvironmentNode {
	t.Helper()
	return &workflow.EnvironmentNode{
		Type:    "remote",
		Name:    name,
		Process: &workflow.ProcessPolicy{Exec: []string{"*"}},
		RawBody: cri293RemoteBody(t, listen),
	}
}

func cri293Graph(t *testing.T, listen string) *workflow.FSMGraph {
	t.Helper()
	return &workflow.FSMGraph{
		PinSet: &lockfile.Lockfile{
			Adapters: []lockfile.LockedAdapter{
				{Type: "noop", ResolvedDigest: "sha256:abcd1234"},
			},
		},
		Environments: map[string]*workflow.EnvironmentNode{
			"remote.worktree": cri293Env(t, "worktree", listen),
			"remote.primary":  cri293Env(t, "primary", listen),
		},
	}
}

func (e *Engine) newCRI293Sessions(t *testing.T) *adapterhost.SessionManager {
	t.Helper()
	sessions := adapterhost.NewSessionManager(nil)
	e.setLockfileOnSessions(sessions)
	return sessions
}

func TestMaybeStartRemoteShim_CRI293_CollidingAddressesIsolatedLocally(t *testing.T) {
	const declared = "127.0.0.1:7779"
	g := cri293Graph(t, declared)
	eng := New(g, nil, &fakeSink{}, WithLocalShimIsolation())
	sessions := eng.newCRI293Sessions(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := eng.maybeStartRemoteShim(ctx, sessions); err != nil {
		t.Fatalf("maybeStartRemoteShim with isolation: %v", err)
	}
	t.Cleanup(func() {
		_ = sessions.Shutdown(context.WithoutCancel(ctx))
	})

	shimWorktree := sessions.RemoteShimForEnv("remote.worktree")
	if shimWorktree == nil {
		t.Fatal("RemoteShimForEnv(\"remote.worktree\") returned nil")
	}
	shimPrimary := sessions.RemoteShimForEnv("remote.primary")
	if shimPrimary == nil {
		t.Fatal("RemoteShimForEnv(\"remote.primary\") returned nil")
	}

	addrWorktree := shimWorktree.ListenAddr()
	addrPrimary := shimPrimary.ListenAddr()
	if addrWorktree == addrPrimary {
		t.Fatalf("both environment shims bound %q; want distinct auto-chosen ports", addrWorktree)
	}
	for name, addr := range map[string]string{"worktree": addrWorktree, "primary": addrPrimary} {
		if strings.HasSuffix(addr, ":7779") {
			t.Errorf("%s shim bound the declared port %q; want an auto-chosen port", name, addr)
		}
	}

	if !canDialLoopbackTCP() {
		t.Log("skipping phone-home reachability assertion: loopback TCP dialing is restricted in current environment")
		return
	}

	// Both shims must be reachable: a fake adapter phones home to each one
	// with a digest matching the pin set, and each shim hands back a handle.
	var infoCalls atomic.Int64
	stop := make(chan struct{})
	defer close(stop)
	hs := &cri137Handshake{Name: "noop", Version: "1.0.0", Digest: "sha256:abcd1234"}
	go dialCri137AdapterLoop(addrWorktree, hs, stop, &infoCalls)
	go dialCri137AdapterLoop(addrPrimary, hs, stop, &infoCalls)
	for name, shim := range map[string]adapterhost.RemoteShim{"worktree": shimWorktree, "primary": shimPrimary} {
		if _, err := shim.WaitForHandle(ctx, "noop", ""); err != nil {
			t.Errorf("%s shim: wait for adapter handle: %v", name, err)
		}
	}
}

func TestMaybeStartRemoteShim_CRI293_CollidingAddressesStillCollideWithoutIsolation(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("acquire loopback port: %v", err)
	}
	declared := l.Addr().String()
	_ = l.Close()

	g := cri293Graph(t, declared)
	eng := New(g, nil, &fakeSink{})
	sessions := eng.newCRI293Sessions(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = eng.maybeStartRemoteShim(ctx, sessions)
	t.Cleanup(func() {
		_ = sessions.Shutdown(context.WithoutCancel(ctx))
	})
	if err == nil {
		t.Fatal("maybeStartRemoteShim without isolation unexpectedly succeeded for colliding environments")
	}
	if !strings.Contains(err.Error(), "bind: address already in use") {
		t.Fatalf("expected bind address in use error, got: %v", err)
	}
}

func TestMaybeStartRemoteShim_CRI293_ForeignProcessCollisionKeepsAddressedError(t *testing.T) {
	foreign, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("bind foreign listener: %v", err)
	}
	defer foreign.Close()
	declared := foreign.Addr().String()

	g := &workflow.FSMGraph{
		PinSet: &lockfile.Lockfile{
			Adapters: []lockfile.LockedAdapter{
				{Type: "noop", ResolvedDigest: "sha256:abcd1234"},
			},
		},
		Environments: map[string]*workflow.EnvironmentNode{
			"remote.primary": cri293Env(t, "primary", declared),
		},
	}
	eng := New(g, nil, &fakeSink{}, WithLocalShimIsolation())
	sessions := eng.newCRI293Sessions(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = eng.maybeStartRemoteShim(ctx, sessions)
	t.Cleanup(func() {
		_ = sessions.Shutdown(context.WithoutCancel(ctx))
	})
	if err == nil {
		t.Fatal("expected bind error against occupied foreign port, got nil")
	}
	if !strings.Contains(err.Error(), declared) {
		t.Errorf("error %q does not name the occupied address %q", err.Error(), declared)
	}
	if !strings.Contains(err.Error(), "bind: address already in use") {
		t.Errorf("expected address already in use, got: %v", err)
	}
}

func TestIsolatedShimEnvs(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "cri293.sock")
	sockEnv := func(t *testing.T) *workflow.EnvironmentNode {
		parser := hclparse.NewParser()
		file, diags := parser.ParseHCL([]byte(fmt.Sprintf(`
listen_address = %q
insecure = true
`, sock)), "remote.hcl")
		if diags.HasErrors() {
			t.Fatalf("parse remote body: %s", diags)
		}
		return &workflow.EnvironmentNode{Type: "remote", Name: "x", RawBody: file.Body}
	}

	t.Run("same fixed port isolates both", func(t *testing.T) {
		envs := map[string]*workflow.EnvironmentNode{
			"remote.a": cri293Env(t, "a", "127.0.0.1:7779"),
			"remote.b": cri293Env(t, "b", "127.0.0.1:7779"),
		}
		got := isolatedShimEnvs(envs)
		if !got["remote.a"] || !got["remote.b"] {
			t.Errorf("isolatedShimEnvs = %v, want both isolated", got)
		}
	})
	t.Run("distinct ports do not isolate", func(t *testing.T) {
		envs := map[string]*workflow.EnvironmentNode{
			"remote.a": cri293Env(t, "a", "127.0.0.1:7779"),
			"remote.b": cri293Env(t, "b", "127.0.0.1:7780"),
		}
		if got := isolatedShimEnvs(envs); len(got) != 0 {
			t.Errorf("isolatedShimEnvs = %v, want none", got)
		}
	})
	t.Run("shared port zero does not isolate", func(t *testing.T) {
		envs := map[string]*workflow.EnvironmentNode{
			"remote.a": cri293Env(t, "a", "127.0.0.1:0"),
			"remote.b": cri293Env(t, "b", "127.0.0.1:0"),
		}
		if got := isolatedShimEnvs(envs); len(got) != 0 {
			t.Errorf("isolatedShimEnvs = %v, want none", got)
		}
	})
	t.Run("shared unix socket path does not isolate", func(t *testing.T) {
		envs := map[string]*workflow.EnvironmentNode{
			"remote.a": sockEnv(t),
			"remote.b": sockEnv(t),
		}
		if got := isolatedShimEnvs(envs); len(got) != 0 {
			t.Errorf("isolatedShimEnvs = %v, want none", got)
		}
	})
	t.Run("unparseable body does not isolate", func(t *testing.T) {
		envs := map[string]*workflow.EnvironmentNode{
			"remote.a": {Type: "remote", Name: "a", RawBody: nil},
			"remote.b": cri293Env(t, "b", "127.0.0.1:7779"),
		}
		if got := isolatedShimEnvs(envs); len(got) != 0 {
			t.Errorf("isolatedShimEnvs = %v, want none", got)
		}
	})
}

func TestHasFixedPort(t *testing.T) {
	for _, tc := range []struct {
		listen string
		want   bool
	}{
		{"0.0.0.0:7778", true},
		{"127.0.0.1:7779", true},
		{"[::1]:7779", true},
		{"127.0.0.1:0", false},
		{"localhost:0", false},
		{"/tmp/criteria.sock", false},
		{"", false},
	} {
		if got := hasFixedPort(tc.listen); got != tc.want {
			t.Errorf("hasFixedPort(%q) = %v, want %v", tc.listen, got, tc.want)
		}
	}
}

// -----------------------------------------------------------------------------
// KB-238: Subworkflow-Declared Environment Shims & Composition
// -----------------------------------------------------------------------------

const kb238DevelopRootHCL = `workflow {
  name          = "kb238-develop"
  version       = "0.1"
  initial_state = "start"
  target_state  = "done"
}

environment "remote" "develop" {
  listen_address     = "127.0.0.1:0"
  per_scope_sessions = true
}

environment "shell" "local" {}

adapter "shell" "castle" {
  environment = remote.develop
}

adapter "noop" "default" {
  environment = shell.local
}

subworkflow "handler" {
  source = "./handler"
}

step "start" {
  target = adapter.noop.default
  outcome "success" { next = step.engage }
  outcome "error" { next = step.engage }
}

step "engage" {
  target = subworkflow.handler
  outcome "success" { next = state.done }
  outcome "failure" { next = state.failed }
}

state "done" {
  terminal = true
  success  = true
}

state "failed" {
  terminal = true
  success  = false
}`

const kb238DevelopRootNoUseHCL = `workflow {
  name          = "kb238-develop-open"
  version       = "0.1"
  initial_state = "start"
  target_state  = "done"
}

environment "remote" "develop" {
  listen_address     = "127.0.0.1:0"
  per_scope_sessions = true
}

environment "shell" "local" {}

adapter "noop" "default" {
  environment = shell.local
}

subworkflow "handler" {
  source = "./handler"
}

step "start" {
  target = adapter.noop.default
  outcome "success" { next = step.engage }
  outcome "error" { next = step.engage }
}

step "engage" {
  target = subworkflow.handler
  outcome "success" { next = state.done }
  outcome "failure" { next = state.failed }
}

state "done" {
  terminal = true
  success  = true
}

state "failed" {
  terminal = true
  success  = false
}`

const kb238DevelopRootAlsoWorktreeHCL = `workflow {
  name          = "kb238-develop-shared"
  version       = "0.1"
  initial_state = "start"
  target_state  = "done"
}

environment "remote" "develop" {
  listen_address     = "127.0.0.1:0"
  per_scope_sessions = true
}

environment "remote" "worktree" {
  listen_address     = "127.0.0.1:0"
  per_scope_sessions = true
}

environment "shell" "local" {}

adapter "noop" "default" {
  environment = shell.local
}

subworkflow "handler" {
  source = "./handler"
}

step "start" {
  target = adapter.noop.default
  outcome "success" { next = step.engage }
  outcome "error" { next = step.engage }
}

step "engage" {
  target = subworkflow.handler
  outcome "success" { next = state.done }
  outcome "failure" { next = state.failed }
}

state "done" {
  terminal = true
  success  = true
}

state "failed" {
  terminal = true
  success  = false
}`

const kb238HandlerHCL = `workflow {
  name          = "workstream_handler_v1"
  version       = "0.1"
  initial_state = "setup"
  target_state  = "done"
}

environment "remote" "worktree" {
  listen_address     = "127.0.0.1:0"
  per_scope_sessions = true
}

environment "remote" "primary" {
  listen_address     = "127.0.0.1:0"
  per_scope_sessions = true
}

adapter "copilot" "coordinator" {
  environment = remote.worktree
}

step "setup" {
  target = adapter.copilot.coordinator
  outcome "success" { next = state.done }
  outcome "failure" { next = state.failed }
}

state "done" {
  terminal = true
  success  = true
}

state "failed" {
  terminal = true
  success  = false
}`

func kb238CombinedLockfile() *lockfile.Lockfile {
	return &lockfile.Lockfile{
		SchemaVersion: 1,
		Adapters: []lockfile.LockedAdapter{
			{
				Type:               "copilot",
				Name:               "coordinator",
				Reference:          "ghcr.io/brokenbots/criteria-adapter-copilot",
				ResolvedDigest:     pinDigest,
				SourceURL:          "https://github.com/brokenbots/criteria",
				SDKProtocolVersion: 2,
				Platforms:          []string{"linux/amd64"},
			},
			{
				Type:               "shell",
				Name:               "castle",
				Reference:          "ghcr.io/brokenbots/criteria-adapter-shell",
				ResolvedDigest:     pinDigest,
				SourceURL:          "https://github.com/brokenbots/criteria",
				SDKProtocolVersion: 2,
				Platforms:          []string{"linux/amd64"},
			},
		},
	}
}

func kb238Statuses(sink *eventTrackingSink, status string) []AdapterLifecycleEvent {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	out := make([]AdapterLifecycleEvent, 0, len(sink.provisionEvents))
	for i := range sink.provisionEvents {
		ev := &sink.provisionEvents[i]
		if ev.Status != status {
			continue
		}
		out = append(out, *ev)
	}
	return out
}

type kb238RunResult struct {
	err        error
	sink       *eventTrackingSink
	executions *atomic.Int64
}

func kb238RunShape(t *testing.T, rootHCL, variant string, servePods bool, budget time.Duration) *kb238RunResult {
	t.Helper()
	if servePods && !canDialLoopbackTCP() {
		t.Skip("skipping test: loopback TCP dialing is restricted in current environment")
	}
	if servePods && testing.Short() {
		t.Skip("skipping integration test with live pods in -short mode")
	}
	root := t.TempDir()
	writeFile(t, root+"/main.chcl", rootHCL)
	writeFile(t, root+"/handler/main.chcl", kb238HandlerHCL)
	writeLockfile(t, root, kb238CombinedLockfile())
	g := compileWorkflowDir(t, root)

	sink := &eventTrackingSink{}
	e := NewTestEngine(g, &fakeLoader{adapters: map[string]adapterhost.Handle{
		"noop": &fakeAdapter{name: "noop", outcome: "success"},
	}}, sink,
		WithWorkflowDir(g.WorkflowDir),
		WithDataDir(root),
		WithRunID("kb238-"+variant))

	res := &kb238RunResult{sink: sink, executions: new(atomic.Int64)}
	stopPod := make(chan struct{})
	t.Cleanup(func() { close(stopPod) })
	if servePods {
		go kb238PodServeType(t, sink, stopPod, "copilot", res.executions)
		go kb238PodServeType(t, sink, stopPod, "shell", res.executions)
	}

	ctx, cancel := context.WithTimeout(context.Background(), budget)
	t.Cleanup(cancel)
	runErr := make(chan error, 1)
	go func() { runErr <- e.Run(ctx) }()
	select {
	case err := <-runErr:
		res.err = err
	case <-time.After(budget + 5*time.Second):
		t.Fatalf("[%s] run did not return within %s", variant, budget+5*time.Second)
	}
	return res
}

func kb238ProvisionsFor(t *testing.T, sink *eventTrackingSink, adapterType string) []AdapterLifecycleEvent {
	t.Helper()
	events := kb238Statuses(sink, "provision_wanted")
	var out []AdapterLifecycleEvent
	for i := range events {
		ev := &events[i]
		if ev.AdapterType == adapterType {
			out = append(out, *ev)
		}
	}
	if len(out) == 0 {
		t.Fatalf("no provision_wanted events for adapter %q; provisions=%+v", adapterType, kb238Statuses(sink, "provision_wanted"))
	}
	return out
}

func kb238AssertHealthyRun(t *testing.T, res *kb238RunResult, variant string) {
	t.Helper()
	if res.err != nil {
		t.Fatalf("[%s] run error: %v", variant, res.err)
	}
	if res.sink.terminal != "done" || !res.sink.terminalOK {
		t.Fatalf("[%s] terminal=%q ok=%v failure=%q; want done/true\nstep outcomes:\n%s",
			variant, res.sink.terminal, res.sink.terminalOK, res.sink.failure,
			strings.Join(res.sink.stepOutcomes(), "\n"))
	}
	for _, line := range res.sink.stepOutcomes() {
		if !strings.Contains(line, "outcome=success") || !strings.Contains(line, "err=<nil>") {
			t.Fatalf("[%s] unexpected step outcome: %s", variant, line)
		}
	}
}

func kb238PodServeType(t *testing.T, sink *eventTrackingSink, stop <-chan struct{}, wantType string, executions *atomic.Int64) {
	t.Helper()
	var hs *cri137Handshake
	var addr string
	for {
		select {
		case <-stop:
			return
		case <-time.After(5 * time.Millisecond):
		}
		events := kb238Statuses(sink, "provision_wanted")
		for i := range events {
			ev := &events[i]
			if ev.AdapterType != wantType {
				continue
			}
			token, err := os.ReadFile(ev.TokenRef)
			if err != nil {
				continue
			}
			addr = ev.ShimListenAddress
			hs = &cri137Handshake{
				Name:    strings.TrimSuffix(filepath.Base(ev.TokenRef), ".token"),
				Version: "1.0.0",
				Digest:  pinDigest,
				Token:   string(token),
				Scope:   ev.ScopeName + "/" + ev.ScopeInstanceID,
			}
			break
		}
		if hs != nil {
			break
		}
	}
	for {
		select {
		case <-stop:
			return
		default:
		}
		if !(&cri145Pod{executions: executions}).dialOnce(addr, hs, stop) {
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Millisecond):
			}
		}
	}
}

func TestKB238_SubworkflowEnvAdapterRunsOnItsOwnEnvShim(t *testing.T) {
	res := kb238RunShape(t, kb238DevelopRootHCL, "develop", true, 60*time.Second)
	kb238AssertHealthyRun(t, res, "develop")

	shell := kb238ProvisionsFor(t, res.sink, "shell")
	copilot := kb238ProvisionsFor(t, res.sink, "copilot")
	if len(shell) != 1 {
		t.Fatalf("shell provision events = %d, want 1", len(shell))
	}
	if len(copilot) != 1 {
		t.Fatalf("copilot provision events = %d, want 1", len(copilot))
	}
	if got := fmt.Sprintf("%s.%s", shell[0].EnvironmentType, shell[0].EnvironmentName); got != "remote.develop" {
		t.Fatalf("shell provision env = %q, want remote.develop", got)
	}
	if got := fmt.Sprintf("%s.%s", copilot[0].EnvironmentType, copilot[0].EnvironmentName); got != "remote.worktree" {
		t.Fatalf("copilot provision env = %q, want remote.worktree", got)
	}
	if copilot[0].ShimListenAddress == "" {
		t.Fatal("copilot provision event carries no shim address")
	}
	if copilot[0].ShimListenAddress == shell[0].ShimListenAddress {
		t.Fatalf("copilot@remote.worktree was published on the develop shim %q; the subworkflow env has no shim of its own", copilot[0].ShimListenAddress)
	}
	if n := res.executions.Load(); n < 1 {
		t.Fatalf("copilot executions = %d, want >= 1 (the body's setup step must execute through the shim)", n)
	}
}

func TestKB238_SubworkflowDeclaredEnvWithoutRootUse(t *testing.T) {
	res := kb238RunShape(t, kb238DevelopRootNoUseHCL, "open", true, 60*time.Second)
	kb238AssertHealthyRun(t, res, "open")

	copilot := kb238ProvisionsFor(t, res.sink, "copilot")
	if len(copilot) != 1 {
		t.Fatalf("copilot provision events = %d, want 1", len(copilot))
	}
	if got := fmt.Sprintf("%s.%s", copilot[0].EnvironmentType, copilot[0].EnvironmentName); got != "remote.worktree" {
		t.Fatalf("copilot provision env = %q, want remote.worktree", got)
	}
	if n := res.executions.Load(); n < 1 {
		t.Fatalf("copilot executions = %d, want >= 1", n)
	}
}

func TestKB238_RootAndSubworkflowDeclareSameEnvironment(t *testing.T) {
	res := kb238RunShape(t, kb238DevelopRootAlsoWorktreeHCL, "shared", true, 60*time.Second)
	kb238AssertHealthyRun(t, res, "shared")
	copilot := kb238ProvisionsFor(t, res.sink, "copilot")
	if len(copilot) != 1 {
		t.Fatalf("copilot provision events = %d, want 1", len(copilot))
	}
	if n := res.executions.Load(); n < 1 {
		t.Fatalf("copilot executions = %d, want >= 1", n)
	}
}

func TestKB238_ShimEnvironmentDeclarationsComposition(t *testing.T) {
	developRoot := &workflow.EnvironmentNode{Type: "remote", Name: "develop"}
	worktreeRoot := &workflow.EnvironmentNode{Type: "remote", Name: "worktree"}
	shellLocal := &workflow.EnvironmentNode{Type: "shell", Name: "local"}
	worktreeBody := &workflow.EnvironmentNode{Type: "remote", Name: "worktree"}
	primaryBody := &workflow.EnvironmentNode{Type: "remote", Name: "primary"}
	nestedBody := &workflow.EnvironmentNode{Type: "remote", Name: "nested"}

	cases := []struct {
		name string
		root *workflow.FSMGraph
		want map[string]*workflow.EnvironmentNode
	}{
		{
			name: "root only ignores non-remote envs",
			root: &workflow.FSMGraph{
				Environments: map[string]*workflow.EnvironmentNode{
					"remote.develop": developRoot,
					"shell.local":    shellLocal,
				},
			},
			want: map[string]*workflow.EnvironmentNode{"remote.develop": developRoot},
		},
		{
			name: "body only",
			root: &workflow.FSMGraph{
				Subworkflows: map[string]*workflow.SubworkflowNode{
					"handler": {Name: "handler", Body: &workflow.FSMGraph{
						Environments: map[string]*workflow.EnvironmentNode{
							"remote.worktree": worktreeBody,
						},
					}},
				},
				SubworkflowOrder: []string{"handler"},
			},
			want: map[string]*workflow.EnvironmentNode{"remote.worktree": worktreeBody},
		},
		{
			name: "union of root and bodies",
			root: &workflow.FSMGraph{
				Environments: map[string]*workflow.EnvironmentNode{"remote.develop": developRoot},
				Subworkflows: map[string]*workflow.SubworkflowNode{
					"handler": {Name: "handler", Body: &workflow.FSMGraph{
						Environments: map[string]*workflow.EnvironmentNode{
							"remote.worktree": worktreeBody,
							"remote.primary":  primaryBody,
						},
					}},
				},
				SubworkflowOrder: []string{"handler"},
			},
			want: map[string]*workflow.EnvironmentNode{
				"remote.develop":  developRoot,
				"remote.worktree": worktreeBody,
				"remote.primary":  primaryBody,
			},
		},
		{
			name: "root wins on shared key",
			root: &workflow.FSMGraph{
				Environments: map[string]*workflow.EnvironmentNode{"remote.worktree": worktreeRoot},
				Subworkflows: map[string]*workflow.SubworkflowNode{
					"handler": {Name: "handler", Body: &workflow.FSMGraph{
						Environments: map[string]*workflow.EnvironmentNode{
							"remote.worktree": worktreeBody,
						},
					}},
				},
				SubworkflowOrder: []string{"handler"},
			},
			want: map[string]*workflow.EnvironmentNode{"remote.worktree": worktreeRoot},
		},
		{
			name: "nested subworkflow bodies",
			root: &workflow.FSMGraph{
				SubworkflowOrder: []string{"outer"},
				Subworkflows: map[string]*workflow.SubworkflowNode{
					"outer": {Name: "outer", Body: &workflow.FSMGraph{
						SubworkflowOrder: []string{"inner"},
						Subworkflows: map[string]*workflow.SubworkflowNode{
							"inner": {Name: "inner", Body: &workflow.FSMGraph{
								Environments: map[string]*workflow.EnvironmentNode{
									"remote.nested": nestedBody,
								},
							}},
						},
					}},
				},
			},
			want: map[string]*workflow.EnvironmentNode{"remote.nested": nestedBody},
		},
		{
			name: "nil graph",
			root: nil,
			want: map[string]*workflow.EnvironmentNode{},
		},
		{
			name: "nil subworkflow node tolerated",
			root: &workflow.FSMGraph{
				Subworkflows: map[string]*workflow.SubworkflowNode{
					"gone": nil,
				},
				SubworkflowOrder: []string{"gone"},
			},
			want: map[string]*workflow.EnvironmentNode{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shimEnvironmentDeclarations(tc.root)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d envs %v, want %d", len(got), kb238EnvKeys(got), len(tc.want))
			}
			for key, node := range tc.want {
				if got[key] != node {
					t.Fatalf("env %q: wrong declaration node selected", key)
				}
			}
		})
	}
}

func kb238EnvKeys(m map[string]*workflow.EnvironmentNode) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
