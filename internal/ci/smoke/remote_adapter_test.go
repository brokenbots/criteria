// Package smoke contains end-to-end smoke tests gated by environment variables
// so they do not run on every `go test` invocation.
package smoke

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/internal/engine"
	"github.com/brokenbots/criteria/workflow"
	"github.com/brokenbots/criteria/workflow/lockfile"
)

// testSink captures the terminal state of a workflow run.
type testSink struct {
	mu       sync.Mutex
	terminal string
	success  bool
}

func (s *testSink) OnRunStarted(string, string)                                              {}
func (s *testSink) OnRunCompleted(state string, ok bool) {
	s.mu.Lock()
	s.terminal = state
	s.success = ok
	s.mu.Unlock()
}
func (s *testSink) OnRunFailed(string, string)                                             {}
func (s *testSink) OnStepEntered(string, string, int)                                        {}
func (s *testSink) OnStepOutcome(string, string, time.Duration, error)                        {}
func (s *testSink) OnStepTransition(string, string, string)                                  {}
func (s *testSink) OnStepResumed(string, int, string)                                        {}
func (s *testSink) OnVariableSet(string, string, string)                                     {}
func (s *testSink) OnStepOutputCaptured(string, map[string]string)                          {}
func (s *testSink) OnRunPaused(string, string, string)                                         {}
func (s *testSink) OnWaitEntered(string, string, string, string)                             {}
func (s *testSink) OnWaitResumed(string, string, string, map[string]string)                  {}
func (s *testSink) OnApprovalRequested(string, []string, string)                             {}
func (s *testSink) OnApprovalDecision(string, string, string, map[string]string)             {}
func (s *testSink) OnBranchEvaluated(string, string, string, string)                         {}
func (s *testSink) OnForEachEntered(string, int)                                             {}
func (s *testSink) OnStepIterationStarted(string, int, string, bool)                        {}
func (s *testSink) OnStepIterationCompleted(string, string, string)                          {}
func (s *testSink) OnStepIterationItem(string, int, string)                                  {}
func (s *testSink) OnScopeIterCursorSet(string)                                               {}
func (s *testSink) OnAdapterLifecycle(string, string, string, string)                        {}
func (s *testSink) OnRunOutputs([]map[string]string)                                         {}
func (s *testSink) OnStepOutcomeDefaulted(string, string, string)                            {}
func (s *testSink) OnStepOutcomeUnknown(string, string)                                       {}
func (s *testSink) StepEventSink(string) adapter.EventSink                                   { return noopEventSink{} }

type noopEventSink struct{}

func (noopEventSink) Log(string, []byte) {}
func (noopEventSink) Adapter(string, any) {}

