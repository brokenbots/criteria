package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/zclconf/go-cty/cty"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow"
)

// lifecycleTrackingSink captures lifecycle events for verification
type lifecycleTrackingSink struct {
	fakeSink

	mu                     sync.Mutex
	adapterLifecycleEvents []string // recorded as "<adapter>:<status>"
}

func (s *lifecycleTrackingSink) OnAdapterLifecycle(runID, adapter, status, detail string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.adapterLifecycleEvents = append(s.adapterLifecycleEvents, adapter+":"+status)
}
func (s *lifecycleTrackingSink) OnAdapterLifecycleEvent(event *AdapterLifecycleEvent) {}

// lifecycleTrackingAdapter tracks session open/close calls
type lifecycleTrackingAdapter struct {
	fakeAdapter
	mu          sync.Mutex
	opensCount  int
	closesCount int
	sessionOpen bool
}

func (p *lifecycleTrackingAdapter) OpenSession(ctx context.Context, sessionID string, config, secrets map[string]string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.opensCount++
	p.sessionOpen = true
	return nil
}

func (p *lifecycleTrackingAdapter) CloseSession(ctx context.Context, sessionID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closesCount++
	p.sessionOpen = false
	return nil
}

// failingInitAdapter tracks session operations but fails on init for a specific session ID
type failingInitAdapter struct {
	fakeAdapter
	mu              sync.Mutex
	opensCount      int
	closesCount     int
	failOnSessionID string // which session to fail on
	shouldFail      bool   // whether to fail
}

func (p *failingInitAdapter) OpenSession(ctx context.Context, sessionID string, config, secrets map[string]string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.opensCount++
	if p.shouldFail && sessionID == p.failOnSessionID {
		return fmt.Errorf("adapter initialization failed")
	}
	return nil
}

func (p *failingInitAdapter) CloseSession(ctx context.Context, sessionID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closesCount++
	return nil
}

// TestEngine_LifecycleEventsEmitted verifies that lifecycle events are emitted when adapters are provisioned/torn down.
func TestEngine_LifecycleEventsEmitted(t *testing.T) {
	g := compile(t, `
workflow {
  name = "test"
  version       = "0.1"
  initial_state = "step1"
  target_state  = "done"
}

step "step1" {
  target = adapter.noop
  outcome "success" { next = step.done }
}

state "done" {
  terminal = true
  success  = true
}`)

	trackingPlugin := &lifecycleTrackingAdapter{
		fakeAdapter: fakeAdapter{name: "noop", outcome: "success"},
	}

	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"noop": trackingPlugin,
	}}

	sink := &lifecycleTrackingSink{}
	eng := New(g, loader, sink)

	// Run should provision adapters before first step
	ctx := context.Background()
	err := eng.Run(ctx)
	if err != nil {
		t.Errorf("Run failed: %v", err)
	}

	if sink.terminal != "done" || !sink.terminalOK {
		t.Errorf("expected success terminal state 'done', got %s ok=%v", sink.terminal, sink.terminalOK)
	}

	// Verify lifecycle events were emitted: opened, then closed
	if len(sink.adapterLifecycleEvents) < 2 {
		t.Errorf("expected at least 2 lifecycle events (opened, closed), got %d: %v", len(sink.adapterLifecycleEvents), sink.adapterLifecycleEvents)
	} else {
		// Should have "noop.default:opened" and "noop.default:closed"
		hasOpened := false
		hasClosed := false
		for _, evt := range sink.adapterLifecycleEvents {
			if evt == "noop.default:opened" {
				hasOpened = true
			} else if evt == "noop.default:closed" {
				hasClosed = true
			}
		}
		if !hasOpened {
			t.Errorf("expected 'opened' lifecycle event for noop.default, got events: %v", sink.adapterLifecycleEvents)
		}
		if !hasClosed {
			t.Errorf("expected 'closed' lifecycle event for noop.default, got events: %v", sink.adapterLifecycleEvents)
		}
	}

	// Verify adapter was opened and closed
	trackingPlugin.mu.Lock()
	opensCount := trackingPlugin.opensCount
	closesCount := trackingPlugin.closesCount
	trackingPlugin.mu.Unlock()

	if opensCount != 1 {
		t.Errorf("expected adapter to be opened once, was opened %d times", opensCount)
	}
	if closesCount != 1 {
		t.Errorf("expected adapter to be closed once, was closed %d times", closesCount)
	}
}

// TestEngine_AdapterTeardownOnCompletion verifies adapters are torn down after workflow completes.
func TestEngine_AdapterTeardownOnCompletion(t *testing.T) {
	g := compile(t, `
workflow {
  name = "test"
  version       = "0.1"
  initial_state = "step1"
  target_state  = "done"
}

step "step1" {
  target = adapter.noop
  outcome "success" { next = step.done }
}

state "done" {
  terminal = true
  success  = true
}`)

	trackingPlugin := &lifecycleTrackingAdapter{
		fakeAdapter: fakeAdapter{name: "noop", outcome: "success"},
	}

	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"noop": trackingPlugin,
	}}

	sink := &lifecycleTrackingSink{}
	eng := New(g, loader, sink)

	err := eng.Run(context.Background())
	if err != nil {
		t.Errorf("Run failed: %v", err)
	}

	// Verify run completed normally
	if !sink.terminalOK {
		t.Error("run did not complete successfully")
	}

	// Verify adapter was closed after completion
	trackingPlugin.mu.Lock()
	closesCount := trackingPlugin.closesCount
	trackingPlugin.mu.Unlock()

	if closesCount != 1 {
		t.Errorf("expected adapter to be closed once on completion, was closed %d times", closesCount)
	}
}

