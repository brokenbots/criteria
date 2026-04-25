package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/brokenbots/overlord/overseer/internal/adapter"
	"github.com/brokenbots/overlord/overseer/internal/plugin"
	"github.com/brokenbots/overlord/overseer/internal/testutil"
	"github.com/brokenbots/overlord/workflow"
)

// fakeSink records engine callbacks for assertion.
type fakeSink struct {
	mu          sync.Mutex
	stepsRun    []string
	transitions []string
	terminal    string
	terminalOK  bool
	failure     string
}

func (s *fakeSink) OnRunStarted(string, string) {}
func (s *fakeSink) OnRunCompleted(state string, ok bool) {
	s.mu.Lock()
	s.terminal = state
	s.terminalOK = ok
	s.mu.Unlock()
}
func (s *fakeSink) OnRunFailed(reason, step string) { s.failure = reason }
func (s *fakeSink) OnStepEntered(step, _ string, _ int) {
	s.mu.Lock()
	s.stepsRun = append(s.stepsRun, step)
	s.mu.Unlock()
}
func (s *fakeSink) OnStepOutcome(string, string, time.Duration, error) {}
func (s *fakeSink) OnStepTransition(from, to, via string) {
	s.mu.Lock()
	s.transitions = append(s.transitions, from+"->"+to)
	s.mu.Unlock()
}
func (s *fakeSink) OnStepResumed(string, int, string)           {}
func (s *fakeSink) StepEventSink(step string) adapter.EventSink { return noopSink{} }

type noopSink struct{}

func (noopSink) Log(string, []byte)  {}
func (noopSink) Adapter(string, any) {}

// fakePlugin returns a programmable outcome.
type fakePlugin struct {
	name string

	outcome string
	err     error
}

func (p *fakePlugin) Info(context.Context) (plugin.Info, error) {
	return plugin.Info{Name: p.name, Version: "test"}, nil
}

func (p *fakePlugin) OpenSession(context.Context, string, map[string]string) error { return nil }

func (p *fakePlugin) Execute(_ context.Context, _ string, _ *workflow.StepNode, _ adapter.EventSink) (adapter.Result, error) {
	return adapter.Result{Outcome: p.outcome}, p.err
}

func (p *fakePlugin) Permit(context.Context, string, string, bool, string) error { return nil }
func (p *fakePlugin) CloseSession(context.Context, string) error                 { return nil }
func (p *fakePlugin) Kill()                                                      {}

type fakeLoader struct {
	plugins map[string]plugin.Plugin
}

func (l *fakeLoader) Resolve(_ context.Context, name string) (plugin.Plugin, error) {
	p, ok := l.plugins[name]
	if !ok {
		return nil, fmt.Errorf("no plugin named %q", name)
	}
	return p, nil
}

func (l *fakeLoader) Shutdown(context.Context) error { return nil }

func compile(t *testing.T, src string) *workflow.FSMGraph {
	t.Helper()
	spec, diags := workflow.Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := workflow.Compile(spec)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	return g
}

