package engine

// Temporary probe (not part of the deliverable): full-run shapes for KB-238.

import (
	"context"
	"net/http"
	_ "net/http/pprof" // keep tooling quiet
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	adapterhost "github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow/lockfile"
)

func kb238CopilotLockfile() *lockfile.Lockfile {
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
		},
	}
}

func kb238RootEnvOnly() string {
	return `workflow {
  name          = "kb238-probe"
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
}

// Variant B: the root ALSO declares the body's remote envs, env-only.
func kb238RootEnvOnlyPlusWorktree() string {
	return `workflow {
  name          = "kb238-probe-b"
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
}

// kb238HandlerHCL mirrors the evidenced workstream_handler_v1 shape: the body
// re-declares its OWN remote envs (worktree + primary, per_scope) and binds a
// copilot-style adapter to remote.worktree.
func kb238HandlerHCL() string {
	return `workflow {
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
}

// Variant C (expected live repro): the root uses remote.develop for its own
// work (an adapter declaration is enough to make the develop provider's
// declared set non-empty), while the body binds copilot.coordinator to a
// remote env the root never declares (remote.worktree). The body's session
// wait then falls back to the most-recently-registered provider (develop's)
// via the legacy-default shim recorded by SetRemoteShimForEnv, whose declared
// set lacks copilot → wait-time PeerChildSetError.
func kb238RootDevelopUsed() string {
	return `workflow {
  name          = "kb238-probe-c"
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
}

// kb238CombinedLockfile pins both adapters that get provisioned in the
// variant-C shape: the root's shell.castle (remote.develop) and the body's
// copilot.coordinator (remote.worktree).
func kb238CombinedLockfile() *lockfile.Lockfile {
	return &lockfile.Lockfile{
		SchemaVersion: 1,
		Adapters: append(kb238CopilotLockfile().Adapters, lockfile.LockedAdapter{
			Type:               "shell",
			Name:               "castle",
			Reference:          "ghcr.io/brokenbots/criteria-adapter-shell",
			ResolvedDigest:     pinDigest,
			SourceURL:          "https://github.com/brokenbots/criteria",
			SDKProtocolVersion: 2,
			Platforms:          []string{"linux/amd64"},
		}),
	}
}

// kb238PodServeType is cri145Pod.serveLoop keyed by adapter type: it waits
// for the first provision_wanted event emitted for wantType and serves that
// event's shim address with a matching handshake.
func kb238PodServeType(t *testing.T, sink *eventTrackingSink, stop <-chan struct{}, wantType string, executions *atomic.Int64) {
	t.Helper()
	var hs *cri137Handshake
	var addr string
	for {
		select {
		case <-stop:
			return
		case <-time.After(10 * time.Millisecond):
		}
		for _, ev := range kb238Statuses(sink, "provision_wanted") {
			if ev.AdapterType != wantType {
				continue
			}
			token, err := os.ReadFile(ev.TokenRef)
			if err != nil {
				continue
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

// kb238Statuses returns all lifecycle events with the given status.
func kb238Statuses(sink *eventTrackingSink, status string) []AdapterLifecycleEvent {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	var out []AdapterLifecycleEvent
	for i := range sink.provisionEvents {
		if sink.provisionEvents[i].Status == status {
			out = append(out, sink.provisionEvents[i])
		}
	}
	return out
}

// kb238StepErrs is unused placeholder removed in favor of sink capture.

func runKb238Shape(t *testing.T, rootHCL string, variant string) {
	t.Helper()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "main.chcl"), rootHCL)
	writeFile(t, filepath.Join(root, "handler", "main.chcl"), kb238HandlerHCL())
	writeLockfile(t, root, kb238CombinedLockfile())
	g := compileWorkflowDir(t, root)

	sink := &eventTrackingSink{}
	e := NewTestEngine(g, &fakeLoader{adapters: map[string]adapterhost.Handle{
		"noop": &fakeAdapter{name: "noop", outcome: "success"},
	}}, sink,
		WithWorkflowDir(g.WorkflowDir),
		WithDataDir(root),
		WithRunID("kb238-"+variant))

	executions := new(atomic.Int64)
	stopPod := make(chan struct{})
	t.Cleanup(func() { close(stopPod) })
	go kb238PodServeType(t, sink, stopPod, "copilot", executions)
	go kb238PodServeType(t, sink, stopPod, "shell", executions)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(func() {
		cancel()
	})

	runErr := make(chan error, 1)
	go func() { runErr <- e.Run(ctx) }()
	select {
	case err := <-runErr:
		t.Logf("[%s] run returned err=%v terminal=%q ok=%v failure=%q", variant, err, sink.terminal, sink.terminalOK, sink.failure)
	case <-time.After(60 * time.Second):
		t.Logf("[%s] run did not finish; terminal=%q", variant, sink.terminal)
	}
	provisions, _ := cri145LifecycleStats(sink)
	for _, p := range provisions {
		t.Logf("[%s] provision: scope=%q type=%s env=%s.%s shim=%q", variant, p.ScopeName, p.AdapterType, p.EnvironmentType, p.EnvironmentName, p.ShimListenAddress)
	}
	for _, so := range sink.stepOutcomes() {
		t.Logf("[%s] %s", variant, so)
	}
}

func TestProbe_KB238_VariantA_RootDevelopOnly(t *testing.T) {
	if os.Getenv("KB238_PROBE") == "" {
		t.Skip("probe; set KB238_PROBE=1")
	}
	runKb238Shape(t, kb238RootEnvOnly(), "A")
}

func TestProbe_KB238_VariantB_RootDeclaresWorktree(t *testing.T) {
	if os.Getenv("KB238_PROBE") == "" {
		t.Skip("probe; set KB238_PROBE=1")
	}
	runKb238Shape(t, kb238RootEnvOnlyPlusWorktree(), "B")
}

func TestProbe_KB238_VariantC_RootUsesDevelop(t *testing.T) {
	if os.Getenv("KB238_PROBE") == "" {
		t.Skip("probe; set KB238_PROBE=1")
	}
	runKb238Shape(t, kb238RootDevelopUsed(), "C")
}

var _ = http.DefaultClient
