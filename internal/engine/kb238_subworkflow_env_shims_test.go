package engine

// KB-238: subworkflow-declared remote environments must get their own hosted
// shim. Before the fix, maybeStartRemoteShim only scanned the ROOT graph's
// environment declarations: a subworkflow body that re-declares its own
// remote environments (the workstream_handler_v1 shape) got no shim for
// them, so the body's session waits fell back to the legacy default shim —
// another environment's provider (SetRemoteShimForEnv records the most
// recently registered shim as the default) — whose declared adapter set
// lacks the body's adapters. Every develop card died at adapter init:
//
//	subworkflow "handler": workflow body init adapters: initialize adapter
//	"copilot.coordinator": adapter "copilot" is not declared for this
//	environment; refusing to wait for a child no peer will host
//
// The pairing below:
//   - TestKB238_ShimsHostedForSubworkflowDeclaredEnvironments is the pre-fix
//     reproduce at the wiring level: it parks the run in a session wait (no
//     pods) and requires a shim to be registered for every remote
//     environment declared by ANY graph in the run. Without the fix the
//     body-only environments have no shim (RemoteShimForEnv returns nil).
//   - TestKB238_SubworkflowEnvAdapterRunsOnItsOwnEnvShim is the post-fix
//     green: the live repro shape completes and the body's provision event
//     carries a shim address distinct from the root environment's. Without
//     the fix it fails with the exact live PeerChildSetError chain.
//
// Fail-closed semantics are untouched: only the env keying/composition
// changed. An environment with no statically-declarable adapters keeps an
// empty declared set, which disables the checks (mixed fleet contract).

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	adapterhost "github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow"
	"github.com/brokenbots/criteria/workflow/lockfile"
)

// kb238DevelopRootHCL is the live workstream shape: the root declares its own
// remote env (develop) env-only and uses it for an adapter, while the
// subworkflow re-declares ITS own remote envs (worktree, primary) and binds
// copilot.coordinator to remote.worktree.
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
}
`

// kb238DevelopRootNoUseHCL drops the root's adapter binding for develop so
// develop's declared set stays empty while the body's env stays undeclared
// at the root: the mixed-fleet shape that used to pass through the open
// fallback. Post-fix the body still gets its own shim.
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
}
`

// kb238DevelopRootAlsoWorktreeHCL additionally declares the body's env at
// the root (the pre-fix workaround shape). The root's declaration must win
// the keying: exactly one shim per environment key.
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
}
`

// kb238HandlerHCL mirrors the evidenced workstream_handler_v1 body: it
// re-declares its own remote envs and binds a copilot-style adapter to
// remote.worktree.
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
}
`

// kb238CombinedLockfile pins both adapters that get provisioned in these
// shapes: the root's shell.castle (remote.develop) and the body's
// copilot.coordinator (remote.worktree).
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

// kb238Statuses returns all lifecycle events with the given status.
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

// kb238RunResult carries what a shape run produced.
type kb238RunResult struct {
	err        error
	sink       *eventTrackingSink
	executions *atomic.Int64
}