// TestEngine_AdapterTeardownOnError verifies adapters are torn down even if step execution returns an error.
func TestEngine_AdapterTeardownOnError(t *testing.T) {
	g := compile(t, `
workflow {
  name = "test"
  version       = "0.1"
  initial_state = "fail_step"
  target_state  = "done"
}

step "fail_step" {
  target = adapter.noop
  outcome "success" { next = step.done }
}

state "done" {
  terminal = true
  success  = true
}`)

	trackingPlugin := &lifecycleTrackingAdapter{
		fakeAdapter: fakeAdapter{name: "noop", outcome: "success", err: fmt.Errorf("step execution failed")},
	}

	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"noop": trackingPlugin,
	}}

	sink := &lifecycleTrackingSink{}
	eng := New(g, loader, sink)

	// Run - should fail due to step error, but adapters should still be torn down
	ctx := context.Background()
	err := eng.Run(ctx)
	// Error expected because the step returns an error
	if err == nil {
		t.Fatal("Run should have failed due to step error, but got nil")
	}

	// Verify adapter was still closed even though step errored
	trackingPlugin.mu.Lock()
	closesCount := trackingPlugin.closesCount
	trackingPlugin.mu.Unlock()

	if closesCount != 1 {
		t.Errorf("expected adapter to be closed once on error, was closed %d times", closesCount)
	}
}

// TestEngine_MultipleAdaptersProvisioned verifies all declared adapters are provisioned and torn down in order.
func TestEngine_MultipleAdaptersProvisioned(t *testing.T) {
	g := compile(t, `
workflow {
  name = "test"
  version       = "0.1"
  initial_state = "step1"
  target_state  = "done"
}

step "step1" {
  target = adapter.noop_a
  outcome "success" { next = step.step2 }
}

step "step2" {
  target = adapter.noop_b
  outcome "success" { next = step.done }
}

state "done" {
  terminal = true
  success  = true
}`)

	trackingA := &lifecycleTrackingAdapter{
		fakeAdapter: fakeAdapter{name: "noop_a", outcome: "success"},
	}
	trackingB := &lifecycleTrackingAdapter{
		fakeAdapter: fakeAdapter{name: "noop_b", outcome: "success"},
	}

	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"noop_a": trackingA,
		"noop_b": trackingB,
	}}

	sink := &lifecycleTrackingSink{}
	eng := New(g, loader, sink)

	err := eng.Run(context.Background())
	if err != nil {
		t.Errorf("Run failed: %v", err)
	}

	// Verify both steps ran
	if len(sink.stepsRun) != 2 {
		t.Errorf("expected 2 steps to run, got %d", len(sink.stepsRun))
	}

	// Verify both adapters were opened and closed
	trackingA.mu.Lock()
	aOpens := trackingA.opensCount
	aCloses := trackingA.closesCount
	trackingA.mu.Unlock()

	trackingB.mu.Lock()
	bOpens := trackingB.opensCount
	bCloses := trackingB.closesCount
	trackingB.mu.Unlock()

	if aOpens != 1 || aCloses != 1 {
		t.Errorf("adapter A: expected 1 open and 1 close, got %d opens and %d closes", aOpens, aCloses)
	}
	if bOpens != 1 || bCloses != 1 {
		t.Errorf("adapter B: expected 1 open and 1 close, got %d opens and %d closes", bOpens, bCloses)
	}

	// Verify teardown happened in reverse order (LIFO)
	// Expected sequence: noop_a:opened, noop_b:opened, noop_b:closed, noop_a:closed
	// Filter to only opened and closed events
	var lifecycleEvents []string
	for _, evt := range sink.adapterLifecycleEvents {
		if strings.HasSuffix(evt, ":opened") || strings.HasSuffix(evt, ":closed") {
			lifecycleEvents = append(lifecycleEvents, evt)
		}
	}

	expected := []string{
		"noop_a.default:opened",
		"noop_b.default:opened",
		"noop_b.default:closed",
		"noop_a.default:closed",
	}

	if len(lifecycleEvents) < len(expected) {
		t.Errorf("expected at least %d lifecycle events (opened/closed only), got %d: %v",
			len(expected), len(lifecycleEvents), lifecycleEvents)
	} else {
		// Verify the exact sequence
		for i, expectedEvt := range expected {
			if i < len(lifecycleEvents) && lifecycleEvents[i] != expectedEvt {
				t.Errorf("at position %d: expected %q, got %q (filtered sequence: %v)",
					i, expectedEvt, lifecycleEvents[i], lifecycleEvents)
			}
		}
	}
}

// TestEngine_AdapterTeardownOnCancel verifies adapters are torn down even when the run context is cancelled,
// demonstrating that teardown uses context.WithoutCancel to complete cleanup.
func TestEngine_AdapterTeardownOnCancel(t *testing.T) {
	g := compile(t, `
workflow {
  name = "test"
  version       = "0.1"
  initial_state = "step1"
  target_state  = "done"
}

step "step1" {
  target = adapter.noop
  outcome "success" { next = step.done }
}

state "done" {
  terminal = true
  success  = true
}`)

	trackingPlugin := &lifecycleTrackingAdapter{
		fakeAdapter: fakeAdapter{name: "noop", outcome: "success"},
	}

	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"noop": trackingPlugin,
	}}

	sink := &lifecycleTrackingSink{}
	eng := New(g, loader, sink)

	// Create a cancelled context to simulate early cancellation
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	// Run with cancelled context - should fail due to cancellation
	_ = eng.Run(ctx)

	// With verification/binding split, the adapter is only verified (no long-lived
	// session) before the canceled context aborts the run. Teardown still emits the
	// closed lifecycle event, but there is no active session to CloseSession.
	trackingPlugin.mu.Lock()
	closesCount := trackingPlugin.closesCount
	trackingPlugin.mu.Unlock()

	if closesCount != 0 {
		t.Errorf("expected verified-only adapter to be closed 0 times (no session bound), was closed %d times", closesCount)
	}

	var closedEvents int
	sink.mu.Lock()
	for _, evt := range sink.adapterLifecycleEvents {
		if evt == "noop.default:closed" {
			closedEvents++
		}
	}
	sink.mu.Unlock()

	if closedEvents != 1 {
		t.Errorf("expected one closed lifecycle event for noop.default, got %d", closedEvents)
	}
}