func TestEngineHappyPath(t *testing.T) {
	g := compile(t, `
workflow "t" {
  version = "0.1"
  initial_state = "a"
  target_state  = "done"
  step "a" {
    adapter = "fake"
    outcome "success" { transition_to = "b" }
  }
  step "b" {
    adapter = "fake"
    outcome "success" { transition_to = "done" }
  }
  state "done" { terminal = true }
}`)
	sink := &fakeSink{}
	loader := &fakeLoader{plugins: map[string]plugin.Plugin{"fake": &fakePlugin{name: "fake", outcome: "success"}}}
	if err := New(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if sink.terminal != "done" || !sink.terminalOK {
		t.Errorf("terminal: %s ok=%v", sink.terminal, sink.terminalOK)
	}
	if len(sink.stepsRun) != 2 {
		t.Errorf("steps run: %v", sink.stepsRun)
	}
}

func TestEngineErrorMappedToFailureOutcome(t *testing.T) {
	g := compile(t, `
workflow "t" {
  version = "0.1"
  initial_state = "a"
  target_state  = "fail"
  step "a" {
    adapter = "fake"
    outcome "success" { transition_to = "ok" }
    outcome "failure" { transition_to = "fail" }
  }
  state "ok" { terminal = true }
  state "fail" {
    terminal = true
    success  = false
  }
}`)
	sink := &fakeSink{}
	loader := &fakeLoader{plugins: map[string]plugin.Plugin{"fake": &fakePlugin{name: "fake", outcome: "", err: errors.New("boom")}}}
	if err := New(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if sink.terminal != "fail" || sink.terminalOK {
		t.Errorf("terminal: %s ok=%v", sink.terminal, sink.terminalOK)
	}
}

func TestEngineMaxStepsGuard(t *testing.T) {
	g := compile(t, `
workflow "t" {
  version = "0.1"
  initial_state = "a"
  target_state  = "done"
  step "a" {
    adapter = "fake"
    outcome "again" { transition_to = "a" }
  }
  state "done" { terminal = true }
  policy { max_total_steps = 3 }
}`)
	sink := &fakeSink{}
	loader := &fakeLoader{plugins: map[string]plugin.Plugin{"fake": &fakePlugin{name: "fake", outcome: "again"}}}
	err := New(g, loader, sink).Run(context.Background())
	if err == nil {
		t.Fatal("expected loop guard error")
	}
}

func TestEngineLifecycleWithNoopPlugin(t *testing.T) {
	pluginBin := buildNoopPlugin(t)
	loader := plugin.NewLoaderWithDiscovery(func(string) (string, error) {
		return pluginBin, nil
	})

	g := compileFile(t, "testdata/agent_lifecycle_noop.hcl")
	sink := &fakeSink{}
	if err := New(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if sink.terminal != "done" || !sink.terminalOK {
		t.Fatalf("terminal state: %s (ok=%v)", sink.terminal, sink.terminalOK)
	}
}

func TestEngineLifecycleOpenTimeoutKeepsSessionAlive(t *testing.T) {
	pluginBin := buildNoopPlugin(t)
	loader := plugin.NewLoaderWithDiscovery(func(string) (string, error) {
		return pluginBin, nil
	})

	g := compileFile(t, "testdata/agent_lifecycle_noop_open_timeout.hcl")
	sink := &captureStepEventSink{}
	if err := New(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if sink.terminal != "done" || !sink.terminalOK {
		t.Fatalf("terminal state: %s (ok=%v)", sink.terminal, sink.terminalOK)
	}
	if sink.sawAdapterKind("session.crash") {
		t.Fatal("unexpected session.crash event")
	}
	if sink.sawAdapterKind("session.respawned") {
		t.Fatal("unexpected session.respawned event")
	}
}

type captureStepEventSink struct {
	fakeSink

	mu     sync.Mutex
	events []adapterEventRecord
}

type adapterEventRecord struct {
	kind string
	data map[string]any
}

func (s *captureStepEventSink) StepEventSink(string) adapter.EventSink {
	return captureEventSink{s: s}
}

func (s *captureStepEventSink) sawAdapterKind(kind string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, evt := range s.events {
		if evt.kind == kind {
			return true
		}
	}
	return false
}

func (s *captureStepEventSink) firstAdapterEvent(kind string) (map[string]any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, evt := range s.events {
		if evt.kind == kind {
			return evt.data, true
		}
	}
	return nil, false
}

type captureEventSink struct {
	s *captureStepEventSink
}

func (s captureEventSink) Log(string, []byte) {}

func (s captureEventSink) Adapter(kind string, data any) {
	s.s.mu.Lock()
	defer s.s.mu.Unlock()
	var payload map[string]any
	if m, ok := data.(map[string]any); ok {
		payload = m
	}
	s.s.events = append(s.s.events, adapterEventRecord{kind: kind, data: payload})
}

func compileFile(t *testing.T, rel string) *workflow.FSMGraph {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve caller")
	}
	srcPath := filepath.Join(filepath.Dir(file), rel)
	src, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatalf("read workflow fixture: %v", err)
	}
	spec, diags := workflow.Parse(srcPath, src)
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := workflow.Compile(spec)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	return g
}

func buildNoopPlugin(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve caller path")
	}
	moduleRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	pluginBin := filepath.Join(t.TempDir(), "overlord-adapter-noop")

	cmd := exec.Command("go", "build", "-o", pluginBin, "./cmd/overlord-adapter-noop")
	cmd.Dir = moduleRoot
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build noop plugin: %v\n%s", err, string(output))
	}

	return pluginBin
}