// kb238RunShape compiles the given root HCL (with the handler body in a
// ./handler subdirectory), runs it to completion within the budget, and
// optionally serves adapter-type-keyed pods (a copilot pod for the body and
// a shell pod for the root) against the provisioned shims.
func kb238RunShape(t *testing.T, rootHCL, variant string, servePods bool, budget time.Duration) *kb238RunResult {
	t.Helper()
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

// kb238ProvisionsFor returns the provision events for an adapter type.
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

// kb238AssertHealthyRun asserts the run completed through the done state
// with no failed step outcomes — in particular none carrying the
// child-set rejection that killed the develop lane.
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

// kb238PodServeType is cri145Pod.serveLoop keyed by adapter type: it waits
// for the first provision_wanted event emitted for wantType and serves that
// event's shim address with a matching handshake, redialing while the host
// cycles verify/bind handles — the same loop a real pod runs.
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
				continue // the token file lands just before the event is emitted
			}
			addr = ev.ShimListenAddress
			hs = &cri137Handshake{
				Name:    wantType,
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

// TestKB238_ShimsHostedForSubworkflowDeclaredEnvironments is the pre-fix
// reproduce at the wiring level. A root pod is served (so root provisioning
// completes and the run reaches runLoop) while the body's copilot pod is
// withheld, parking the run in the body's session wait with the session
// manager live. Every remote environment declared by ANY graph in the run
// must then have its own registered shim. Without the fix the body-only
// environments (worktree, primary) have none — RemoteShimRegisteredForEnv
// reports no registration even though RemoteShimForEnv falls back to the
// develop shim — and the body's adapter init later dies with the
// child-set rejection.
func TestKB238_ShimsHostedForSubworkflowDeclaredEnvironments(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root+"/main.chcl", kb238DevelopRootHCL)
	writeFile(t, root+"/handler/main.chcl", kb238HandlerHCL)
	writeLockfile(t, root, kb238CombinedLockfile())
	g := compileWorkflowDir(t, root)

	sink := &eventTrackingSink{}
	e := NewTestEngine(g, &fakeLoader{adapters: map[string]adapterhost.Handle{
		"noop": &fakeAdapter{name: "noop", outcome: "success"},
	}}, sink,
		WithWorkflowDir(g.WorkflowDir),
		WithDataDir(root),
		WithRunID("kb238-presence"))

	// Serve the root's shell pod only; withhold copilot so the body parks.
	stopPod := make(chan struct{})
	t.Cleanup(func() { close(stopPod) })
	go kb238PodServeType(t, sink, stopPod, "shell", new(atomic.Int64))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	runErr := make(chan error, 1)
	go func() { runErr <- e.Run(ctx) }()

	want := []string{"remote.develop", "remote.worktree", "remote.primary"}
	saw := map[string]bool{}
	deadline := time.Now().Add(8 * time.Second)
	for len(saw) < len(want) {
		select {
		case err := <-runErr:
			t.Fatalf("run returned before all shims observed (saw %v): %v", saw, err)
		case <-time.After(time.Millisecond):
		}
		if time.Now().After(deadline) {
			break
		}
		// Waiting for run-start orders this goroutine's reads after the
		// run goroutine's setup: OnRunStarted fires after setLiveRunState
		// and after shim registration in maybeStartRemoteShim, so the
		// session-manager read below is race-safe.
		if !sink.runStartedSeen() {
			continue
		}
		sessions := e.LiveSessions()
		if sessions == nil {
			// runLoop already completed and cleared the live state: the
			// run died before (or instead of) parking in the body's wait —
			// the pre-fix signature.
			t.Fatalf("run finished before all shims observed: want %v, saw %v — subworkflow-declared remote environments got no shim", want, saw)
		}
		for _, key := range want {
			if !saw[key] && sessions.RemoteShimRegisteredForEnv(key) {
				saw[key] = true
			}
		}
	}
	cancel()
	select {
	case <-runErr:
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return after context cancellation")
	}
	if len(saw) != len(want) {
		t.Fatalf("shims hosted for %v; want all of %v — subworkflow-declared remote environments got no shim", saw, want)
	}
}

// TestKB238_SubworkflowEnvAdapterRunsOnItsOwnEnvShim is the post-fix green
// pairing the live error: the repro shape completes (terminal done, every
// step successful) and the body's copilot provision event publishes a shim
// address DISTINCT from the root environment's — the wait no longer falls
// back to another env's provider. Without the fix this test fails with
//
//	step=engage outcome=failure err=subworkflow "handler": workflow body
//	init adapters: initialize adapter "copilot.coordinator": adapter
//	"copilot" is not declared for this environment; refusing to wait for a
//	child no peer will host
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

// TestKB238_SubworkflowDeclaredEnvWithoutRootUse keeps the mixed-fleet open
// behavior: the root's develop env stays adapter-less (empty declared set)
// while the body-only worktree env still gets hosted and serves the body.
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

// TestKB238_RootAndSubworkflowDeclareSameEnvironment covers the shared-env
// shape (the pre-fix workaround): both graphs declare remote.worktree, one
// shim must serve it, and the body must still run.
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

// TestKB238_ShimEnvironmentDeclarationsComposition pins the collector
// semantics: union across root and subworkflow bodies (nested included),
// root wins on key conflicts, non-remote environments ignored, nil-graph
// and nil-node tolerance.
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
				if got[key] != node { // pointer identity: the same declaration must win
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
