// Package smoke contains end-to-end smoke tests gated by environment variables
// so they do not run on every `go test` invocation.
package smoke

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/internal/engine"
)

// TestPeerSmoke_HappyPath exercises the full peer phone-home flow with the
// real `criteria peer` binary: the engine starts a remote shim, `criteria
// peer` dials in, presents the identity frame, boots the noop adapter child
// as a go-plugin subprocess, and a workflow step executes successfully
// through the peer.
//
// Gated by CRITERIA_PEER_E2E=1.
func TestPeerSmoke_HappyPath(t *testing.T) {
	if os.Getenv("CRITERIA_PEER_E2E") != "1" {
		t.Skip("set CRITERIA_PEER_E2E=1 to run peer smoke tests")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	moduleRoot := findModuleRoot(t)
	criteriaBin := buildCriteriaBinary(t, moduleRoot)
	noopBin := buildNoopSmokeBinary(t, moduleRoot)
	digest := sha256OfFile(t, noopBin)

	shimAddr := pickFreeAddr(t)
	workflowDir := t.TempDir()

	spec := parseWorkflow(t, fmt.Sprintf(`
workflow {
  name = "peer-smoke-happy"
  version = "0.1"
  initial_state = "run"
  target_state  = "done"
}

environment "remote" "test" {
  listen_address = %q
  accept_token   = "smoke-token"
}

adapter "noop" "demo" {
  environment = remote.test
}

step "run" {
  target = adapter.noop.demo
  input {
    emit_log = "peer-smoke-hello"
  }
  outcome "success" { next = step.done }
}

state "done" {
  terminal = true
  success  = true
}
`, shimAddr))

	graph := compileWorkflow(t, spec)
	lf := buildLockfile("noop", "demo", digest)

	peerCmd, peerLogs, peerCancel := startPeer(ctx, t, criteriaBin, shimAddr, "smoke-token", noopBin, digest)
	defer peerCancel()

	sink := &testSink{}
	eng := engine.New(graph, adapterhost.NewLoader(), sink,
		engine.WithWorkflowDir(workflowDir),
		engine.WithLockfile(lf),
	)

	if err := eng.Run(ctx); err != nil {
		dumpPeerLogs(t, peerLogs)
		t.Fatalf("engine run: %v", err)
	}

	peerCancel()
	_ = peerCmd.Wait()

	if !sink.success {
		t.Fatalf("workflow did not complete successfully: terminal=%q", sink.terminal)
	}
}

// TestPeerSmoke_CrashRecovery kills `criteria peer` mid-execution, asserts
// the crash policy kicks in, then adopts a freshly started peer and verifies
// the workflow completes after the respawn retry.
//
// Gated by CRITERIA_PEER_E2E=1.
func TestPeerSmoke_CrashRecovery(t *testing.T) {
	if os.Getenv("CRITERIA_PEER_E2E") != "1" {
		t.Skip("set CRITERIA_PEER_E2E=1 to run peer smoke tests")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	moduleRoot := findModuleRoot(t)
	criteriaBin := buildCriteriaBinary(t, moduleRoot)
	noopBin := buildNoopSmokeBinary(t, moduleRoot)
	digest := sha256OfFile(t, noopBin)

	shimAddr := pickFreeAddr(t)
	workflowDir := t.TempDir()

	spec := parseWorkflow(t, fmt.Sprintf(`
workflow {
  name = "peer-smoke-crash"
  version = "0.1"
  initial_state = "run"
  target_state  = "done"
}

environment "remote" "test" {
  listen_address = %q
  accept_token   = "smoke-token"
}

adapter "noop" "demo" {
  environment = remote.test
  on_crash = "respawn"
}

step "run" {
  target = adapter.noop.demo
  input {
    emit_log = "peer-smoke-crash"
    delay_ms = "5000"
  }
  outcome "success" { next = step.done }
}

state "done" {
  terminal = true
  success  = true
}
`, shimAddr))

	graph := compileWorkflow(t, spec)
	lf := buildLockfile("noop", "demo", digest)

	engCtx, engCancel := context.WithCancel(ctx)
	defer engCancel()
	type engineResult struct {
		err      error
		success  bool
		terminal string
	}
	engDone := make(chan engineResult, 1)
	go func() {
		sink := &testSink{}
		eng := engine.New(graph, adapterhost.NewLoader(), sink,
			engine.WithWorkflowDir(workflowDir),
			engine.WithLockfile(lf),
		)
		r := engineResult{err: eng.Run(engCtx), success: sink.success, terminal: sink.terminal}
		engDone <- r
	}()

	// Give the engine a moment to start the shim, then connect peer #1.
	time.Sleep(500 * time.Millisecond)
	peerCmd1, peerLogs1, peerCancel1 := startPeer(ctx, t, criteriaBin, shimAddr, "smoke-token", noopBin, digest)

	// The step sits in a 5s noop delay; land the kill inside that window.
	time.Sleep(1200 * time.Millisecond)

	// Start the replacement peer BEFORE killing the old one so the shim has
	// a fresh session ready when the engine calls respawn.
	_, peerLogs2, peerCancel2 := startPeer(ctx, t, criteriaBin, shimAddr, "smoke-token", noopBin, digest)
	defer peerCancel2()
	defer dumpPeerLogs(t, peerLogs2)

	// Now kill peer #1 with SIGKILL — the shim already has peer #2.
	peerCancel1()
	if err := peerCmd1.Wait(); err == nil {
		t.Errorf("expected peer #1 to die from SIGKILL, exited cleanly")
	}
	dumpPeerLogs(t, peerLogs1)

	// Wait for criteria to finish.
	select {
	case r := <-engDone:
		if r.err != nil {
			t.Fatalf("engine run error: %v", r.err)
		}
		if !r.success {
			t.Fatalf("workflow did not complete successfully: terminal=%q", r.terminal)
		}
	case <-ctx.Done():
		t.Fatal("timeout waiting for engine run to finish")
	}
}

// --- peer-specific helpers ---

// buildCriteriaBinary builds the repo's criteria binary (cmd/criteria), the
// real phone-home peer the smoke suite drives.
func buildCriteriaBinary(t *testing.T, moduleRoot string) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "criteria")
	cmd := exec.Command("go", "build", "-o", binary, "./cmd/criteria")
	cmd.Dir = moduleRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build criteria binary: %v\n%s", err, string(out))
	}
	return binary
}