// TestEnginePermissionGrantAndDeny verifies the full permission-gating flow:
// a step with allow_tools=["read_file"] against the permissive test plugin
// that requests two tools produces exactly one permission.granted event for
// read_file and one permission.denied event for write_file, and the run ends
// in needs_review (because the plugin returns needs_review on any denial).
func TestEnginePermissionGrantAndDeny(t *testing.T) {
	pluginBin := testutil.BuildPermissivePlugin(t)
	loader := plugin.NewLoaderWithDiscovery(func(string) (string, error) {
		return pluginBin, nil
	})
	t.Cleanup(func() { _ = loader.Shutdown(context.Background()) })

	g := compile(t, `
workflow "perm" {
  version       = "0.1"
  initial_state = "open"
  target_state  = "done"

  agent "bot" { adapter = "permissive" }

  step "open" {
    agent     = "bot"
    lifecycle = "open"
    outcome "success" { transition_to = "run" }
  }
  step "run" {
    agent       = "bot"
    config      = { perm_tools = "read_file,write_file" }
    allow_tools = ["read_file"]
    outcome "success"      { transition_to = "close" }
    outcome "needs_review" { transition_to = "close" }
  }
  step "close" {
    agent     = "bot"
    lifecycle = "close"
    outcome "success" { transition_to = "done" }
  }
  state "done" { terminal = true }
}`)

	sink := &captureStepEventSink{}
	if err := New(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if sink.terminal != "done" || !sink.terminalOK {
		t.Fatalf("terminal: %s ok=%v", sink.terminal, sink.terminalOK)
	}
	if !sink.sawAdapterKind("permission.granted") {
		t.Error("expected permission.granted event")
	}
	if !sink.sawAdapterKind("permission.denied") {
		t.Error("expected permission.denied event")
	}
	if sink.sawAdapterKind("permission.request") {
		t.Error("unexpected legacy permission.request event: policy should emit granted/denied instead")
	}
	granted, ok := sink.firstAdapterEvent("permission.granted")
	if !ok {
		t.Fatal("expected permission.granted payload")
	}
	if granted["pattern"] != "read_file" {
		t.Fatalf("permission.granted pattern=%v want read_file", granted["pattern"])
	}
	if granted["request_id"] == "" {
		t.Fatal("permission.granted request_id must be non-empty")
	}
	denied, ok := sink.firstAdapterEvent("permission.denied")
	if !ok {
		t.Fatal("expected permission.denied payload")
	}
	if denied["reason"] == "" {
		t.Fatal("permission.denied reason must be non-empty")
	}
}

// TestEngineDefaultPolicyDeniesAll verifies that a workflow without allow_tools
// produces a permission.denied event for every tool request and no
// permission.granted events.
func TestEngineDefaultPolicyDeniesAll(t *testing.T) {
	pluginBin := testutil.BuildPermissivePlugin(t)
	loader := plugin.NewLoaderWithDiscovery(func(string) (string, error) {
		return pluginBin, nil
	})
	t.Cleanup(func() { _ = loader.Shutdown(context.Background()) })

	g := compile(t, `
workflow "perm-deny" {
  version       = "0.1"
  initial_state = "open"
  target_state  = "done"

  agent "bot" { adapter = "permissive" }

  step "open" {
    agent     = "bot"
    lifecycle = "open"
    outcome "success" { transition_to = "run" }
  }
  step "run" {
    agent  = "bot"
    config = { perm_tools = "read_file" }
    outcome "needs_review" { transition_to = "close" }
    outcome "success"      { transition_to = "close" }
  }
  step "close" {
    agent     = "bot"
    lifecycle = "close"
    outcome "success" { transition_to = "done" }
  }
  state "done" { terminal = true }
}`)

	sink := &captureStepEventSink{}
	if err := New(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if sink.sawAdapterKind("permission.granted") {
		t.Error("expected no permission.granted events when allow_tools is empty")
	}
	if !sink.sawAdapterKind("permission.denied") {
		t.Error("expected permission.denied event from default deny policy")
	}
}

func TestEngineShellFingerprintAllowlist(t *testing.T) {
	pluginBin := testutil.BuildPermissivePlugin(t)
	loader := plugin.NewLoaderWithDiscovery(func(string) (string, error) {
		return pluginBin, nil
	})
	t.Cleanup(func() { _ = loader.Shutdown(context.Background()) })

	g := compile(t, `
workflow "perm-shell" {
  version       = "0.1"
  initial_state = "open"
  target_state  = "done"

  agent "bot" { adapter = "permissive" }

  step "open" {
    agent     = "bot"
    lifecycle = "open"
    outcome "success" { transition_to = "run" }
  }
  step "run" {
    agent       = "bot"
    config      = { perm_tools = "shell|git status,shell|rm -rf /" }
    allow_tools = ["shell:git *"]
    outcome "success"      { transition_to = "close" }
    outcome "needs_review" { transition_to = "close" }
  }
  step "close" {
    agent     = "bot"
    lifecycle = "close"
    outcome "success" { transition_to = "done" }
  }
  state "done" { terminal = true }
}`)

	sink := &captureStepEventSink{}
	if err := New(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	granted, ok := sink.firstAdapterEvent("permission.granted")
	if !ok {
		t.Fatal("expected permission.granted for shell|git status")
	}
	if granted["pattern"] != "shell:git *" {
		t.Fatalf("permission.granted pattern=%v want shell:git *", granted["pattern"])
	}
	if !sink.sawAdapterKind("permission.denied") {
		t.Fatal("expected permission.denied for shell|rm -rf /")
	}
}