// TestEngine_AdapterInitFailureRollsBack verifies that when a second adapter fails to initialize,
// all previously provisioned adapters are rolled back in reverse order.
func TestEngine_AdapterInitFailureRollsBack(t *testing.T) {
	g := compile(t, `
workflow {
  name = "test"
  version       = "0.1"
  initial_state = "step1"
  target_state  = "done"
}

step "step1" {
  target = adapter.noop_a
  outcome "success" { next = step.step2 }
}

step "step2" {
  target = adapter.noop_b
  outcome "success" { next = step.done }
}

state "done" {
  terminal = true
  success  = true
}`)

	trackingA := &lifecycleTrackingAdapter{
		fakeAdapter: fakeAdapter{name: "noop_a", outcome: "success"},
	}

	// noop_b will fail to initialize
	failingPlugin := &failingInitAdapter{
		fakeAdapter:     fakeAdapter{name: "noop_b", outcome: "success"},
		failOnSessionID: "noop_b.default",
		shouldFail:      true,
	}

	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"noop_a": trackingA,
		"noop_b": failingPlugin,
	}}

	sink := &lifecycleTrackingSink{}
	eng := New(g, loader, sink)

	// Run should fail due to adapter B init failure
	err := eng.Run(context.Background())
	if err == nil {
		t.Fatal("Run should have failed due to adapter B init failure, but got nil")
	}

	// Verify adapter A was opened and then closed (rolled back)
	trackingA.mu.Lock()
	aOpens := trackingA.opensCount
	aCloses := trackingA.closesCount
	trackingA.mu.Unlock()

	if aOpens != 1 {
		t.Errorf("adapter A: expected 1 open, got %d", aOpens)
	}
	if aCloses != 1 {
		t.Errorf("adapter A: expected 1 close (rollback), got %d", aCloses)
	}

	// Verify adapter B was attempted to open but never closed
	failingPlugin.mu.Lock()
	bOpens := failingPlugin.opensCount
	bCloses := failingPlugin.closesCount
	failingPlugin.mu.Unlock()

	if bOpens != 1 {
		t.Errorf("adapter B: expected 1 open attempt, got %d", bOpens)
	}
	if bCloses != 0 {
		t.Errorf("adapter B: expected 0 closes (never opened), got %d", bCloses)
	}
}

// configCapturingAdapter records the config map passed to OpenSession.
type configCapturingAdapter struct {
	fakeAdapter
	mu             sync.Mutex
	capturedConfig map[string]string
}

func (p *configCapturingAdapter) OpenSession(_ context.Context, _ string, config, _ map[string]string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.capturedConfig = make(map[string]string, len(config))
	for k, v := range config {
		p.capturedConfig[k] = v
	}
	return nil
}

// TestInitScopeAdapters_RuntimeConfigFromVar verifies that var.* references in
// adapter config blocks resolve to actual runtime values — not the compile-time
// defaults — when initScopeAdapters is called with runtime-overridden vars.
func TestInitScopeAdapters_RuntimeConfigFromVar(t *testing.T) {
	// Compile a workflow where the adapter config references var.model.
	// The variable default is "compile-default"; we override it at runtime to
	// "runtime-model" and assert the adapter's OpenSession receives the latter.
	g := compile(t, `
workflow {
  name          = "test"
  version       = "0.1"
  initial_state = "step1"
  target_state  = "done"
}

variable "model" {
  type    = string
  default = "compile-default"
}

adapter "noop" "default" {
  config {
    model = var.model
  }
}

step "step1" {
  target = adapter.noop.default
  outcome "success" { next = step.done }
}

state "done" {
  terminal = true
  success  = true
}`)

	// Sanity-check: the compile-folded Config should reflect the variable default.
	adapterNode := g.Adapters["noop.default"]
	if adapterNode == nil {
		t.Fatal("adapter 'noop.default' not found in compiled graph")
	}
	if adapterNode.Config["model"] != "compile-default" {
		t.Errorf("compile-time config[model] = %q, want %q", adapterNode.Config["model"], "compile-default")
	}

	capturer := &configCapturingAdapter{
		fakeAdapter: fakeAdapter{name: "noop", outcome: "success"},
	}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"noop": capturer,
	}}

	sink := &fakeSink{}
	// Use WithVarOverrides to supply the runtime value of var.model.
	eng := New(g, loader, sink, WithVarOverrides(map[string]cty.Value{"model": cty.StringVal("runtime-model")}))
	if err := eng.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	capturer.mu.Lock()
	gotConfig := capturer.capturedConfig
	capturer.mu.Unlock()

	if gotConfig == nil {
		t.Fatal("OpenSession was not called on the config-capturing adapter")
	}
	got := gotConfig["model"]
	if got != "runtime-model" {
		t.Errorf("adapter config[model] = %q, want %q (runtime var resolution failed)", got, "runtime-model")
	}
}

// mkdirAdapter is a test helper that creates the directory named in the step's
// "path" input. It simulates a bootstrap step that materializes an adapter's
// working directory before that adapter is bound.
type mkdirAdapter struct {
	fakeAdapter
}

func (m *mkdirAdapter) Execute(_ context.Context, _ string, step *workflow.StepNode, _ adapter.EventSink) (adapter.Result, error) {
	if err := os.MkdirAll(step.Input["path"], 0o750); err != nil {
		return adapter.Result{Outcome: "failure"}, fmt.Errorf("mkdir: %w", err)
	}
	return adapter.Result{Outcome: "success"}, nil
}

