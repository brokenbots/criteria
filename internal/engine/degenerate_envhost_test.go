package engine

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow"
)

// TestMaybeStartDegenerateEnvHosts_BindsShellAndSandboxEnvs pins the M4
// binder: starting the run must register the degenerate in-process host of
// record for every shell/sandbox environment — including declarations that
// live in a compiled subworkflow body — and for nothing else: the
// remote/container kinds own their own binders, undeclared keys stay
// unbound, and the legacy default slot stays empty.
func TestMaybeStartDegenerateEnvHosts_BindsShellAndSandboxEnvs(t *testing.T) {
	g := &workflow.FSMGraph{
		Environments: map[string]*workflow.EnvironmentNode{
			"shell.main-shell":  {Type: "shell", Name: "main-shell"},
			"sandbox.sandboxed": {Type: "sandbox", Name: "sandboxed"},
			"remote.remote-env": {Type: "remote", Name: "remote-env"},
			"container.ctr-env": {Type: "container", Name: "ctr-env"},
		},
		Subworkflows: map[string]*workflow.SubworkflowNode{
			"callee": {
				Name: "callee",
				Body: &workflow.FSMGraph{
					Environments: map[string]*workflow.EnvironmentNode{
						"shell.child-shell": {Type: "shell", Name: "child-shell"},
					},
				},
			},
		},
		SubworkflowOrder: []string{"callee"},
	}
	eng := New(g, nil, &fakeSink{})
	sessions := adapterhost.NewSessionManager(nil)

	if err := eng.maybeStartDegenerateEnvHosts(sessions); err != nil {
		t.Fatalf("maybeStartDegenerateEnvHosts: %v", err)
	}

	for _, key := range []string{"shell.main-shell", "sandbox.sandboxed", "shell.child-shell"} {
		if !sessions.EnvHostRegisteredForEnv(key) {
			t.Errorf("%s: EnvHostRegisteredForEnv = false, want the degenerate in-process binding", key)
		}
	}

	// Bindings are exact: the other isolation kinds own their binders, an
	// undeclared key binds nothing, and no default slot exists.
	for _, key := range []string{"remote.remote-env", "container.ctr-env", "shell.undeclared"} {
		if sessions.EnvHostRegisteredForEnv(key) {
			t.Errorf("%s: EnvHostRegisteredForEnv = true, want false", key)
		}
	}
	if sessions.EnvHostRegisteredForEnv("") {
		t.Error("default slot: EnvHostRegisteredForEnv = true, want false")
	}
}

// TestMaybeStartDegenerateEnvHosts_ShellBindingStaysOffTheShimLanes pins
// the dispatch non-change at the binder level: after a run's binder
// installs the degenerate host for a shell environment, that adapter's
// dispatch still resolves NOTHING through the seam's shim lanes (both are
// remote-only), so its sessions keep the local loader path — the
// degenerate host's arms are never exercised by dispatch. The
// remote/container resolution gates themselves are pinned in the
// adapterhost contract tests (TestDegenerateHost_DoesNotChangeShellDispatch).
func TestMaybeStartDegenerateEnvHosts_ShellBindingStaysOffTheShimLanes(t *testing.T) {
	g := &workflow.FSMGraph{
		Environments: map[string]*workflow.EnvironmentNode{
			"shell.demo": {Type: "shell", Name: "demo"},
		},
		Adapters: map[string]*workflow.AdapterNode{
			"noop.demo": {Type: "noop", Name: "demo", Environment: "shell.demo"},
		},
	}
	eng := New(g, nil, &fakeSink{})
	sessions := adapterhost.NewSessionManager(nil)
	if err := eng.maybeStartDegenerateEnvHosts(sessions); err != nil {
		t.Fatalf("maybeStartDegenerateEnvHosts: %v", err)
	}
	if !sessions.EnvHostRegisteredForEnv("shell.demo") {
		t.Fatal("shell.demo: EnvHostRegisteredForEnv = false, want the degenerate in-process binding")
	}
	// The shell adapter still routes through the local loader: the host
	// resolution gates are remote-only and container-only, so its
	// environment's binding cannot appear on any dispatch lane.
	if sessions.RemoteShimRegisteredForEnv("shell.demo") {
		t.Error("RemoteShimRegisteredForEnv(shell.demo) = true, want false")
	}
	if sessions.RemoteShimForEnv("shell.demo") != nil {
		t.Error("RemoteShimForEnv(shell.demo) = non-nil, want nil")
	}
}

// TestRun_ShellEnvironmentBindingKeepsLocalDispatch asserts the wiring on
// laneSink records adapter-lifecycle observations so the run-path test can
// pin the provisioning lane a shell-bound adapter takes (the local loader's
// verified/opened/closed sequence) and prove no init failure or crash fired.
type laneSink struct {
	*fakeSink

	mu        sync.Mutex
	lifecycle []laneEvent
}

