package cli

// reattach_cri293_test.go — CRI-293 regression tests at the CLI level: the
// local `criteria apply` crash-reattach engine (buildReattachTrackerAndEngine,
// reached from resumeOneLocalRun) must construct the engine with local shim
// isolation enabled, exactly like the fresh-run and resume-cycle engines, so
// two remote environments declaring one shared listen_address bind their own
// auto-chosen loopback ports instead of colliding on the second bind.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// cri293ReattachWorkflowHCL is the CRI-293 repro shape: two remote
// environments (worktree + primary, as in workstream_handler_v1) declaring
// the same listen_address, each with one noop adapter, in insecure mode so
// no accept tokens are needed on loopback.
const cri293ReattachWorkflowHCL = `
workflow {
  name          = "cri293_reattach"
  version       = "0.1"
  initial_state = "worktree"
  target_state  = "done"
}

environment "remote" "worktree" {
  listen_address = %q
  insecure       = true
}

environment "remote" "primary" {
  listen_address = %q
  insecure       = true
}

adapter "noop" "worktree" {
  environment = remote.worktree
}

adapter "noop" "primary" {
  environment = remote.primary
}

step "worktree" {
  target = adapter.noop.worktree
  outcome "success" { next = step.primary }
  outcome "failure" { next = step.done }
}

step "primary" {
  target = adapter.noop.primary
  outcome "success" { next = step.done }
  outcome "failure" { next = step.done }
}

state "done" {
  terminal = true
  success  = true
}
`

// captureLog is a mutex-guarded std-log sink. The remote shim logs its bound
// address through the slog default logger, which routes through the std log
// package, so capturing the std log captures shim bind events in-process.
type captureLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *captureLog) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *captureLog) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

// bufContains reports whether any captured log line contains substr.
func (c *captureLog) bufContains(substr string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Contains(c.buf.String(), substr)
}

// shimListenAddrs extracts the addr= values of all "remote shim listening"
// log lines observed so far.
func (c *captureLog) shimListenAddrs() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var addrs []string
	for _, line := range strings.Split(c.buf.String(), "\n") {
		if !strings.Contains(line, "remote shim listening") {
			continue
		}
		if _, after, ok := strings.Cut(line, "addr="); ok {
			addr := after
			if idx := strings.IndexAny(after, " \t"); idx >= 0 {
				addr = after[:idx]
			}
			addrs = append(addrs, addr)
		}
	}
	return addrs
}

// waitShimListenAddrs polls until count distinct shim addresses are observed
// or the deadline expires.
func waitShimListenAddrs(t *testing.T, logSink *captureLog, count int, within time.Duration) []string {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		addrs := logSink.shimListenAddrs()
		if len(addrs) >= count {
			return addrs[:count]
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d shim listen lines; got log:\n%s", count, logSink.String())
	return nil
}

// cri293ReattachCheckpoint builds an on-disk checkpoint for the shared
// listen_address workflow and returns it (the path is recorded on cp).
func cri293ReattachCheckpoint(t *testing.T, runID string) (cp *StepCheckpoint) {
	t.Helper()
	declared := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	wfFile := writeWorkflowFile(t, fmt.Sprintf(cri293ReattachWorkflowHCL, declared, declared))
	cp = &StepCheckpoint{
		RunID:        runID,
		Workflow:     "cri293_reattach",
		WorkflowPath: wfFile,
		CurrentStep:  "worktree",
		Attempt:      0,
	}
	if err := WriteStepCheckpoint(cp); err != nil {
		t.Fatalf("WriteStepCheckpoint: %v", err)
	}
	return cp
}

// TestBuildReattachTrackerAndEngine_CRI293_IsolationEnabled asserts the
// wiring contract directly: the local crash-reattach engine is constructed
// with local shim isolation enabled. This is the exact defect the reviewer
// found — the reattach site was the one local construction site that did not
// set the option, so it regresses if the helper or site drops it.
func TestBuildReattachTrackerAndEngine_CRI293_IsolationEnabled(t *testing.T) {
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())

	cp := cri293ReattachCheckpoint(t, "cri293-iso-flag")

	graph, loader, _, ok := prepareReattach(context.Background(), discardLogger(), cp)
	if !ok {
		t.Fatal("prepareReattach failed")
	}
	defer loader.Shutdown(context.Background())

	_, _, _, eng, engErr := buildReattachTrackerAndEngine(cp, discardLogger(), graph, loader, io.Discard, outputModeJSON, 1, nil)
	if engErr != nil {
		t.Fatalf("buildReattachTrackerAndEngine: %v", engErr)
	}
	iso := reflect.ValueOf(eng).Elem().FieldByName("localShimIsolation")
	if !iso.IsValid() {
		t.Fatal("engine has no localShimIsolation field; test needs updating")
	}
	if !iso.Bool() {
		t.Fatal("buildReattachTrackerAndEngine built the local reattach engine without local shim isolation (CRI-293): two remote environments sharing a listen_address collide on shim bind")
	}
}

// TestResumeOneLocalRun_CRI293_SharedListenAddressIsolated drives the full
// local crash-resume path for a two-remote-environment workflow sharing one
// listen_address. Both environment shims must bind (distinct auto-chosen
// loopback ports) without any "address already in use" error — the ticket's
// exact failure signature. Without shim isolation on the reattach engine the
// second bind fails, the run logs the EADDRINUSE error, and only one (or
// zero) shim listen lines appear, failing the test.
func TestResumeOneLocalRun_CRI293_SharedListenAddressIsolated(t *testing.T) {
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())

	cp := cri293ReattachCheckpoint(t, "cri293-reattach-iso")

	logSink := &captureLog{}
	prev := log.Writer()
	defer log.SetOutput(prev)
	log.SetOutput(logSink)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan error, 1)
	go func() {
		_, runErr := resumeOneLocalRun(ctx, discardLogger(), cp, io.Discard, outputModeJSON, nil)
		runDone <- runErr
	}()

	// Both environments' shims must come up on their own loopback ports.
	addrs := waitShimListenAddrs(t, logSink, 2, 15*time.Second)
	if addrs[0] == addrs[1] {
		t.Fatalf("both remote environment shims bound the same address %s; isolation must give each environment its own shim", addrs[0])
	}
	for _, addr := range addrs {
		if !strings.HasPrefix(addr, "127.0.0.1:") {
			t.Errorf("shim bound %q; want a loopback address", addr)
		}
	}
	if logSink.bufContains("address already in use") {
		t.Errorf("run log contains the CRI-293 bind-collision error:\n%s", logSink.String())
	}

	// The run is expected to block waiting for the remote adapters to phone
	// home (nothing spawns them in the test); cancel to end it.
	cancel()
	select {
	case runErr := <-runDone:
		if runErr == nil {
			t.Log("resumeOneLocalRun returned nil after cancellation")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("resumeOneLocalRun did not return after cancellation")
	}
}
