package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/plugin"
	"github.com/brokenbots/criteria/internal/testutil"
	"github.com/brokenbots/criteria/workflow"
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
func (s *fakeSink) OnStepResumed(string, int, string)              {}
func (s *fakeSink) OnVariableSet(string, string, string)           {}
func (s *fakeSink) OnStepOutputCaptured(string, map[string]string) {}
func (s *fakeSink) OnRunPaused(string, string, string)             {}
func (s *fakeSink) OnWaitEntered(string, string, string, string)   {}
func (s *fakeSink) OnWaitResumed(string, string, string, map[string]string) {
}
func (s *fakeSink) OnApprovalRequested(string, []string, string) {}
func (s *fakeSink) OnApprovalDecision(string, string, string, map[string]string) {
}
func (s *fakeSink) OnBranchEvaluated(string, string, string, string)  {}
func (s *fakeSink) OnForEachEntered(string, int)                      {}
func (s *fakeSink) OnStepIterationStarted(string, int, string, bool)  {}
func (s *fakeSink) OnStepIterationCompleted(string, string, string)   {}
func (s *fakeSink) OnStepIterationItem(string, int, string)           {}
func (s *fakeSink) OnScopeIterCursorSet(string)                       {}
func (s *fakeSink) OnAdapterLifecycle(string, string, string, string) {}
func (s *fakeSink) StepEventSink(step string) adapter.EventSink       { return noopSink{} }

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
	g, diags := workflow.Compile(spec, nil)
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
	t.Cleanup(func() { _ = loader.Shutdown(context.Background()) })

	g := compileFile(t, "testdata/agent_lifecycle_noop.hcl")
	sink := &fakeSink{}
	if err := New(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if sink.terminal != "done" || !sink.terminalOK {
		t.Fatalf("terminal state: %s (ok=%v)", sink.terminal, sink.terminalOK)
	}
}