// buildNoopSmokeBinary builds the noop conformance adapter, used as the
// peer's adapter child.
func buildNoopSmokeBinary(t *testing.T, moduleRoot string) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "criteria-adapter-noop")
	cmd := exec.Command("go", "build", "-o", binary, "./internal/adapter/conformance/testdata/noop")
	cmd.Dir = moduleRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build noop adapter: %v\n%s", err, string(out))
	}
	return binary
}

// startPeer launches the real `criteria peer` binary against the shim
// address with compressed backoff so reconnects stay fast under test, and
// captures its output for failure diagnostics.
func startPeer(ctx context.Context, t *testing.T, criteriaBin, addr, token, adapterBin, digest string) (*exec.Cmd, *bytes.Buffer, context.CancelFunc) {
	t.Helper()
	cmdCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(cmdCtx, criteriaBin, "peer")
	logs := &bytes.Buffer{}
	cmd.Stdout = logs
	cmd.Stderr = logs
	cmd.Env = append(os.Environ(),
		"CRITERIA_REMOTE_HOST="+addr,
		"CRITERIA_REMOTE_TOKEN="+token,
		"CRITERIA_REMOTE_SCOPE=peer-smoke",
		"CRITERIA_REMOTE_DIGEST="+digest,
		"CRITERIA_ADAPTER_NAME=noop",
		"CRITERIA_ADAPTER_VERSION=0.1.0",
		"CRITERIA_ADAPTER_BINARY="+adapterBin,
		"CRITERIA_PEER_BACKOFF_MIN=200ms",
		"CRITERIA_PEER_BACKOFF_MAX=1s",
		"CRITERIA_LOG_LEVEL=debug",
	)
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start criteria peer: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		_ = cmd.Wait()
	})
	return cmd, logs, cancel
}

// dumpPeerLogs prints captured peer output when a smoke step fails.
func dumpPeerLogs(t *testing.T, logs *bytes.Buffer) {
	t.Helper()
	if logs != nil && logs.Len() > 0 {
		t.Logf("criteria peer output:\n%s", logs.String())
	}
}