// TestEngine_AdapterWorkingDirectoryCreatedByStep verifies that an adapter bound
// to a working directory created by an earlier workflow step runs successfully
// from a clean state. The adapter is verified eagerly at run start (in a neutral
// directory) but not bound until the first step that targets it, by which time
// the directory exists.
func TestEngine_AdapterWorkingDirectoryCreatedByStep(t *testing.T) {
	tmp := t.TempDir()
	worktree := filepath.Join(tmp, "worktree")

	g := compile(t, `
workflow {
  name          = "mkdir-test"
  version       = "0.1"
  initial_state = "mkdir"
  target_state  = "done"
}

variable "dir" {
  type    = string
  default = ""
}

environment "shell" "workdir" {
  working_directory = var.dir
}

adapter "noop" "use" {
  environment = shell.workdir
}

step "mkdir" {
  target = adapter.mkdir
  input {
    path = var.dir
  }
  outcome "success" { next = step.use }
}

step "use" {
  target = adapter.noop.use
  outcome "success" { next = step.done }
}

state "done" {
  terminal = true
  success  = true
}`)

	mkdirHandle := &mkdirAdapter{fakeAdapter: fakeAdapter{name: "mkdir", outcome: "success"}}
	useAdapter := &lifecycleTrackingAdapter{fakeAdapter: fakeAdapter{name: "noop", outcome: "success"}}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"mkdir": mkdirHandle,
		"noop":  useAdapter,
	}}

	sink := &lifecycleTrackingSink{}
	eng := New(g, loader, sink, WithVarOverrides(map[string]cty.Value{
		"dir": cty.StringVal(worktree),
	}))

	if err := eng.Run(context.Background()); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if _, err := os.Stat(worktree); err != nil {
		t.Fatalf("working directory was not created: %v", err)
	}

	useAdapter.mu.Lock()
	opens := useAdapter.opensCount
	closes := useAdapter.closesCount
	useAdapter.mu.Unlock()

	if opens != 1 {
		t.Errorf("adapter opens: want 1, got %d", opens)
	}
	if closes != 1 {
		t.Errorf("adapter closes: want 1, got %d", closes)
	}
}

// failingBindAdapter fails OpenSession, simulating a deferred bind-time failure
// such as a working directory that has not been created before the adapter is
// first used.
type failingBindAdapter struct {
	fakeAdapter
	workingDir string
}

func (f *failingBindAdapter) OpenSession(_ context.Context, _ string, _, _ map[string]string) error {
	if _, err := os.Stat(f.workingDir); err != nil {
		return fmt.Errorf("working directory does not exist: %w", err)
	}
	return nil
}

// TestEngine_DeferredBindFailureIdentifiesAdapterStepAndWorkingDir verifies that
// when a deferred session bind fails at first use, the error identifies the
// adapter instance, the step, and the resolved working directory. This is a
// regression test for the error wrapping in SessionManager.bindVerifiedAndLookup.
func TestEngine_DeferredBindFailureIdentifiesAdapterStepAndWorkingDir(t *testing.T) {
	tmp := t.TempDir()
	worktree := filepath.Join(tmp, "worktree") // intentionally never created

	g := compile(t, `
workflow {
  name          = "bind-failure-test"
  version       = "0.1"
  initial_state = "use"
  target_state  = "done"
}

variable "dir" {
  type    = string
  default = ""
}

environment "shell" "workdir" {
  working_directory = var.dir
}

adapter "noop" "use" {
  environment = shell.workdir
}

step "use" {
  target = adapter.noop.use
  outcome "success" { next = step.done }
}

state "done" {
  terminal = true
  success  = true
}`)

	failing := &failingBindAdapter{fakeAdapter: fakeAdapter{name: "noop"}, workingDir: worktree}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"noop": failing,
	}}

	sink := &lifecycleTrackingSink{}
	eng := New(g, loader, sink, WithVarOverrides(map[string]cty.Value{
		"dir": cty.StringVal(worktree),
	}))

	err := eng.Run(context.Background())
	if err == nil {
		t.Fatal("expected deferred bind failure, got nil")
	}

	errStr := err.Error()
	for _, want := range []string{"noop.use", "use", worktree} {
		if !strings.Contains(errStr, want) {
			t.Errorf("error should identify %q; got:\n%s", want, errStr)
		}
	}
}

// TestEngine_StrictSandboxMissingPrimitivesFailsAtStartup verifies that a
// strict-mode sandbox adapter whose host primitives are unavailable fails the
// run during eager phase-1 verification, before any step executes. This is a
// regression test for the deferral of sandbox enforcement to bind time.
func TestEngine_StrictSandboxMissingPrimitivesFailsAtStartup(t *testing.T) {
	g := compile(t, `
workflow {
  name          = "strict-sandbox-unreachable"
  version       = "0.1"
  initial_state = "done"
  target_state  = "done"
}

environment "sandbox" "strict" {
  policy_mode = "strict"
}

adapter "noop" "x" {
  environment = sandbox.strict
}

state "done" {
  terminal = true
  success  = true
}`)

	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"noop": &fakeAdapter{name: "noop", outcome: "success"},
	}}

	sink := &lifecycleTrackingSink{}
	eng := New(g, loader, sink, WithSandboxProbeOverride(strictMissingSandboxCaps))

	err := eng.Run(context.Background())
	if err == nil {
		t.Fatal("expected startup failure for strict sandbox with missing primitives, got nil")
	}
	if !strings.Contains(err.Error(), "noop.x") {
		t.Errorf("error should name adapter instance, got: %v", err)
	}
	if !strings.Contains(err.Error(), "strict") {
		t.Errorf("error should mention strict sandbox failure, got: %v", err)
	}

	// No step should have been entered: the failure must surface from Verify,
	// not from Execute at first use.
	sink.mu.Lock()
	steps := len(sink.stepsRun)
	sink.mu.Unlock()
	if steps != 0 {
		t.Errorf("expected no steps to run, got %d", steps)
	}
}