// TestNamedAgentLifecycleEventsOnExecutionStep is a regression test verifying
// that OnAdapterLifecycle events for a named-agent workflow are emitted on the
// execution step ("run_agent"), not on the open/close lifecycle steps (W12).
func TestNamedAgentLifecycleEventsOnExecutionStep(t *testing.T) {
	pluginBin := buildNoopPlugin(t)
	loader := plugin.NewLoaderWithDiscovery(func(string) (string, error) {
		return pluginBin, nil
	})
	t.Cleanup(func() { _ = loader.Shutdown(context.Background()) })

	g := compileFile(t, "testdata/agent_lifecycle_noop.hcl")
	sink := &lifecycleCaptureSink{}
	if err := New(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	sink.mu.Lock()
	defer sink.mu.Unlock()

	// The execution step must carry both "started" and "exited" events.
	runEvents := sink.lifecycle["run_agent"]
	if len(runEvents) < 2 {
		t.Fatalf("run_agent: want ≥2 lifecycle events, got %v", runEvents)
	}
	if runEvents[0] != "started" {
		t.Errorf("run_agent: first event want %q got %q", "started", runEvents[0])
	}
	if runEvents[len(runEvents)-1] != "exited" {
		t.Errorf("run_agent: last event want %q got %q", "exited", runEvents[len(runEvents)-1])
	}

	// Open and close lifecycle steps must carry no lifecycle events.
	if len(sink.lifecycle["open_agent"]) != 0 {
		t.Errorf("open_agent: expected no lifecycle events, got %v", sink.lifecycle["open_agent"])
	}
	if len(sink.lifecycle["close_agent"]) != 0 {
		t.Errorf("close_agent: expected no lifecycle events, got %v", sink.lifecycle["close_agent"])
	}
}

// lifecycleCaptureSink extends fakeSink to record per-step lifecycle events.
type lifecycleCaptureSink struct {
	fakeSink

	mu        sync.Mutex
	lifecycle map[string][]string // step → []status
}

func (s *lifecycleCaptureSink) OnAdapterLifecycle(step, _ /*adapter*/, status, _ /*detail*/ string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lifecycle == nil {
		s.lifecycle = make(map[string][]string)
	}
	s.lifecycle[step] = append(s.lifecycle[step], status)
}

func TestEngineLifecycleOpenTimeoutKeepsSessionAlive(t *testing.T) {
	pluginBin := buildNoopPlugin(t)
	loader := plugin.NewLoaderWithDiscovery(func(string) (string, error) {
		return pluginBin, nil
	})
	t.Cleanup(func() { _ = loader.Shutdown(context.Background()) })

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
	g, diags := workflow.Compile(spec, nil)
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
	pluginBin := filepath.Join(t.TempDir(), "criteria-adapter-noop")

	cmd := exec.Command("go", "build", "-o", pluginBin, "./cmd/criteria-adapter-noop")
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
    input { perm_tools = "read_file,write_file" }
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
    input { perm_tools = "read_file" }
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
    input { perm_tools = "shell|git status,shell|rm -rf /" }
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

// TestMaxVisits_Hit verifies that a workflow with a back-edge loop on a step
// with max_visits = 3 fails on the 4th visit with the expected error message.
func TestMaxVisits_Hit(t *testing.T) {
	g := compile(t, `
workflow "t" {
  version = "0.1"
  initial_state = "loop"
  target_state  = "done"
  step "loop" {
    adapter    = "fake"
    max_visits = 3
    outcome "again" { transition_to = "loop" }
    outcome "done"  { transition_to = "done" }
  }
  state "done" { terminal = true }
  policy { max_total_steps = 1000 }
}`)
	sink := &fakeSink{}
	loader := &fakeLoader{plugins: map[string]plugin.Plugin{"fake": &fakePlugin{name: "fake", outcome: "again"}}}
	err := New(g, loader, sink).Run(context.Background())
	if err == nil {
		t.Fatal("expected max_visits error")
	}
	if !strings.Contains(err.Error(), `exceeded max_visits`) {
		t.Errorf("unexpected error: %v", err)
	}
	if !strings.Contains(err.Error(), `"loop"`) {
		t.Errorf("expected step name in error: %v", err)
	}
	// The step should have been entered exactly 3 times.
	count := 0
	for _, s := range sink.stepsRun {
		if s == "loop" {
			count++
		}
	}
	if count != 3 {
		t.Errorf("expected 3 visits, got %d", count)
	}
}

// TestMaxVisits_NotHit verifies that a workflow with a back-edge loop and
// max_visits = 100 completes normally when the loop exits before the limit.
func TestMaxVisits_NotHit(t *testing.T) {
	// The plugin returns "again" for the first 4 calls, then "done".
	callCount := 0
	var mu sync.Mutex
	plg := &callCountPlugin{
		name: "fake",
		outcomeFor: func(n int) string {
			if n < 5 {
				return "again"
			}
			return "done"
		},
		count: &callCount,
		mu:    &mu,
	}
	g := compile(t, `
workflow "t" {
  version = "0.1"
  initial_state = "loop"
  target_state  = "done"
  step "loop" {
    adapter    = "fake"
    max_visits = 100
    outcome "again" { transition_to = "loop" }
    outcome "done"  { transition_to = "done" }
  }
  state "done" { terminal = true }
  policy { max_total_steps = 1000 }
}`)
	sink := &fakeSink{}
	loader := &fakeLoader{plugins: map[string]plugin.Plugin{"fake": plg}}
	if err := New(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sink.terminal != "done" || !sink.terminalOK {
		t.Errorf("terminal=%q ok=%v want done/true", sink.terminal, sink.terminalOK)
	}
}

// TestMaxVisits_OmittedIsUnlimited verifies that a step without max_visits
// does not trip any limit and completes normally.
func TestMaxVisits_OmittedIsUnlimited(t *testing.T) {
	g := compile(t, `
workflow "t" {
  version = "0.1"
  initial_state = "a"
  target_state  = "done"
  step "a" {
    adapter = "fake"
    outcome "success" { transition_to = "done" }
  }
  state "done" { terminal = true }
}`)
	step := g.Steps["a"]
	if step.MaxVisits != 0 {
		t.Errorf("MaxVisits = %d, want 0 (unlimited)", step.MaxVisits)
	}
	sink := &fakeSink{}
	loader := &fakeLoader{plugins: map[string]plugin.Plugin{"fake": &fakePlugin{name: "fake", outcome: "success"}}}
	if err := New(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
}

// TestMaxVisits_RetryCounts verifies that each retry attempt consumes a
// separate visit. With max_visits=2 and max_step_retries=3 (4 total attempts
// allowed), the step gets attempt 1 (visit 1), attempt 2 (visit 2), then
// attempt 3 is blocked by max_visits before it can execute.
func TestMaxVisits_RetryCounts(t *testing.T) {
	g := compile(t, `
workflow "t" {
  version = "0.1"
  initial_state = "work"
  target_state  = "done"
  step "work" {
    adapter    = "fake"
    max_visits = 2
    outcome "done" { transition_to = "done" }
  }
  state "done" { terminal = true }
  policy {
    max_total_steps  = 1000
    max_step_retries = 3
  }
}`)
	// Plugin that always errors so the retry loop is exercised.
	loader := &fakeLoader{plugins: map[string]plugin.Plugin{"fake": &errPlugin{name: "fake", err: errors.New("boom")}}}
	sink := &fakeSink{}
	err := New(g, loader, sink).Run(context.Background())
	if err == nil {
		t.Fatal("expected max_visits error")
	}
	if !strings.Contains(err.Error(), "exceeded max_visits (2)") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestMaxVisits_Persists verifies that visit counts survive a simulated
// crash-resume cycle: run an engine until max_total_steps fails, capture the
// visit state, create a new engine seeded with those counts, and confirm the
// max_visits limit still fires at the correct iteration.
func TestMaxVisits_Persists(t *testing.T) {
	// With max_total_steps = 2 and a back-edge loop:
	//   Entry 1: TotalSteps++ → 1 (≤2, OK) → runStepFromAttempt: Visits++ → 1, step executes
	//   Entry 2: TotalSteps++ → 2 (≤2, OK) → runStepFromAttempt: Visits++ → 2, step executes
	//   Entry 3: TotalSteps++ → 3 (>2, FAIL) → max_total_steps fires; Visits stays at 2
	// After first run: visits = {loop: 2}.
	//
	// Resumed with visits = {loop: 2}, max_visits = 5:
	//   Entry 3: Visits++ → 3 (<5), step executes
	//   Entry 4: Visits++ → 4 (<5), step executes
	//   Entry 5: Visits++ → 5 (<5), step executes
	//   Entry 6: 5 >= 5 → max_visits fires
	g := compile(t, `
workflow "t" {
  version = "0.1"
  initial_state = "loop"
  target_state  = "done"
  step "loop" {
    adapter    = "fake"
    max_visits = 5
    outcome "again" { transition_to = "loop" }
    outcome "done"  { transition_to = "done" }
  }
  state "done" { terminal = true }
  policy { max_total_steps = 2 }
}`)
	sink := &fakeSink{}
	loader := &fakeLoader{plugins: map[string]plugin.Plugin{"fake": &fakePlugin{name: "fake", outcome: "again"}}}
	eng := New(g, loader, sink)
	err := eng.Run(context.Background())
	if err == nil {
		t.Fatal("expected max_total_steps error from first run")
	}
	if !strings.Contains(err.Error(), "max_total_steps") {
		t.Fatalf("expected max_total_steps error, got: %v", err)
	}

	// The TotalSteps check fires before runStepFromAttempt on the third entry,
	// so visits["loop"] = 2 (only two successful visits were incremented).
	visits := eng.VisitCounts()
	if visits["loop"] != 2 {
		t.Fatalf("want 2 visits after first run, got %d", visits["loop"])
	}

	// Resume with saved visit counts; raise max_total_steps so it doesn't
	// interfere, and expect max_visits to fire after 3 more executions.
	g2 := compile(t, `
workflow "t" {
  version = "0.1"
  initial_state = "loop"
  target_state  = "done"
  step "loop" {
    adapter    = "fake"
    max_visits = 5
    outcome "again" { transition_to = "loop" }
    outcome "done"  { transition_to = "done" }
  }
  state "done" { terminal = true }
  policy { max_total_steps = 1000 }
}`)
	sink2 := &fakeSink{}
	eng2 := New(g2, loader, sink2, WithResumedVisits(visits))
	err2 := eng2.RunFrom(context.Background(), "loop", 1)
	if err2 == nil {
		t.Fatal("expected max_visits error from resumed run")
	}
	if !strings.Contains(err2.Error(), "exceeded max_visits (5)") {
		t.Errorf("unexpected error: %v", err2)
	}
	// With 2 visits already recorded, 3 more fully execute (visits 3, 4, 5)
	// before the 6th triggers max_visits.
	additionalVisits := 0
	for _, s := range sink2.stepsRun {
		if s == "loop" {
			additionalVisits++
		}
	}
	if additionalVisits != 3 {
		t.Errorf("want 3 additional visits before limit, got %d", additionalVisits)
	}
}

// callCountPlugin is a fakePlugin variant that tracks call count and uses a
// caller-supplied function to determine the outcome per call.
type callCountPlugin struct {
	name       string
	outcomeFor func(int) string
	count      *int
	mu         *sync.Mutex
}

func (p *callCountPlugin) Info(context.Context) (plugin.Info, error) {
	return plugin.Info{Name: p.name, Version: "test"}, nil
}
func (p *callCountPlugin) OpenSession(context.Context, string, map[string]string) error {
	return nil
}
func (p *callCountPlugin) Execute(_ context.Context, _ string, _ *workflow.StepNode, _ adapter.EventSink) (adapter.Result, error) {
	p.mu.Lock()
	*p.count++
	n := *p.count
	p.mu.Unlock()
	return adapter.Result{Outcome: p.outcomeFor(n)}, nil
}
func (p *callCountPlugin) Permit(context.Context, string, string, bool, string) error { return nil }
func (p *callCountPlugin) CloseSession(context.Context, string) error                 { return nil }
func (p *callCountPlugin) Kill()                                                      {}

// errPlugin is a plugin that always returns an error, used to exercise the
// retry loop in runStepFromAttempt for max_visits retry-counting tests.
type errPlugin struct {
	name string
	err  error
}

func (p *errPlugin) Info(context.Context) (plugin.Info, error) {
	return plugin.Info{Name: p.name, Version: "test"}, nil
}
func (p *errPlugin) OpenSession(context.Context, string, map[string]string) error { return nil }
func (p *errPlugin) Execute(_ context.Context, _ string, _ *workflow.StepNode, _ adapter.EventSink) (adapter.Result, error) {
	return adapter.Result{}, p.err
}
func (p *errPlugin) Permit(context.Context, string, string, bool, string) error { return nil }
func (p *errPlugin) CloseSession(context.Context, string) error                 { return nil }
func (p *errPlugin) Kill()                                                      {}

// TestMaxVisits_CancelledAttemptDoesNotConsumeVisit verifies that a pre-cancelled
// context returns a cancellation error WITHOUT incrementing the visit count in
// runStepFromAttempt. This is a regression test for the ctx.Err() ordering in
// that path: if incrementVisit were called before ctx.Err(), cancellation would
// inflate the visit count and could incorrectly trip max_visits on resume.
func TestMaxVisits_CancelledAttemptDoesNotConsumeVisit(t *testing.T) {
	g := compile(t, `
workflow "t" {
  version = "0.1"
  initial_state = "work"
  target_state  = "done"
  step "work" {
    adapter    = "fake"
    max_visits = 1
    outcome "done" { transition_to = "done" }
  }
  state "done" { terminal = true }
  policy { max_total_steps = 1000 }
}`)
	loader := &fakeLoader{plugins: map[string]plugin.Plugin{"fake": &fakePlugin{name: "fake", outcome: "done"}}}
	sink := &fakeSink{}
	eng := New(g, loader, sink)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel before Run

	err := eng.Run(ctx)
	if err == nil {
		t.Fatal("expected error from cancelled context; got nil")
	}
	if strings.Contains(err.Error(), "exceeded max_visits") {
		t.Errorf("cancellation must not trip max_visits; got: %v", err)
	}

	visits := eng.VisitCounts()
	if got := visits["work"]; got != 0 {
		t.Errorf("visit count for cancelled attempt = %d, want 0 (cancellation must not consume a visit)", got)
	}
}

// TestMaxVisits_CancelledWorkflowIterationDoesNotConsumeVisit verifies that a
// pre-cancelled context returns a cancellation error WITHOUT incrementing the
// visit count in runWorkflowIteration. This is a regression test for the
// ctx.Err() ordering in that path: if incrementVisit were called before
// ctx.Err(), cancellation would inflate iteration visit counts.
func TestMaxVisits_CancelledWorkflowIterationDoesNotConsumeVisit(t *testing.T) {
	g := compile(t, `
workflow "t" {
  version = "0.1"
  initial_state = "process"
  target_state  = "done"
  step "process" {
    type       = "workflow"
    for_each   = ["a"]
    max_visits = 1
    workflow {
      step "inner" {
        adapter = "fake"
        outcome "success" { transition_to = "_continue" }
      }
    }
    outcome "all_succeeded" { transition_to = "done" }
    outcome "any_failed"    { transition_to = "done" }
  }
  state "done" { terminal = true }
  policy { max_total_steps = 1000 }
}`)
	loader := &fakeLoader{plugins: map[string]plugin.Plugin{"fake": &fakePlugin{name: "fake", outcome: "success"}}}
	sink := &fakeSink{}
	eng := New(g, loader, sink)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel before Run

	err := eng.Run(ctx)
	if err == nil {
		t.Fatal("expected error from cancelled context; got nil")
	}
	if strings.Contains(err.Error(), "exceeded max_visits") {
		t.Errorf("cancellation must not trip max_visits; got: %v", err)
	}

	visits := eng.VisitCounts()
	if got := visits["process"]; got != 0 {
		t.Errorf("visit count for cancelled workflow iteration = %d, want 0 (cancellation must not consume a visit)", got)
	}
}
