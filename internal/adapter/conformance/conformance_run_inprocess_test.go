package conformance

// conformance_run_inprocess_test.go — covers the in-process Run entry point
// with a fully in-memory stub adapter, plus direct smokes over the matrix
// engine sink and the no-op lifecycle hooks of the in-memory fakes. The
// socket/binary suites (runV2Suites) are exercised by the adapter binary
// tests; this file pins the contract harness itself.

import (
	"bufio"
	"bytes"
	"context"
	"os/exec"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/internal/engine"
	"github.com/brokenbots/criteria/workflow"
)

const runStubAdapterName = "run-stub"

// runStubAdapter is an in-process adapter.Adapter: with a `command` input it
// runs a POSIX shell command and streams its stdout through Log events; with
// `delay_ms` it sleeps while honouring context cancellation.
type runStubAdapter struct{}

func (runStubAdapter) Name() string               { return runStubAdapterName }
func (runStubAdapter) Info() workflow.AdapterInfo { return workflow.AdapterInfo{} }

func (runStubAdapter) Execute(ctx context.Context, step *workflow.StepNode, sink adapter.EventSink) (adapter.Result, error) {
	if cmdText := step.Input["command"]; cmdText != "" && runtime.GOOS != "windows" {
		cmd := exec.CommandContext(ctx, "sh", "-c", cmdText)
		// CommandContext kills only the direct child (sh). When the shell
		// forks a descendant that inherits stdout/stderr, Output would block
		// on the stdio copy goroutines past the harness's 500ms cancellation
		// budget; WaitDelay bounds that wait and lets Execute surface
		// ctx.Err() as the cancellation error.
		cmd.WaitDelay = 100 * time.Millisecond
		out, err := cmd.Output()
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return adapter.Result{}, ctxErr
			}
			return adapter.Result{}, err
		}
		if sink != nil {
			sc := bufio.NewScanner(bytes.NewReader(out))
			sc.Buffer(make([]byte, 64*1024), 1024*1024)
			for sc.Scan() {
				sink.Log("stdout", append([]byte(sc.Text()), '\n'))
			}
		}
		return adapter.Result{Outcome: "success"}, nil
	}

	if raw, ok := step.Input["delay_ms"]; ok {
		if delayMs, err := strconv.Atoi(raw); err == nil && delayMs > 0 {
			select {
			case <-time.After(time.Duration(delayMs) * time.Millisecond):
			case <-ctx.Done():
				return adapter.Result{}, ctx.Err()
			}
		}
	}
	if sink != nil {
		sink.Log("stub", []byte("ok\n"))
	}
	return adapter.Result{Outcome: "success"}, nil
}

// TestConformanceRun_InProcess drives the full contract harness through
// conformance.Run: name stability, nil sink, happy path, context
// cancellation, step timeout, outcome domain and chunked IO.
func TestConformanceRun_InProcess(t *testing.T) {
	Run(t, runStubAdapterName, func() adapter.Adapter { return runStubAdapter{} }, Options{
		StepConfig:      map[string]string{"command": "printf 'conformed\\n'"},
		AllowedOutcomes: []string{"success"},
		Streaming:       true,
	})
}

// TestRunStubAdapter_ForkedDescendantCancellation pins the bounded shutdown
// of the stub's shell-out: with WaitDelay unset, a shell that forks a
// descendant holding stdout/stderr open (e.g. a non-optimizing /bin/sh
// around `sleep 5`, or a forking shim first on PATH) left Output blocked on
// the os/exec stdio copy goroutines past the harness's 500ms cancellation
// budget, failing the context_cancellation contract test with leaked
// goroutines. The command here shells out via the default PATH shell and
// backgrounds a child that keeps its output descriptors open while the
// shell itself exits.
func TestRunStubAdapter_ForkedDescendantCancellation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell backgrounding required")
	}
	// Snapshot before spawning anything so benign goroutines from other
	// tests in the binary are not attributed to this test.
	snapshot := goleak.IgnoreCurrent()

	step := baseStep("forked-descendant", runStubAdapterName, map[string]string{
		"command": "sleep 5 & printf 'forked\\n'",
	})

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	t.Cleanup(cancel)

	done := make(chan struct{})
	var execErr error
	go func() {
		_, execErr = runStubAdapter{}.Execute(ctx, step, nil)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Execute did not return within the 500ms harness budget after cancellation")
	}

	if !isCancellationLikeError(execErr) {
		t.Fatalf("expected cancellation-like error, got: %v", execErr)
	}
	t.Cleanup(func() { goleak.VerifyNone(t, snapshot) })
}