// TestEngine_UnreachableBrokenAdapterFailsAtStartup verifies that a broken
// adapter declared in a workflow but never reached by any step still fails the
// run during eager verification, before any step executes. This proves that
// lazy session binding did not weaken the fail-fast startup guarantee.
func TestEngine_UnreachableBrokenAdapterFailsAtStartup(t *testing.T) {
	const initErrMsg = "broken adapter cannot initialize"

	g := compile(t, `
workflow {
  name          = "unreachable-broken"
  version       = "0.1"
  initial_state = "done"
  target_state  = "done"
}

adapter "broken" "x" {
  config {}
}

state "done" {
  terminal = true
  success  = true
}`)

	broken := &failingOpenAdapter{
		fakeAdapter: fakeAdapter{name: "broken"},
		openErr:     errors.New(initErrMsg),
	}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"broken": broken,
	}}

	sink := &lifecycleTrackingSink{}
	eng := New(g, loader, sink)

	err := eng.Run(context.Background())
	if err == nil {
		t.Fatal("expected startup failure for broken unreachable adapter, got nil")
	}
	if !strings.Contains(err.Error(), "broken.x") {
		t.Errorf("error should name adapter instance, got: %v", err)
	}
	if !strings.Contains(err.Error(), initErrMsg) {
		t.Errorf("error should contain adapter failure message, got: %v", err)
	}

	// No step should have been entered.
	sink.mu.Lock()
	steps := len(sink.stepsRun)
	sink.mu.Unlock()
	if steps != 0 {
		t.Errorf("expected no steps to run, got %d", steps)
	}
}

// TestEngine_InvalidWorkingDirectoryRejectedAtStartup verifies that a resolved
// working_directory that is structurally invalid (outside allowed roots) is
// rejected eagerly during adapter verification, not deferred to first use.
func TestEngine_InvalidWorkingDirectoryRejectedAtStartup(t *testing.T) {
	allowed := t.TempDir()
	outside := "/tmp/criteria-outside-root-" + fmt.Sprintf("%d", os.Getpid())

	g := compile(t, `
workflow {
  name          = "invalid-wd"
  version       = "0.1"
  initial_state = "done"
  target_state  = "done"
}

variable "dir" {
  type    = string
  default = ""
}

environment "shell" "env" {
  working_directory = var.dir
}

adapter "noop" "use" {
  environment = shell.env
}

state "done" {
  terminal = true
  success  = true
}`)

	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"noop": &lifecycleTrackingAdapter{fakeAdapter: fakeAdapter{name: "noop", outcome: "success"}},
	}}

	sink := &lifecycleTrackingSink{}
	eng := New(g, loader, sink,
		WithVarOverrides(map[string]cty.Value{"dir": cty.StringVal(outside)}),
		WithWorkingDirAllowedRoots([]string{allowed}),
	)

	err := eng.Run(context.Background())
	if err == nil {
		t.Fatal("expected startup failure for invalid working directory, got nil")
	}
	if !strings.Contains(err.Error(), "outside allowed roots") {
		t.Errorf("error should report working directory outside allowed roots, got: %v", err)
	}

	// No step should have been entered.
	sink.mu.Lock()
	steps := len(sink.stepsRun)
	sink.mu.Unlock()
	if steps != 0 {
		t.Errorf("expected no steps to run, got %d", steps)
	}
}

// TestEngine_WorkingDirectoryWithDotDotRejectedAtStartup verifies that a
// resolved working_directory containing ".." is rejected eagerly at run start,
// even when no allowed-root restriction is configured.
func TestEngine_WorkingDirectoryWithDotDotRejectedAtStartup(t *testing.T) {
	g := compile(t, `
workflow {
  name          = "dotdot-wd"
  version       = "0.1"
  initial_state = "done"
  target_state  = "done"
}

variable "dir" {
  type    = string
  default = ""
}

environment "shell" "env" {
  working_directory = var.dir
}

adapter "noop" "use" {
  environment = shell.env
}

state "done" {
  terminal = true
  success  = true
}`)

	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"noop": &lifecycleTrackingAdapter{fakeAdapter: fakeAdapter{name: "noop", outcome: "success"}},
	}}

	sink := &lifecycleTrackingSink{}
	eng := New(g, loader, sink,
		WithVarOverrides(map[string]cty.Value{"dir": cty.StringVal("/tmp/../etc")}),
	)

	err := eng.Run(context.Background())
	if err == nil {
		t.Fatal("expected startup failure for working directory containing .., got nil")
	}
	if !strings.Contains(err.Error(), `".."`) {
		t.Errorf("error should report working directory contains .., got: %v", err)
	}

	sink.mu.Lock()
	steps := len(sink.stepsRun)
	sink.mu.Unlock()
	if steps != 0 {
		t.Errorf("expected no steps to run, got %d", steps)
	}
}

// schemaAdapter is a fake adapter whose Info response declares a manifest
// config schema. It is used to exercise phase-1 config/secret validation.
type schemaAdapter struct {
	fakeAdapter
	schema map[string]workflow.ConfigField
}

func (p *schemaAdapter) Info(context.Context) (adapterhost.Info, error) {
	return adapterhost.Info{
		Name:    p.name,
		Version: "test",
		AdapterInfo: workflow.AdapterInfo{
			ConfigSchema: p.schema,
		},
	}, nil
}

// schemaTrackingAdapter combines lifecycle open/close tracking with a declared
// manifest config schema.
type schemaTrackingAdapter struct {
	lifecycleTrackingAdapter
	schema map[string]workflow.ConfigField
}

func (p *schemaTrackingAdapter) Info(context.Context) (adapterhost.Info, error) {
	return adapterhost.Info{
		Name:    p.name,
		Version: "test",
		AdapterInfo: workflow.AdapterInfo{
			ConfigSchema: p.schema,
		},
	}, nil
}