type laneEvent struct {
	scope, name, status, msg string
}

func (s *laneSink) OnAdapterLifecycle(scope, name, status, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lifecycle = append(s.lifecycle, laneEvent{scope, name, status, msg})
}

func (s *laneSink) laneFor(name string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var statuses []string
	for _, ev := range s.lifecycle {
		if ev.name == name {
			statuses = append(statuses, ev.status)
		}
	}
	return statuses
}

func (s *laneSink) allLanes() []laneEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]laneEvent(nil), s.lifecycle...)
}

// TestRun_ShellEnvironmentBindingKeepsLocalDispatch asserts the wiring on
// the real run path: Run installs the degenerate in-process host for the
// shell environment, and the shell-bound adapter stage still provisions
// (verified/opened) and executes through the local loader to a success
// terminal — the degenerate host's refusing arms are never exercised by
// dispatch.
func TestRun_ShellEnvironmentBindingKeepsLocalDispatch(t *testing.T) {
	g := &workflow.FSMGraph{
		Name:         "shell-dispatch",
		InitialState: "run",
		TargetState:  "done",
		Policy:       workflow.DefaultPolicy,
		Environments: map[string]*workflow.EnvironmentNode{
			"shell.demo": {Type: "shell", Name: "demo"},
		},
		Adapters: map[string]*workflow.AdapterNode{
			"noop.demo": {Type: "noop", Name: "demo", Environment: "shell.demo"},
		},
		AdapterOrder: []string{"noop.demo"},
		Steps: map[string]*workflow.StepNode{
			"run": {
				Name:       "run",
				TargetKind: workflow.StepTargetAdapter,
				AdapterRef: "noop.demo",
				Outcomes:   map[string]*workflow.CompiledOutcome{"success": {Next: "done"}},
			},
		},
		States: map[string]*workflow.StateNode{
			"done": {Name: "done", Terminal: true, Success: true},
		},
	}
	sink := &laneSink{fakeSink: &fakeSink{}}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"noop": &fakeAdapter{name: "noop", outcome: "success"},
	}}
	if err := NewTestEngine(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if terminal, ok := sink.terminalState(); terminal != "done" || !ok {
		t.Errorf("terminal = %s ok=%v, want the run to succeed through the local loader", terminal, ok)
	}
	if got := sink.laneFor("noop.demo"); !slices.Equal(got, []string{"verified", "opened", "closed"}) {
		t.Errorf("noop.demo lifecycle = %v, want the local-loader provisioning lane verified/opened/closed", got)
	}
	for _, ev := range sink.allLanes() {
		if ev.status == "init_failed" || ev.status == "crashed" {
			t.Errorf("lifecycle event %+v recorded; want no init failure or crash from the degenerate binding", ev)
		}
	}
}

// TestRunFrom_ShellEnvironmentBindingKeepsLocalDispatch pins the second
// binder call site: the resume entrypoint binds the same degenerate host
// set and the shell-bound adapter still re-provisions and executes through
// the local loader to a success terminal.
func TestRunFrom_ShellEnvironmentBindingKeepsLocalDispatch(t *testing.T) {
	g := &workflow.FSMGraph{
		Name:         "shell-resume",
		InitialState: "run",
		TargetState:  "done",
		Policy:       workflow.DefaultPolicy,
		Environments: map[string]*workflow.EnvironmentNode{
			"shell.demo": {Type: "shell", Name: "demo"},
		},
		Adapters: map[string]*workflow.AdapterNode{
			"noop.demo": {Type: "noop", Name: "demo", Environment: "shell.demo"},
		},
		AdapterOrder: []string{"noop.demo"},
		Steps: map[string]*workflow.StepNode{
			"run": {
				Name:       "run",
				TargetKind: workflow.StepTargetAdapter,
				AdapterRef: "noop.demo",
				Outcomes:   map[string]*workflow.CompiledOutcome{"success": {Next: "done"}},
			},
		},
		States: map[string]*workflow.StateNode{
			"done": {Name: "done", Terminal: true, Success: true},
		},
	}
	sink := &laneSink{fakeSink: &fakeSink{}}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"noop": &fakeAdapter{name: "noop", outcome: "success"},
	}}
	if err := NewTestEngine(g, loader, sink).RunFrom(context.Background(), "run", 1); err != nil {
		t.Fatalf("run from: %v", err)
	}
	if terminal, ok := sink.terminalState(); terminal != "done" || !ok {
		t.Errorf("terminal = %s ok=%v, want the resume to succeed through the local loader", terminal, ok)
	}
	if got := sink.laneFor("noop.demo"); !slices.Equal(got, []string{"verified", "opened", "closed"}) {
		t.Errorf("noop.demo lifecycle = %v, want the local-loader provisioning lane verified/opened/closed", got)
	}
}
