package engine

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	adapterhost "github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow/lockfile"
)

// kb232SlowPodAcceptanceHCL returns the parent workflow: a local warmup step,
// then the subworkflow step whose body owns the per-scope remote work — the
// shape KB-232 found racy (the sub-workflow's per-scope adapter pods dialed
// before the child scope registered).
func kb232SlowPodAcceptanceHCL() string {
	return `workflow {
  name          = "kb232-acceptance"
  version       = "0.1"
  initial_state = "warmup"
  target_state  = "done"

  # Widen the retry budget past the known verify→bind stale-handle window
  # (production shares this window; the pod redials on the same timescale).
  policy {
    max_step_retries = 50
  }
}

environment "remote" "prod" {
  listen_address     = "127.0.0.1:0"
  per_scope_sessions = true
}

environment "shell" "local" {}

adapter "noop" "default" {
  environment = shell.local
}

subworkflow "child" {
  source = "./child"
}

step "warmup" {
  target = adapter.noop.default
  outcome "success" { next = step.engage }
}

step "engage" {
  target = subworkflow.child
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

// kb232SlowPodChildHCL returns the subworkflow body: per_scope_sessions on the
// shared remote environment with its own adapter key, so the child scope-entry
// rotates its own per-scope instance and provisions its own session exactly
// like the evidenced workstream_handler run did.
func kb232SlowPodChildHCL() string {
	return `workflow {
  name          = "kb232-child"
  version       = "0.1"
  initial_state = "setup_worktree"
  target_state  = "done"
}

environment "remote" "prod" {
  listen_address     = "127.0.0.1:0"
  per_scope_sessions = true
}

adapter "shell" "worktree" {
  environment = remote.prod
}

step "setup_worktree" {
  target = adapter.shell.worktree
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

// kb232AcceptanceLockfile pins both shell adapter keys with the digest the
// engine's shim digest verifier accepts.
func kb232AcceptanceLockfile() *lockfile.Lockfile {
	return &lockfile.Lockfile{
		SchemaVersion: 1,
		Adapters: []lockfile.LockedAdapter{
			{
				Type:               "shell",
				Name:               "worktree",
				Reference:          "ghcr.io/brokenbots/criteria-adapter-shell",
				ResolvedDigest:     pinDigest,
				SourceURL:          "https://github.com/brokenbots/criteria",
				SDKProtocolVersion: 2,
				Platforms:          []string{"linux/amd64"},
			},
			{
				Type:               "shell",
				Name:               "default",
				Reference:          "ghcr.io/brokenbots/criteria-adapter-shell",
				ResolvedDigest:     pinDigest,
				SourceURL:          "https://github.com/brokenbots/criteria",
				SDKProtocolVersion: 2,
				Platforms:          []string{"linux/amd64"},
			},
		},
	}
}

// TestKB232_SubworkflowPerScopeSessionEstablishesWithSlowPodStart is the
// KB-232 acceptance test: a parent run reaches its sub-workflow, whose
// per-scope adapter session is satisfied by a pod that takes five seconds to
// come up and start dialing (the "operator creates the pod late" shape found
// in the evidence run). The session must establish — the child step executes
// over the bridged session and the run completes — instead of burning the
// step's deadline on a dead dial, with no half-window pending signal for a
// merely slow-but-healthy pod.
func TestKB232_SubworkflowPerScopeSessionEstablishesWithSlowPodStart(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "main.chcl"), kb232SlowPodAcceptanceHCL())
	writeFile(t, filepath.Join(root, "child", "main.chcl"), kb232SlowPodChildHCL())
	writeLockfile(t, root, kb232AcceptanceLockfile())
	g := compileWorkflowDir(t, root)

	beforeDirs := map[string]struct{}{}
	if matches, err := filepath.Glob(filepath.Join(os.TempDir(), "criteria-remote-*")); err == nil {
		for _, m := range matches {
			beforeDirs[m] = struct{}{}
		}
	}

	sink := &eventTrackingSink{}
	e := NewTestEngine(g, &fakeLoader{adapters: map[string]adapterhost.Handle{
		"noop": &fakeAdapter{name: "noop", outcome: "success"},
	}}, sink,
		WithWorkflowDir(g.WorkflowDir),
		WithDataDir(root),
		WithRunID("kb232-acceptance"))

	// The operator-driven adapter pod starts five seconds after the run does:
	// by then the child step is already waiting on its per-scope session, so
	// every dial until the pod arrives observes a pending wait.
	executions := &atomic.Int64{}
	pod := &cri145Pod{executions: executions}
	stopPod := make(chan struct{})
	go kb232DelayedPodServeLoop(sink, 5*time.Second, pod, stopPod)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(func() {
		close(stopPod)
		cancel()
		waitForCri137ShimTeardown(t, beforeDirs)
	})

	started := time.Now()
	runErr := make(chan error, 1)
	go func() { runErr <- e.Run(ctx) }()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(70 * time.Second):
		t.Fatalf("run did not finish in time; terminal=%q", sink.terminal)
	}
	if got := sink.terminal; got != "done" {
		t.Fatalf("terminal state = %q (ok=%v, failure=%q), want done", got, sink.terminalOK, sink.failure)
	}
	if !sink.terminalOK {
		t.Fatalf("run did not succeed (failure=%q)", sink.failure)
	}
	if elapsed := time.Since(started); elapsed > 30*time.Second {
		t.Errorf("run took %s for a 5s-pod-delay workflow; a session deadline burn would look like this", elapsed)
	}

	// The child scope's per-scope session established and executed: the scope
	// provision event carries the parent step's scope name, and the fake pod
	// served it — proving the dial converged into a working session.
	if got := executions.Load(); got < 1 {
		t.Fatalf("adapter Execute calls = %d, want at least 1 (the child step ran over the session)", got)
	}
	provisions, _ := cri145LifecycleStats(sink)
	var childProvision *AdapterLifecycleEvent
	for i := range provisions {
		if provisions[i].ScopeName != "" {
			childProvision = &provisions[i]
			break
		}
	}
	if childProvision == nil {
		t.Fatalf("no per-scope provision for the child's session (%d provisions: %+v)", len(provisions), provisions)
	}
	if childProvision.ScopeName != "engage" {
		t.Errorf("child provision scope_name = %q, want the subworkflow step name \"engage\"", childProvision.ScopeName)
	}
	if childProvision.RunID != "kb232-acceptance" {
		t.Errorf("child provision run_id = %q, want kb232-acceptance", childProvision.RunID)
	}

	// A healthy-but-slow pod is not a pending session: the half-window
	// signal must stay silent for the whole run.
	if tracker := sink; tracker.hasStatus(ScopeSessionPendingStatus) {
		t.Error("scope_session_pending emitted for a run whose session established well inside the window")
	}
}

// kb232DelayedPodServeLoop parks the pod for startDelay (the operator's
// 5-second pod-start), then hands over to the standard redial loop: the pod
// polls the lifecycle stream for its scope's provision event, reads the
// rotated token, and keeps dialing the shim with the pinned digest — serving
// each connection until the host closes it and redialing (the retry path).
func kb232DelayedPodServeLoop(sink *eventTrackingSink, startDelay time.Duration, pod *cri145Pod, stop <-chan struct{}) {
	select {
	case <-time.After(startDelay):
	case <-stop:
		return
	}
	pod.serveLoop(sink, stop)
}