// TestEngine_VerifyRejectsMissingRequiredConfigAtStartup verifies that an
// adapter whose manifest declares a required config field fails during eager
// phase-1 verification when the field is omitted, before any step runs. This
// regression locks in the fail-fast guarantee for invalid config.
func TestEngine_VerifyRejectsMissingRequiredConfigAtStartup(t *testing.T) {
	g := compile(t, `
workflow {
  name          = "cfg-test"
  version       = "0.1"
  initial_state = "done"
  target_state  = "done"
}

adapter "configful" "x" {
  config {}
}

state "done" {
  terminal = true
  success  = true
}`)

	adapter := &schemaAdapter{
		fakeAdapter: fakeAdapter{name: "configful"},
		schema: map[string]workflow.ConfigField{
			"required_config": {Required: true, Type: workflow.ConfigFieldString},
		},
	}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"configful": adapter,
	}}

	sink := &lifecycleTrackingSink{}
	eng := New(g, loader, sink)

	err := eng.Run(context.Background())
	if err == nil {
		t.Fatal("expected startup failure for missing required config, got nil")
	}
	if !strings.Contains(err.Error(), "configful.x") {
		t.Errorf("error should name adapter instance, got: %v", err)
	}
	if !strings.Contains(err.Error(), "missing required config field(s): required_config") {
		t.Errorf("error should report missing required config, got: %v", err)
	}

	sink.mu.Lock()
	steps := len(sink.stepsRun)
	sink.mu.Unlock()
	if steps != 0 {
		t.Errorf("expected no steps to run, got %d", steps)
	}
}

// TestEngine_VerifyRejectsMissingRequiredSecretAtStartup verifies that an
// adapter whose manifest declares a required sensitive field fails during eager
// phase-1 verification when the secret is omitted, before any step runs. This
// regression locks in the fail-fast guarantee for missing secrets.
func TestEngine_VerifyRejectsMissingRequiredSecretAtStartup(t *testing.T) {
	g := compile(t, `
workflow {
  name          = "sec-test"
  version       = "0.1"
  initial_state = "done"
  target_state  = "done"
}

adapter "configful" "x" {
  config {}
}

state "done" {
  terminal = true
  success  = true
}`)

	adapter := &schemaAdapter{
		fakeAdapter: fakeAdapter{name: "configful"},
		schema: map[string]workflow.ConfigField{
			"required_secret": {Required: true, Sensitive: true, Type: workflow.ConfigFieldString},
		},
	}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"configful": adapter,
	}}

	sink := &lifecycleTrackingSink{}
	eng := New(g, loader, sink)

	err := eng.Run(context.Background())
	if err == nil {
		t.Fatal("expected startup failure for missing required secret, got nil")
	}
	if !strings.Contains(err.Error(), "configful.x") {
		t.Errorf("error should name adapter instance, got: %v", err)
	}
	if !strings.Contains(err.Error(), "missing required secret(s): required_secret") {
		t.Errorf("error should report missing required secret, got: %v", err)
	}

	sink.mu.Lock()
	steps := len(sink.stepsRun)
	sink.mu.Unlock()
	if steps != 0 {
		t.Errorf("expected no steps to run, got %d", steps)
	}
}

// TestEngine_VerifyAcceptsCompleteConfigAndSecretThenBinds verifies that
// providing all required config values and secrets allows the adapter to pass
// phase-1 verification, bind at first use, and run to completion.
func TestEngine_VerifyAcceptsCompleteConfigAndSecretThenBinds(t *testing.T) {
	t.Setenv("TEST_CFG_SECRET", "shh")

	g := compile(t, `
workflow {
  name          = "cfg-ok"
  version       = "0.1"
  initial_state = "use"
  target_state  = "done"
}

variable "required_secret" {
  type   = string
  secret = true
}

adapter "configful" "x" {
  config {
    required_config = "ok"
  }
  secrets {
    required_secret = var.required_secret
  }
}

step "use" {
  target = adapter.configful.x
  outcome "success" { next = step.done }
}

state "done" {
  terminal = true
  success  = true
}`)

	adapter := &schemaTrackingAdapter{
		lifecycleTrackingAdapter: lifecycleTrackingAdapter{fakeAdapter: fakeAdapter{name: "configful", outcome: "success"}},
		schema: map[string]workflow.ConfigField{
			"required_config": {Required: true, Type: workflow.ConfigFieldString},
			"required_secret": {Required: true, Sensitive: true, Type: workflow.ConfigFieldString},
		},
	}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"configful": adapter,
	}}

	sink := &lifecycleTrackingSink{}
	eng := New(g, loader, sink)
	eng.varOverrides = map[string]cty.Value{
		"required_secret": cty.StringVal("env:TEST_CFG_SECRET"),
	}

	if err := eng.Run(context.Background()); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	adapter.mu.Lock()
	opens := adapter.opensCount
	closes := adapter.closesCount
	adapter.mu.Unlock()

	if opens != 1 {
		t.Errorf("adapter opens: want 1, got %d", opens)
	}
	if closes != 1 {
		t.Errorf("adapter closes: want 1, got %d", closes)
	}

	sink.mu.Lock()
	steps := len(sink.stepsRun)
	sink.mu.Unlock()
	if steps != 1 {
		t.Errorf("expected exactly one step to run, got %d", steps)
	}
}

// fakeRemoteHandle is a minimal adapterhost.Handle for per-scope remote tests.
type fakeRemoteHandle struct{}

