package engine

// cri270_repro_test.go — CRI-270: per-scope remote adapters never received the
// environment's working_directory. The engine resolves the environment
// working_directory at adapter init and stores it on the session, but it only
// reaches a process via the locally-launched command customizer (cmd.Dir);
// remotely dispatched adapters (per-scope operator pods, phone-home adapters)
// are spawned by the remote host and never launched by this engine, so every
// bare-git shell step ran in the image default cwd and failed with
// "fatal: not a git repository".
//
// The fix delivers the session's resolved working_directory to a remote
// adapter through the per-step input key it already honors
// ("working_directory"), injected only when the step does not set one —
// per-step inputs keep winning, local/container sessions keep their
// customizer-owned cwd, and the compiled step node is never mutated. The
// injection is additionally gated on the adapter's declared input surface
// (the InputSchema captured at the verify handshake): a non-empty surface
// that does not declare working_directory never receives the key, so
// adapters that forward arbitrary input keys onward (e.g. mcp tool
// arguments) are not corrupted; nil/empty surfaces stay permissive.
//
// These tests pin that contract at the session boundary: a remote-path step
// Execute must deliver the resolved environment working_directory as input
// working_directory, a per-step working_directory input must not be
// overridden, a remote session without a working_directory must inject
// nothing, and a local session must receive no injection.

import (
	"context"
	"fmt"
	"sync"
	"testing"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow"
)

// recordingHandle is an adapterhost.Handle that records the Input map of every
// step handed to Execute, so tests can assert exactly what the adapter
// received on the wire. schema, when non-nil, is returned from Info so the
// host caches the adapter's declared input surface during the verify
// handshake (the withRemoteWorkingDir gate reads it).
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
func (h *recordingHandle) Execute(_ context.Context, _ string, step *workflow.StepNode, _ adapter.EventSink) (adapter.Result, error) {
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

// compileCRI270RemoteGraph compiles a per-scope remote workflow whose
// environment declares working_directory and whose two steps exercise the
// injection precedence: "work" passes no working_directory (injection must
// apply) and "override" passes its own (injection must not apply).
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
}

step "override" {
  target = adapter.noop.default
  input {
    working_directory = "/cri270/step-own-cwd"
  }
  outcome "success" { next = state.done }
}