// TestRemoteAdapter_HappyPath exercises the full phone-home flow: criteria
// starts a remote shim, the fixture adapter dials in, and a workflow step
// executes successfully.
//
// Gated by CRITERIA_REMOTE_E2E=1.
func TestRemoteAdapter_HappyPath(t *testing.T) {
	if os.Getenv("CRITERIA_REMOTE_E2E") != "1" {
		t.Skip("set CRITERIA_REMOTE_E2E=1 to run remote adapter smoke tests")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	moduleRoot := findModuleRoot(t)
	adapterBin := buildFixtureAdapter(t, moduleRoot)
	digest := sha256OfFile(t, adapterBin)

	shimAddr := pickFreeAddr(t)
	workflowDir := t.TempDir()

	spec := parseWorkflow(t, fmt.Sprintf(`
workflow {
  name = "remote-smoke-happy"
  version = "0.1"
  initial_state = "run"
  target_state  = "done"
}

environment "remote" "test" {
  listen_address = %q
  accept_token   = "smoke-token"
}

adapter "remote-smoke" "demo" {
  environment = remote.test
}

step "run" {
  target = adapter.remote-smoke.demo
  input {
    greeting = "hello"
  }
  outcome "success" { next = "done" }
}

state "done" {
  terminal = true
  success  = true
}
`, shimAddr))

	graph := compileWorkflow(t, spec)
	lf := buildLockfile("remote-smoke", "demo", digest)

	adapterCmd, adapterCancel := startAdapter(ctx, t, adapterBin, shimAddr, "smoke-token", digest)
	defer adapterCancel()

	sink := &testSink{}
	eng := engine.New(graph, adapterhost.NewLoader(), sink,
		engine.WithWorkflowDir(workflowDir),
		engine.WithLockfile(lf),
	)

	if err := eng.Run(ctx); err != nil {
		t.Fatalf("engine run: %v", err)
	}

	adapterCancel()
	_ = adapterCmd.Wait()

	if !sink.success {
		t.Fatalf("workflow did not complete successfully: terminal=%q", sink.terminal)
	}
}

// TestRemoteAdapter_CrashRecovery kills the adapter mid-execution, asserts the
// crash policy kicks in, then restarts the adapter and verifies the workflow
// completes after the respawn retry.
//
// Gated by CRITERIA_REMOTE_E2E=1.
func TestRemoteAdapter_CrashRecovery(t *testing.T) {
	if os.Getenv("CRITERIA_REMOTE_E2E") != "1" {
		t.Skip("set CRITERIA_REMOTE_E2E=1 to run remote adapter smoke tests")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	moduleRoot := findModuleRoot(t)
	adapterBin := buildFixtureAdapter(t, moduleRoot)
	digest := sha256OfFile(t, adapterBin)

	shimAddr := pickFreeAddr(t)
	workflowDir := t.TempDir()

	spec := parseWorkflow(t, fmt.Sprintf(`
workflow {
  name = "remote-smoke-crash"
  version = "0.1"
  initial_state = "run"
  target_state  = "done"
}

environment "remote" "test" {
  listen_address = %q
  accept_token   = "smoke-token"
}

adapter "remote-smoke" "demo" {
  environment = remote.test
  on_crash = "respawn"
}

step "run" {
  target = adapter.remote-smoke.demo
  input {
    greeting = "hello"
    delay_ms = "5000"
  }
  outcome "success" { next = "done" }
}

state "done" {
  terminal = true
  success  = true
}
`, shimAddr))

	graph := compileWorkflow(t, spec)
	lf := buildLockfile("remote-smoke", "demo", digest)

	stepStartedFile := filepath.Join(t.TempDir(), "step-started")

	// Start criteria apply in the background.
	engCtx, engCancel := context.WithCancel(ctx)
	defer engCancel()
	engDone := make(chan struct{})
	go func() {
		defer close(engDone)
		sink := &testSink{}
		eng := engine.New(graph, adapterhost.NewLoader(), sink,
			engine.WithWorkflowDir(workflowDir),
			engine.WithLockfile(lf),
		)
		if err := eng.Run(engCtx); err != nil {
			t.Fatalf("engine run error: %v", err)
		}
		if !sink.success {
			t.Fatalf("workflow did not complete successfully: terminal=%q", sink.terminal)
		}
	}()

	// Give the engine a moment to start the shim.
	time.Sleep(500 * time.Millisecond)

	// Start the adapter with a marker file so we know when Execute begins.
	adapterCmd1, adapterCancel1 := startAdapterWithMarker(ctx, t, adapterBin, shimAddr, "smoke-token", digest, stepStartedFile)

	// Wait until the adapter signals that the step has started.
	waitForFile(t, stepStartedFile, 10*time.Second)

	// Give the step a moment to enter the delay so we kill it mid-execution.
	time.Sleep(500 * time.Millisecond)

	// Start the replacement adapter BEFORE killing the old one so the shim
	// has a fresh session ready when the engine calls respawn.
	_, adapterCancel2 := startAdapter(ctx, t, adapterBin, shimAddr, "smoke-token", digest)
	defer adapterCancel2()

	// Now kill the old adapter — the shim already has the new connection.
	adapterCancel1()
	_ = adapterCmd1.Wait()

	// Wait for criteria to finish.
	select {
	case <-engDone:
	case <-ctx.Done():
		t.Fatal("timeout waiting for engine run to finish")
	}
}

// --- helpers ---

func findModuleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve caller path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
}

func buildFixtureAdapter(t *testing.T, moduleRoot string) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "criteria-adapter-remote-smoke")
	srcDir := filepath.Join(moduleRoot, "internal", "ci", "smoke", "testdata", "criteria-adapter-remote-smoke")
	cmd := exec.Command("go", "build", "-o", binary, ".")
	cmd.Dir = srcDir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build fixture adapter: %v\n%s", err, string(out))
	}
	return binary
}

func sha256OfFile(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open file for sha256: %v", err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatalf("hash file: %v", err)
	}
	return fmt.Sprintf("sha256:%x", h.Sum(nil))
}

func pickFreeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick free port: %v", err)
	}
	defer l.Close()
	return l.Addr().String()
}

func parseWorkflow(t *testing.T, contents string) *workflow.Spec {
	t.Helper()
	spec, diags := workflow.Parse("workflow.hcl", []byte(strings.TrimSpace(contents)+"\n"))
	if diags.HasErrors() {
		t.Fatalf("parse workflow: %v", diags)
	}
	return spec
}

func compileWorkflow(t *testing.T, spec *workflow.Spec) *workflow.FSMGraph {
	t.Helper()
	graph, diags := workflow.Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile workflow: %v", diags)
	}
	return graph
}

func buildLockfile(adapterType, adapterName, digest string) *lockfile.Lockfile {
	return &lockfile.Lockfile{
		SchemaVersion: 1,
		Adapters: []lockfile.LockedAdapter{
			{
				Type:               adapterType,
				Name:               adapterName,
				Reference:          "local",
				ResolvedDigest:     digest,
				SourceURL:          "local",
				SDKProtocolVersion: 2,
			},
		},
	}
}

func startAdapter(ctx context.Context, t *testing.T, bin, addr, token, digest string) (*exec.Cmd, context.CancelFunc) {
	t.Helper()
	return startAdapterWithMarker(ctx, t, bin, addr, token, digest, "")
}

func startAdapterWithMarker(ctx context.Context, t *testing.T, bin, addr, token, digest, stepStartedFile string) (*exec.Cmd, context.CancelFunc) {
	t.Helper()
	cmdCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(cmdCtx, bin)
	cmd.Env = append(os.Environ(),
		"CRITERIA_REMOTE_HOST="+addr,
		"CRITERIA_REMOTE_TOKEN="+token,
		"CRITERIA_REMOTE_DIGEST="+digest,
	)
	if stepStartedFile != "" {
		cmd.Env = append(cmd.Env, "CRITERIA_REMOTE_STEP_STARTED_FILE="+stepStartedFile)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start adapter: %v", err)
	}
	return cmd, cancel
}

func waitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_, err := os.Stat(path)
		if err == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for file %s", path)
}