func (h *fakeRemoteHandle) Info(context.Context) (adapterhost.Info, error) {
	return adapterhost.Info{Name: "noop", Version: "test"}, nil
}
func (h *fakeRemoteHandle) OpenSession(context.Context, string, map[string]string, map[string]string) error {
	return nil
}
func (h *fakeRemoteHandle) Execute(context.Context, string, *workflow.StepNode, adapter.EventSink) (adapter.Result, error) {
	return adapter.Result{Outcome: "success"}, nil
}
func (h *fakeRemoteHandle) CloseSession(context.Context, string) error { return nil }
func (h *fakeRemoteHandle) Kill()                                      {}
func (h *fakeRemoteHandle) Pause(context.Context, string) error        { return nil }
func (h *fakeRemoteHandle) Resume(context.Context, string) error       { return nil }
func (h *fakeRemoteHandle) Inspect(context.Context, string) (*v2.InspectResponse, error) {
	return nil, nil
}
func (h *fakeRemoteHandle) Snapshot(context.Context, string) (*v2.SnapshotResponse, error) {
	return nil, nil
}
func (h *fakeRemoteHandle) Restore(context.Context, string, []byte, uint32) error { return nil }

// fakeRemoteShim records scope registrations and handle requests for tests.
type fakeRemoteShim struct {
	mu           sync.Mutex
	handle       adapterhost.Handle
	listenAddr   string
	registered   map[string]string
	unregistered []string
	closed       []string
	calls        []string
}

func newFakeRemoteShim(h adapterhost.Handle) *fakeRemoteShim {
	return &fakeRemoteShim{
		handle:     h,
		listenAddr: "127.0.0.1:4242",
		registered: make(map[string]string),
	}
}

func (f *fakeRemoteShim) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fakeRemoteShim) WaitForHandle(_ context.Context, adapterType, scope string) (adapterhost.Handle, error) {
	f.record(fmt.Sprintf("WaitForHandle:%s:%s", adapterType, scope))
	return f.handle, nil
}

func (f *fakeRemoteShim) WaitForFreshHandle(_ context.Context, adapterType, scope string, _ adapterhost.Handle) (adapterhost.Handle, error) {
	f.record(fmt.Sprintf("WaitForFreshHandle:%s:%s", adapterType, scope))
	return f.handle, nil
}

func (f *fakeRemoteShim) RegisterScope(scope, token string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.registered[scope] = token
}

func (f *fakeRemoteShim) UnregisterScope(scope string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unregistered = append(f.unregistered, scope)
}

func (f *fakeRemoteShim) CloseHandle(_ context.Context, adapterType, scope string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = append(f.closed, fmt.Sprintf("%s:%s", adapterType, scope))
	return nil
}

func (f *fakeRemoteShim) ListenAddr() string { return f.listenAddr }

func (f *fakeRemoteShim) Stop(context.Context) error { return nil }

func (f *fakeRemoteShim) registeredToken(scope string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.registered[scope]
}

// eventTrackingSink captures both legacy and structured lifecycle events.
type eventTrackingSink struct {
	fakeSink
	mu                sync.Mutex
	lifecycleStatuses []string
	provisionEvents   []AdapterLifecycleEvent
}

func (s *eventTrackingSink) OnAdapterLifecycle(runID, adapter, status, detail string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lifecycleStatuses = append(s.lifecycleStatuses, fmt.Sprintf("%s:%s:%s", runID, adapter, status))
}

func (s *eventTrackingSink) OnAdapterLifecycleEvent(event *AdapterLifecycleEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.provisionEvents = append(s.provisionEvents, *event)
}

func (s *eventTrackingSink) hasStatus(status string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.provisionEvents {
		if s.provisionEvents[i].Status == status {
			return true
		}
	}
	return false
}

func (s *eventTrackingSink) firstStatus(status string) (AdapterLifecycleEvent, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.provisionEvents {
		if s.provisionEvents[i].Status == status {
			return s.provisionEvents[i], true
		}
	}
	return AdapterLifecycleEvent{}, false
}

// perScopeRemoteGraph returns a compiled workflow with a remote environment that
// has per_scope_sessions enabled and a single noop adapter bound to it.
func perScopeRemoteGraph(t *testing.T) *workflow.FSMGraph {
	t.Helper()
	return compile(t, `
workflow {
  name = "per-scope-test"
  version = "0.1"
  initial_state = "start"
  target_state  = "done"
}

environment "remote" "prod" {
  listen_address     = "127.0.0.1:0"
  per_scope_sessions = true
}

adapter "noop" "default" {
  environment = remote.prod
}

step "start" {
  target = adapter.noop.default
  outcome "success" { next = step.done }
}

state "done" {
  terminal = true
  success  = true
}`)
}