// TestMatrixEngineSink_CoversAllCallbacks calls every sink callback in a
// single goroutine and checks what the recording accessors expose.
func TestMatrixEngineSink_CoversAllCallbacks(t *testing.T) {
	s := &matrixEngineSink{}

	// Pure no-op callbacks must be safe to call.
	s.OnRunStarted("run-1", "user")
	s.OnStepResumed("step-a", 1, "reason")
	s.OnVariableSet("key", "value", "origin")
	s.OnStepOutputCaptured("step-a", map[string]string{"out": "v"})
	s.OnRunPaused("run-1", "step-a", "reason")
	s.OnRunResumed("run-1")
	s.OnWaitEntered("step-w", "signal-a", "until", "on")
	s.OnWaitResumed("step-w", "signal-a", "node", nil)
	s.OnApprovalRequested("step-a", []string{"ops"}, "request-1")
	s.OnApprovalDecision("step-a", "request-1", "approved", nil)
	s.OnBranchEvaluated("step-a", "cond", "taken", "on")
	s.OnForEachEntered("step-f", 3)
	s.OnStepIterationStarted("step-f", 1, "item-key", false)
	s.OnStepIterationCompleted("step-f", "success", "item-key")
	s.OnStepIterationItem("step-f", 1, "item-1")
	s.OnScopeIterCursorSet("cursor-1")
	s.OnAdapterLifecycle("pull", "op", "id", "detail")
	s.OnAdapterLifecycleEvent(&engine.AdapterLifecycleEvent{})
	s.OnRunOutputs([]*pb.RunOutputs_Output{{Name: "k", Value: "v"}})
	s.OnAgentPromptInjected("step-a", "model", "prompt", "origin", time.Now())
	s.OnCheckpointPointer(&engine.CheckpointPointerEvent{})

	// Recording callbacks feed the accessors the matrix asserts on.
	s.OnRunCompleted("succeeded", true)
	require.Equal(t, "succeeded", s.terminalState())
	require.True(t, s.runOK())

	s.OnRunFailed("boom", "step-a")
	require.Equal(t, "boom (step step-a)", s.runFailure())
	require.True(t, s.runOK(), "OnRunFailed does not clear the run completion flag")

	s.OnStepEntered("step-a", "run-1", 0)
	s.OnStepOutcome("step-a", "success", time.Second, nil, "")
	s.OnStepTransition("step-a", "step-b", "outcome")
	require.Equal(t, []string{"step-a"}, s.stepsRun())
	require.Equal(t, []string{"step-a=success"}, s.stepOutcomes())
	require.Equal(t, []string{"step-a->step-b"}, s.transitions())

	s.OnStepOutcomeDefaulted("step-a", "original", "defaulted")
	s.OnStepOutcomeUnknown("step-u", "welp")
	s.OnStepOutcomeInvalid("step-i", "nope", []string{"a", "b"}, 2)
	require.Len(t, s.unknownOutcomes(), 3)

	// StepEventSink hands out the recorder whose Log is a no-op and whose
	// Adapter events are attributed to the step. stepEventCount only counts
	// events recorded under matrixCallerStep, so attribute there.
	rec := s.StepEventSink(matrixCallerStep)
	require.NotNil(t, rec)
	rec.Log("stream", []byte("ignored"))
	rec.Adapter("tool_reply", map[string]any{"k": "v"})
	require.Equal(t, 1, s.stepEventCount("tool_reply"))

	// Audit collector ignores nil entries and records the rest.
	audit := &matrixAuditCollector{}
	audit.Write(nil)
	audit.Write(&adapterhost.DecisionLogEntry{})
	require.Len(t, audit.entries, 1)
}

// TestMatrixFakes_NoopLifecycleHooks covers the unused session-control hooks
// of the in-memory caller/callee/agent fakes: they are protocol surface (the
// engine may invoke them) and must be safe to call on a zero value.
func TestMatrixFakes_NoopLifecycleHooks(t *testing.T) {
	ctx := context.Background()

	caller := &matrixCallerAdapter{}
	caller.Kill()
	require.NoError(t, caller.Pause(ctx, "session-1"))
	require.NoError(t, caller.Resume(ctx, "session-1"))
	inspect, err := caller.Inspect(ctx, "session-1")
	require.NoError(t, err)
	require.NotNil(t, inspect)
	snapshot, err := caller.Snapshot(ctx, "session-1")
	require.NoError(t, err)
	require.NotNil(t, snapshot)
	require.NoError(t, caller.Restore(ctx, "session-1", []byte("state"), 1))

	callee := &matrixCalleeAdapter{}
	callee.Kill()
	require.NoError(t, callee.Pause(ctx, "session-1"))
	require.NoError(t, callee.Resume(ctx, "session-1"))
	inspect, err = callee.Inspect(ctx, "session-1")
	require.NoError(t, err)
	require.NotNil(t, inspect)
	snapshot, err = callee.Snapshot(ctx, "session-1")
	require.NoError(t, err)
	require.NotNil(t, snapshot)
	require.NoError(t, callee.Restore(ctx, "session-1", []byte("state"), 1))

	agent := newAgentCaller()
	agent.Kill()
	require.NoError(t, agent.Pause(ctx, "session-1"))
	require.NoError(t, agent.Resume(ctx, "session-1"))
	inspect, err = agent.Inspect(ctx, "session-1")
	require.NoError(t, err)
	require.NotNil(t, inspect)
	snapshot, err = agent.Snapshot(ctx, "session-1")
	require.NoError(t, err)
	require.NotNil(t, snapshot)
	require.NoError(t, agent.Restore(ctx, "session-1", []byte("state"), 1))

	// Compile-time assertion that the noop v2 hooks still return the typed
	// responses the engine decodes.
	require.IsType(t, &v2.InspectResponse{}, inspect)
}