state "done" {
  terminal = true
  success  = true
}`, envWorkingDir))
}

// initCRI270Scope provisions the compiled graph's adapters exactly as the
// engine does at scope entry, so the session carries the resolved environment
// working_directory before any step runs.
func initCRI270Scope(t *testing.T, g *workflow.FSMGraph, sessions *adapterhost.SessionManager) {
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
	if len(order) != 1 || order[0] != "noop.default" {
		t.Fatalf("expected order [noop.default], got %v", order)
	}
	t.Cleanup(func() { _ = sessions.Shutdown(context.Background()) })
}

// TestEngine_CRI270_RemoteAdapterReceivesEnvironmentWorkingDir drives step
// Executes through the session manager over the remote (per-scope shim) path
// and asserts the adapter receives the environment's resolved
// working_directory as its working_directory input.
func TestEngine_CRI270_RemoteAdapterReceivesEnvironmentWorkingDir(t *testing.T) {
	ctx := context.Background()
	envWorkingDir := t.TempDir()
	g := compileCRI270RemoteGraph(t, envWorkingDir)

	handle := &recordingHandle{}
	sessions := adapterhost.NewSessionManager(&fakeLoader{})
	sessions.SetGraph(g)
	sessions.SetRemoteShim(newFakeRemoteShim(handle))
	initCRI270Scope(t, g, sessions)

	// A step with no working_directory input must receive the environment's
	// resolved working_directory.
	work := g.Steps["work"]
	if work == nil {
		t.Fatal("fixture must declare step work")
	}
	if got, ok := work.Input["working_directory"]; ok {
		t.Fatalf("compiled step must not pre-carry working_directory, got %q", got)
	}
	if _, err := sessions.Execute(ctx, "noop.default", work, noopEventSink{}); err != nil {
		t.Fatalf("execute work: %v", err)
	}
	if n := handle.inputCount(); n != 1 {
		t.Fatalf("adapter executed %d time(s), want 1", n)
	}
	if got := handle.inputAt(0)["working_directory"]; got != envWorkingDir {
		t.Fatalf("adapter input working_directory = %q, want the resolved environment working_directory %q (CRI-270)", got, envWorkingDir)
	}

	// The compiled step node must remain untouched: re-running the same step
	// (e.g. after a respawn or in another scope) must still receive a fresh
	// injection, and no other consumer of the graph may observe the injected
	// key as if the workflow author had written it.
	if _, ok := work.Input["working_directory"]; ok {
		t.Fatal("compiled step Input was mutated by the injection")
	}

	// A step that declares its own working_directory input must keep it: the
	// per-step value wins and the environment value is not merged in.
	override := g.Steps["override"]
	if override == nil {
		t.Fatal("fixture must declare step override")
	}
	if _, err := sessions.Execute(ctx, "noop.default", override, noopEventSink{}); err != nil {
		t.Fatalf("execute override: %v", err)
	}
	if n := handle.inputCount(); n != 2 {
		t.Fatalf("adapter executed %d time(s), want 2", n)
	}
	if got := handle.inputAt(1)["working_directory"]; got != "/cri270/step-own-cwd" {
		t.Fatalf("per-step working_directory was overridden: got %q, want /cri270/step-own-cwd", got)
	}
}

// TestEngine_CRI270_RemoteWithoutWorkingDirInjectsNothing pins that a remote
// session whose environment declares no working_directory receives no
// working_directory input.
func TestEngine_CRI270_RemoteWithoutWorkingDirInjectsNothing(t *testing.T) {
	ctx := context.Background()
	g := perScopeRemoteGraph(t)

	handle := &recordingHandle{}
	sessions := adapterhost.NewSessionManager(&fakeLoader{})
	sessions.SetGraph(g)
	sessions.SetRemoteShim(newFakeRemoteShim(handle))
	initCRI270Scope(t, g, sessions)

	work := g.Steps["start"]
	if work == nil {
		t.Fatal("fixture must declare step start")
	}
	if _, err := sessions.Execute(ctx, "noop.default", work, noopEventSink{}); err != nil {
		t.Fatalf("execute start: %v", err)
	}
	if n := handle.inputCount(); n != 1 {
		t.Fatalf("adapter executed %d time(s), want 1", n)
	}
	if got, ok := handle.inputAt(0)["working_directory"]; ok {
		t.Fatalf("remote session without a working_directory must not inject one, got %q", got)
	}
}

// TestEngine_CRI270_LocalAdapterGetsNoInjection pins that locally launched
// (shell environment) sessions keep their customizer-owned cwd behavior: the
// engine must not inject working_directory into their step inputs, where an
// unexpected key would change the adapter-observed contract.
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
	if _, err := sessions.Execute(ctx, "noop.default", work, noopEventSink{}); err != nil {
		t.Fatalf("execute work: %v", err)
	}
	if n := handle.inputCount(); n != 1 {
		t.Fatalf("adapter executed %d time(s), want 1", n)
	}
	if got, ok := handle.inputAt(0)["working_directory"]; ok {
		t.Fatalf("local session must receive no injected working_directory, got %q", got)
	}
}

// compileCRI270RemoteSchemaGraph compiles the same per-scope remote shape as
// compileCRI270RemoteGraph, but its single step carries a regular input key
// (no working_directory), so the gating tests can pin exactly what the
// adapter receives when the engine's declared-input-surface gate is in play.
// The declared surface itself is authored by the adapter binary (returned
// from Info during the verify handshake), mirroring how a manifest-declared
// adapter surface reaches the host.
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
}

state "done" {
  terminal = true
  success  = true
}`, envWorkingDir))
}

// TestEngine_CRI270_InjectionGatedOnDeclaredInputSchema pins the review gate:
// a remote adapter whose declared input surface does NOT include
// working_directory must never receive the engine-injected key. The mcp
// adapter forwards every non-reserved input key to its MCP server as a tool
// argument, so an injected undeclared key would corrupt every tool call, and
// the nested callee path (calleeInputFromArgs) already rejects undeclared
// keys — the engine injection must not create them.
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
	initCRI270Scope(t, g, sessions)

	work := g.Steps["work"]
	if work == nil {
		t.Fatal("fixture must declare step work")
	}
	if _, err := sessions.Execute(ctx, "noop.default", work, noopEventSink{}); err != nil {
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

// TestEngine_CRI270_InjectionOnDeclaredWorkingDirKey pins the positive side
// of the gate: a remote adapter whose declared input surface explicitly
// includes working_directory still receives the engine-injected environment
// working_directory (the standard per-scope shell-style adapter shape), with
// the step's own input keys preserved and the compiled step unmutated.
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
	initCRI270Scope(t, g, sessions)

	work := g.Steps["work"]
	if work == nil {
		t.Fatal("fixture must declare step work")
	}
	if _, err := sessions.Execute(ctx, "noop.default", work, noopEventSink{}); err != nil {
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