func TestInitScopeAdapters_PerScope_EmitsProvisionWanted(t *testing.T) {
	ctx := context.Background()
	g := perScopeRemoteGraph(t)
	dataDir := t.TempDir()

	sessions := adapterhost.NewSessionManager(&fakeLoader{})
	sessions.SetGraph(g)
	shim := newFakeRemoteShim(&fakeRemoteHandle{})
	sessions.SetRemoteShim(shim)

	lifecycle := newScopeLifecycleState(dataDir)
	lifecycle.setRunID("run-123")
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

	// A provision-wanted event must have been emitted before WaitForHandle returned.
	if !sink.hasStatus("provision_wanted") {
		t.Fatalf("expected provision_wanted event, got %v", sink.provisionEvents)
	}

	event, ok := sink.firstStatus("provision_wanted")
	if !ok {
		t.Fatal("provision_wanted event disappeared")
	}
	if event.RunID != "run-123" {
		t.Errorf("RunID = %q, want run-123", event.RunID)
	}
	if event.ScopeName != "" {
		t.Errorf("ScopeName = %q, want empty root scope", event.ScopeName)
	}
	if event.AdapterName != "default" {
		t.Errorf("AdapterName = %q, want default", event.AdapterName)
	}
	if event.ShimListenAddress != "127.0.0.1:4242" {
		t.Errorf("ShimListenAddress = %q, want 127.0.0.1:4242", event.ShimListenAddress)
	}
	if event.TokenRef == "" {
		t.Fatal("TokenRef must be a non-empty file path")
	}
	if !strings.HasPrefix(event.TokenRef, dataDir) {
		t.Errorf("TokenRef %q must be under dataDir %q", event.TokenRef, dataDir)
	}

	// The token reference must exist and contain the same token registered with the shim.
	tokenBytes, err := os.ReadFile(event.TokenRef)
	if err != nil {
		t.Fatalf("read token file: %v", err)
	}
	token := string(tokenBytes)
	scopeKey := event.ScopeName + "/" + event.ScopeInstanceID
	if got := shim.registeredToken(scopeKey); got != token {
		t.Errorf("shim registered token %q, want %q", got, token)
	}

	// Directory/file permissions must restrict access.
	dir := filepath.Dir(event.TokenRef)
	if info, err := os.Stat(dir); err != nil {
		t.Fatalf("stat token dir: %v", err)
	} else if info.Mode().Perm() != 0o700 {
		t.Errorf("token dir permissions = %o, want 0o700", info.Mode().Perm())
	}
	if info, err := os.Stat(event.TokenRef); err != nil {
		t.Fatalf("stat token file: %v", err)
	} else if info.Mode().Perm() != 0o600 {
		t.Errorf("token file permissions = %o, want 0o600", info.Mode().Perm())
	}

	// The raw token must not appear in any emitted event payload.
	payload := fmt.Sprintf("%+v", sink.provisionEvents)
	if strings.Contains(payload, token) {
		t.Fatalf("event payload contains raw token; payload=%s", payload)
	}

	// WaitForHandle must have been keyed by adapter type + scope.
	found := false
	for _, c := range shim.calls {
		if c == "WaitForHandle:noop:"+scopeKey {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected WaitForHandle call with scope %q, calls=%v", scopeKey, shim.calls)
	}
}

func TestTearDownScopeAdapters_PerScope_EmitsReleased(t *testing.T) {
	ctx := context.Background()
	g := perScopeRemoteGraph(t)
	dataDir := t.TempDir()

	sessions := adapterhost.NewSessionManager(&fakeLoader{})
	sessions.SetGraph(g)
	shim := newFakeRemoteShim(&fakeRemoteHandle{})
	sessions.SetRemoteShim(shim)

	lifecycle := newScopeLifecycleState(dataDir)
	lifecycle.setRunID("run-123")
	sink := &eventTrackingSink{}
	rlc := &remoteLifecycleContext{scopeLifecycle: lifecycle}
	deps := Deps{Sessions: sessions, Sink: sink}

	order, err := initScopeAdapters(ctx, g, deps, nil, dataDir, "", nil, rlc)
	if err != nil {
		t.Fatalf("initScopeAdapters: %v", err)
	}

	tearDownScopeAdapters(ctx, order, deps, rlc)

	if !sink.hasStatus("released") {
		t.Fatalf("expected released event, got %v", sink.provisionEvents)
	}

	released, ok := sink.firstStatus("released")
	if !ok {
		t.Fatal("released event disappeared")
	}
	provision, _ := sink.firstStatus("provision_wanted")
	if released.TokenRef != provision.TokenRef {
		t.Errorf("released.TokenRef %q != provision.TokenRef %q", released.TokenRef, provision.TokenRef)
	}
	if released.ScopeInstanceID != provision.ScopeInstanceID {
		t.Errorf("released.ScopeInstanceID %q != provision.ScopeInstanceID %q", released.ScopeInstanceID, provision.ScopeInstanceID)
	}

	scopeKey := provision.ScopeName + "/" + provision.ScopeInstanceID
	if !containsString(shim.unregistered, scopeKey) {
		t.Errorf("expected UnregisterScope %q, got %v", scopeKey, shim.unregistered)
	}
	wantClose := "noop:" + scopeKey
	if !containsString(shim.closed, wantClose) {
		t.Errorf("expected CloseRemoteHandle %q, got %v", wantClose, shim.closed)
	}
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func TestInitScopeAdapters_PerScopeDisabled_NoLifecycleEvents(t *testing.T) {
	ctx := context.Background()
	g := compile(t, `
workflow {
  name = "legacy-remote-test"
  version = "0.1"
  initial_state = "start"
  target_state  = "done"
}

environment "remote" "prod" {
  listen_address = "127.0.0.1:0"
}

adapter "noop" "default" {
  environment = remote.prod
}

step "start" {
  target = adapter.noop.default
  outcome "success" { next = step.done }
}

state "done" {
  terminal = true
  success  = true
}`)
	dataDir := t.TempDir()

	sessions := adapterhost.NewSessionManager(&fakeLoader{})
	sessions.SetGraph(g)
	shim := newFakeRemoteShim(&fakeRemoteHandle{})
	sessions.SetRemoteShim(shim)

	lifecycle := newScopeLifecycleState(dataDir)
	lifecycle.setRunID("run-legacy")
	sink := &eventTrackingSink{}
	rlc := &remoteLifecycleContext{scopeLifecycle: lifecycle}
	deps := Deps{Sessions: sessions, Sink: sink}

	order, err := initScopeAdapters(ctx, g, deps, nil, dataDir, "", nil, rlc)
	if err != nil {
		t.Fatalf("initScopeAdapters: %v", err)
	}
	if len(order) != 1 {
		t.Fatalf("expected order [noop.default], got %v", order)
	}

	if len(sink.provisionEvents) != 0 {
		t.Errorf("expected no provision events with per_scope_sessions disabled, got %v", sink.provisionEvents)
	}
	if len(shim.registered) != 0 {
		t.Errorf("expected no scope registrations with per_scope_sessions disabled, got %v", shim.registered)
	}

	// WaitForHandle must have been called with an empty scope (legacy keying).
	found := false
	for _, c := range shim.calls {
		if c == "WaitForHandle:noop:" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected legacy WaitForHandle with empty scope, calls=%v", shim.calls)
	}
}
