package cli

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/brokenbots/criteria/internal/cli/applytest"
	servertrans "github.com/brokenbots/criteria/internal/transport/server"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

// startTestAgent starts runAgent against the supplied fake server and returns
// the context cancel function and a channel that receives the runAgent return
// value. The caller must call cancel and wait on the channel before the test
// ends to satisfy goleak.
func startTestAgent(t *testing.T, fake *applytest.Fake) (cancel context.CancelFunc, errCh <-chan error) {
	t.Helper()
	return startTestAgentOpts(t, fake, &agentOptions{})
}

// startTestAgentOpts starts runAgent with the supplied options merged over the
// test defaults.
func startTestAgentOpts(t *testing.T, fake *applytest.Fake, opts *agentOptions) (cancel context.CancelFunc, errCh <-chan error) {
	t.Helper()
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())

	ctx, cancel := context.WithCancel(context.Background())
	errChOut := make(chan error, 1)
	if opts.serverURL == "" {
		opts.serverURL = fake.URL()
	}
	if opts.name == "" {
		opts.name = "test-agent"
	}
	if opts.tlsMode == "" {
		opts.tlsMode = serverTransTLSModeName(servertrans.TLSDisable)
	}
	if opts.log == nil {
		opts.log = newApplyLogger()
	}
	go func() { errChOut <- runAgent(ctx, opts) }()
	return cancel, errChOut
}

// serverTransTLSModeName returns the CLI flag value that corresponds to a
// transport TLS mode constant.
func serverTransTLSModeName(mode servertrans.TLSMode) string {
	switch mode {
	case servertrans.TLSDisable:
		return "disable"
	case servertrans.TLSEnable:
		return "tls"
	case servertrans.TLSMutual:
		return "mtls"
	default:
		return ""
	}
}

// waitAgent waits for runAgent to return after the context has been canceled.
func waitAgent(t *testing.T, errCh <-chan error) {
	t.Helper()
	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("runAgent returned unexpected error: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("runAgent did not exit within 15s")
	}
}

// startAgentWithStateDir starts runAgent with a specific Criteria state
// directory. Use this for crash-recovery tests that need a second agent
// process to see the same persisted state as the first.
func startAgentWithStateDir(t *testing.T, fake *applytest.Fake, stateDir string) (cancel context.CancelFunc, errCh <-chan error) {
	t.Helper()
	t.Setenv("CRITERIA_STATE_DIR", stateDir)

	ctx, cancel := context.WithCancel(context.Background())
	errChOut := make(chan error, 1)
	opts := &agentOptions{
		serverURL: fake.URL(),
		name:      "test-agent",
		tlsMode:   serverTransTLSModeName(servertrans.TLSDisable),
		log:       newApplyLogger(),
	}
	go func() { errChOut <- runAgent(ctx, opts) }()
	return cancel, errChOut
}

// makeAssignment builds a WorkflowAssignment for the given source HCL.
func makeAssignment(runID, workflowName, source string) *pb.WorkflowAssignment {
	return &pb.WorkflowAssignment{
		RunId:          runID,
		WorkflowName:   workflowName,
		WorkflowSource: source,
	}
}

// requireStableEventCount waits up to d and fails if the count of events of
// typeName for runID changes during the window. Use this instead of a raw
// sleep for negative assertions ("no duplicate execution").
func requireStableEventCount(t *testing.T, fake *applytest.Fake, runID, typeName string, count int, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if got := runEventCountOfType(fake, runID, typeName); got != count {
			t.Fatalf("event count changed unexpectedly: got %d %s events for %s, want %d", got, typeName, runID, count)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// runHasEventOfType reports whether the fake received a terminal event of the
// given type for the given run_id.
func runHasEventOfType(fake *applytest.Fake, runID, typeName string) bool {
	for _, env := range fake.Events() {
		if env.RunId != runID {
			continue
		}
		switch typeName {
		case "RunCompleted":
			if env.GetRunCompleted() != nil {
				return true
			}
		case "RunFailed":
			if env.GetRunFailed() != nil {
				return true
			}
		case "RunStarted":
			if env.GetRunStarted() != nil {
				return true
			}
		case "WaitEntered":
			if env.GetWaitEntered() != nil {
				return true
			}
		}
	}
	return false
}

// runTerminalEventIndex returns the index in the global event log of the first
// RunCompleted or RunFailed event for runID, or -1 if none has arrived yet.
func runTerminalEventIndex(fake *applytest.Fake, runID string) int {
	for i, env := range fake.Events() {
		if env.RunId != runID {
			continue
		}
		if env.GetRunCompleted() != nil || env.GetRunFailed() != nil {
			return i
		}
	}
	return -1
}

// runEventIndex returns the index of the first event of typeName for runID,
// or -1 if none has arrived yet.
func runEventIndex(fake *applytest.Fake, runID, typeName string) int {
	for i, env := range fake.Events() {
		if env.RunId != runID {
			continue
		}
		switch typeName {
		case "RunStarted":
			if env.GetRunStarted() != nil {
				return i
			}
		case "RunCompleted":
			if env.GetRunCompleted() != nil {
				return i
			}
		}
	}
	return -1
}

// runEventCountOfType returns how many events of typeName the fake received for
// the given run_id.
func runEventCountOfType(fake *applytest.Fake, runID, typeName string) int {
	count := 0
	for _, env := range fake.Events() {
		if env.RunId != runID {
			continue
		}
		switch typeName {
		case "RunFailed":
			if env.GetRunFailed() != nil {
				count++
			}
		case "RunCompleted":
			if env.GetRunCompleted() != nil {
				count++
			}
		case "RunStarted":
			if env.GetRunStarted() != nil {
				count++
			}
		case "StepEntered":
			if env.GetStepEntered() != nil {
				count++
			}
		}
	}
	return count
}

// stepEnteredCount returns how many StepEntered events the fake received for a
// specific run_id and step name.
func stepEnteredCount(fake *applytest.Fake, runID, step string) int {
	count := 0
	for _, env := range fake.Events() {
		if env.RunId != runID {
			continue
		}
		if se := env.GetStepEntered(); se != nil && se.Step == step {
			count++
		}
	}
	return count
}

func TestAgent_HappyPath(t *testing.T) {
	requireNoGoroutineLeak(t)
	fake := applytest.New(t)
	cancel, errCh := startTestAgent(t, fake)
	defer cancel()

	fake.WaitForCond(t, 5*time.Second, func() bool { return fake.ControlAttachCount() > 0 })

	runID := uuid.NewString()
	fake.QueueAssignment(makeAssignment(runID, "two_step", twoStepWorkflow))
	fake.WaitForCond(t, 10*time.Second, func() bool { return runHasEventOfType(fake, runID, "RunCompleted") })

	cancel()
	waitAgent(t, errCh)
}

func TestAgent_SequentialAssignments(t *testing.T) {
	requireNoGoroutineLeak(t)
	fake := applytest.New(t)
	cancel, errCh := startTestAgent(t, fake)
	defer cancel()

	fake.WaitForCond(t, 5*time.Second, func() bool { return fake.ControlAttachCount() > 0 })

	// First assignment uses a long-running step so we can queue the second one
	// before the first finishes. The second assignment should not start until
	// the first one is no longer active, proving one-at-a-time execution.
	firstID := uuid.NewString()
	fake.QueueAssignment(makeAssignment(firstID, "cancel_test", cancelWorkflow))
	fake.WaitForCond(t, 5*time.Second, func() bool { return fake.HasStepEntered("step_two") })

	secondID := uuid.NewString()
	fake.QueueAssignment(makeAssignment(secondID, "two_step", twoStepWorkflow))

	fake.CancelRun(firstID, "test sequential cleanup")
	fake.WaitForCond(t, 10*time.Second, func() bool {
		return runEventIndex(fake, secondID, "RunStarted") >= 0
	})
	fake.WaitForCond(t, 10*time.Second, func() bool { return runHasEventOfType(fake, secondID, "RunCompleted") })

	firstStartIdx := runEventIndex(fake, firstID, "RunStarted")
	secondStartIdx := runEventIndex(fake, secondID, "RunStarted")
	require.Less(t, firstStartIdx, secondStartIdx, "first run should start before second")

	cancel()
	waitAgent(t, errCh)
}

func TestAgent_Cancellation(t *testing.T) {
	requireNoGoroutineLeak(t)
	fake := applytest.New(t)
	cancel, errCh := startTestAgent(t, fake)
	defer cancel()

	fake.WaitForCond(t, 5*time.Second, func() bool { return fake.ControlAttachCount() > 0 })

	runID := uuid.NewString()
	fake.QueueAssignment(makeAssignment(runID, "cancel_test", cancelWorkflow))
	fake.WaitForCond(t, 5*time.Second, func() bool { return fake.HasStepEntered("step_two") })

	fake.CancelRun(runID, "test cancel")
	fake.WaitForCond(t, 10*time.Second, func() bool {
		return runTerminalEventIndex(fake, runID) >= 0
	})

	cancel()
	waitAgent(t, errCh)
}

func TestAgent_Resume(t *testing.T) {
	requireNoGoroutineLeak(t)
	fake := applytest.New(t)
	cancel, errCh := startTestAgent(t, fake)
	defer cancel()

	fake.WaitForCond(t, 5*time.Second, func() bool { return fake.ControlAttachCount() > 0 })

	runID := uuid.NewString()
	fake.QueueAssignment(makeAssignment(runID, "pause_resume", pauseResumeWorkflow))
	fake.WaitForCond(t, 5*time.Second, func() bool { return runHasEventOfType(fake, runID, "WaitEntered") })

	fake.ResumeRun(runID, "resume")
	fake.WaitForCond(t, 10*time.Second, func() bool { return runHasEventOfType(fake, runID, "RunCompleted") })

	cancel()
	waitAgent(t, errCh)
}

func TestAgent_Reconnect(t *testing.T) {
	requireNoGoroutineLeak(t)
	fake := applytest.New(t)
	cancel, errCh := startTestAgent(t, fake)
	defer cancel()

	fake.WaitForCond(t, 5*time.Second, func() bool { return fake.ControlAttachCount() > 0 })
	require.Equal(t, 1, fake.RegistrationCount(), "agent should register exactly once on startup")

	fake.DropControl()
	fake.WaitForCond(t, 10*time.Second, func() bool { return fake.ControlAttachCount() >= 2 })
	require.Equal(t, 1, fake.RegistrationCount(), "agent must not re-register after reconnect")

	runID := uuid.NewString()
	fake.QueueAssignment(makeAssignment(runID, "two_step", twoStepWorkflow))
	fake.WaitForCond(t, 10*time.Second, func() bool { return runHasEventOfType(fake, runID, "RunCompleted") })

	cancel()
	waitAgent(t, errCh)
}

// invalidWorkflowSource is workflow HCL that cannot be parsed, exercising the
// compile-time failure path in agent assignments.
const invalidWorkflowSource = `
this is not valid criteria workflow source
`

func TestAgent_CompileFailureReportsRunFailed(t *testing.T) {
	requireNoGoroutineLeak(t)
	fake := applytest.New(t)
	cancel, errCh := startTestAgent(t, fake)
	defer cancel()

	fake.WaitForCond(t, 5*time.Second, func() bool { return fake.ControlAttachCount() > 0 })

	runID := uuid.NewString()
	fake.QueueAssignment(makeAssignment(runID, "invalid", invalidWorkflowSource))
	fake.WaitForCond(t, 10*time.Second, func() bool { return runHasEventOfType(fake, runID, "RunFailed") })

	// The agent must remain available for a subsequent assignment after reporting
	// the terminal compile failure.
	nextID := uuid.NewString()
	fake.QueueAssignment(makeAssignment(nextID, "two_step", twoStepWorkflow))
	fake.WaitForCond(t, 10*time.Second, func() bool { return runHasEventOfType(fake, nextID, "RunCompleted") })

	cancel()
	waitAgent(t, errCh)
}

func TestAgent_InitFailureReportsRunFailed(t *testing.T) {
	requireNoGoroutineLeak(t)
	fake := applytest.New(t)

	// Point the agent at a non-existent var-file so that run initialization
	// (mergeVarSources inside buildAgentRun) fails before execution starts.
	badVarFile := filepath.Join(t.TempDir(), "missing.json")
	cancel, errCh := startTestAgentOpts(t, fake, &agentOptions{
		varFiles: []string{badVarFile},
	})
	defer cancel()

	fake.WaitForCond(t, 5*time.Second, func() bool { return fake.ControlAttachCount() > 0 })

	runID := uuid.NewString()
	fake.QueueAssignment(makeAssignment(runID, "two_step", twoStepWorkflow))
	fake.WaitForCond(t, 10*time.Second, func() bool { return runHasEventOfType(fake, runID, "RunFailed") })

	// A second assignment must still be processed, proving the agent is still
	// accepting work after the terminal initialization failure.
	nextID := uuid.NewString()
	fake.QueueAssignment(makeAssignment(nextID, "two_step", twoStepWorkflow))
	fake.WaitForCond(t, 10*time.Second, func() bool { return runHasEventOfType(fake, nextID, "RunFailed") })

	cancel()
	waitAgent(t, errCh)
}

func TestAgent_CompileFailure_DuplicateTerminalEventAfterReconnect(t *testing.T) {
	requireNoGoroutineLeak(t)
	fake := applytest.New(t)
	cancel, errCh := startTestAgent(t, fake)
	defer cancel()

	fake.WaitForCond(t, 5*time.Second, func() bool { return fake.ControlAttachCount() > 0 })

	runID := uuid.NewString()
	fake.QueueAssignment(makeAssignment(runID, "invalid", invalidWorkflowSource))
	fake.WaitForCond(t, 10*time.Second, func() bool { return runHasEventOfType(fake, runID, "RunFailed") })

	// Force a transport-level reconnect and wait for the control stream to
	// reattach. The publisher's replay/dedup must not produce a second RunFailed
	// for the same run id.
	fake.DropControl()
	fake.WaitForCond(t, 10*time.Second, func() bool { return fake.ControlAttachCount() >= 2 })

	require.Equal(t, 1, runEventCountOfType(fake, runID, "RunFailed"), "reconnect must not duplicate RunFailed")

	cancel()
	waitAgent(t, errCh)
}

// crashRecoveryWorkflow has a single slow initial step so a test can cancel
// the agent after the step checkpoint has been written but before the run
// reaches a terminal state.
const crashRecoveryWorkflow = `
workflow {
  name          = "crash_recovery"
  version       = "0.1"
  initial_state = "slow"
  target_state  = "done"
}

adapter "noop" "default" {}

step "slow" {
  target = adapter.noop.default
  input { delay_ms = "2000" }
  outcome "success" { next = step.fast }
}

step "fast" {
  target = adapter.noop.default
  outcome "success" { next = step.done }
}

state "done" {
  terminal = true
  success  = true
}
`

// TestAgent_CrashRecovery_DeterministicResume verifies CRI-67: a container
// exit after a lease has been delivered and RunStarted sent, but before the
// terminal acknowledgement, recovers deterministically without executing the
// workflow twice. The restarted agent reattaches to the server, resumes from
// the persisted step checkpoint, and returns to the idle assignment loop.
func TestAgent_CrashRecovery_DeterministicResume(t *testing.T) {
	requireNoGoroutineLeak(t)
	fake := applytest.New(t)
	stateDir := t.TempDir()

	cancel1, errCh1 := startAgentWithStateDir(t, fake, stateDir)
	fake.WaitForCond(t, 5*time.Second, func() bool { return fake.ControlAttachCount() > 0 })

	runID := uuid.NewString()
	fake.QueueAssignment(makeAssignment(runID, "crash_recovery", crashRecoveryWorkflow))
	fake.WaitForCond(t, 5*time.Second, func() bool { return fake.HasStepEntered("slow") })

	// Simulate container exit: cancel the agent context before the slow step
	// finishes. Local run state and the step checkpoint must survive.
	cancel1()
	waitAgent(t, errCh1)

	require.Eventually(t, func() bool {
		_, err := readLocalRunState(runID)
		return err == nil
	}, 2*time.Second, 50*time.Millisecond, "run state should be persisted after crash")
	require.Eventually(t, func() bool {
		cp, err := readStepCheckpoint(runID)
		return err == nil && cp != nil && cp.CurrentStep == "slow"
	}, 2*time.Second, 50*time.Millisecond, "step checkpoint should be persisted after crash")

	// Simulate a restarted container: the server still reports the run as
	// in-flight at the slow step, and the new agent uses the same state dir.
	fake.SetReattachState(runID, "running", "slow", 1, "", "")
	cancel2, errCh2 := startAgentWithStateDir(t, fake, stateDir)
	defer cancel2()
	fake.WaitForCond(t, 10*time.Second, func() bool { return runHasEventOfType(fake, runID, "RunCompleted") })

	// The workflow must not restart from the beginning: only one RunStarted
	// and the slow step must execute exactly once across the crash boundary.
	require.Equal(t, 1, runEventCountOfType(fake, runID, "RunStarted"), "RunStarted must be emitted exactly once")
	require.Equal(t, 2, stepEnteredCount(fake, runID, "slow"), "slow step should be entered once before crash and once during resume")
	require.Equal(t, 0, runEventCountOfType(fake, runID, "RunFailed"), "agent shutdown must not report a terminal failure")

	// After recovery the agent must be back in its idle loop and able to accept
	// a brand new assignment.
	nextID := uuid.NewString()
	fake.QueueAssignment(makeAssignment(nextID, "two_step", twoStepWorkflow))
	fake.WaitForCond(t, 10*time.Second, func() bool { return runHasEventOfType(fake, nextID, "RunCompleted") })

	cancel2()
	waitAgent(t, errCh2)
}

// TestAgent_DuplicateDelivery_Idempotent verifies CRI-67: re-delivery of the
// same assignment while it is queued, running, and terminal is declined without
// re-executing the workflow.
func TestAgent_DuplicateDelivery_Idempotent(t *testing.T) {
	requireNoGoroutineLeak(t)
	fake := applytest.New(t)
	cancel, errCh := startTestAgent(t, fake)
	defer cancel()

	fake.WaitForCond(t, 5*time.Second, func() bool { return fake.ControlAttachCount() > 0 })

	// First assignment occupies the agent with a long-running step.
	firstID := uuid.NewString()
	fake.QueueAssignment(makeAssignment(firstID, "cancel_test", cancelWorkflow))
	fake.WaitForCond(t, 5*time.Second, func() bool { return fake.HasStepEntered("step_two") })

	// Second assignment is queued behind the first. Re-deliver it while queued.
	secondID := uuid.NewString()
	secondAssignment := makeAssignment(secondID, "two_step", twoStepWorkflow)
	fake.QueueAssignment(secondAssignment)
	fake.QueueAssignment(secondAssignment)

	// Re-deliver the first assignment while it is running.
	fake.QueueAssignment(makeAssignment(firstID, "cancel_test", cancelWorkflow))

	fake.CancelRun(firstID, "finish queued duplicate test")
	fake.WaitForCond(t, 10*time.Second, func() bool { return runHasEventOfType(fake, secondID, "RunCompleted") })

	// The second run must have started exactly once despite the queued duplicate.
	require.Equal(t, 1, runEventCountOfType(fake, secondID, "RunStarted"), "queued duplicate must not start a second run")

	// Re-deliver the now-terminal second assignment.
	fake.QueueAssignment(secondAssignment)
	// Wait deterministically for any ill-behaved duplicate execution.
	requireStableEventCount(t, fake, secondID, "RunStarted", 1, 500*time.Millisecond)
	require.Equal(t, 1, runEventCountOfType(fake, secondID, "RunCompleted"), "terminal run must remain terminal")

	cancel()
	waitAgent(t, errCh)
}

// TestAgent_CrashRecovery_TerminalDuplicateDeclined verifies CRI-67: when a
// crashed run is already terminal on the server, the recovered agent declines
// the duplicate assignment deterministically and does not execute the
// workflow again.
func TestAgent_CrashRecovery_TerminalDuplicateDeclined(t *testing.T) {
	requireNoGoroutineLeak(t)
	fake := applytest.New(t)
	stateDir := t.TempDir()

	cancel1, errCh1 := startAgentWithStateDir(t, fake, stateDir)
	fake.WaitForCond(t, 5*time.Second, func() bool { return fake.ControlAttachCount() > 0 })

	runID := uuid.NewString()
	assignment := makeAssignment(runID, "crash_recovery", crashRecoveryWorkflow)
	fake.QueueAssignment(assignment)
	fake.WaitForCond(t, 5*time.Second, func() bool { return fake.HasStepEntered("slow") })

	cancel1()
	waitAgent(t, errCh1)

	// The server considers the run already terminal.
	fake.SetReattachState(runID, "succeeded", "", 0, "", "")

	cancel2, errCh2 := startAgentWithStateDir(t, fake, stateDir)
	defer cancel2()
	fake.WaitForCond(t, 5*time.Second, func() bool { return fake.ControlAttachCount() > 0 })

	// The agent should be back in its idle loop. Delivering the same assignment
	// again must be declined; no new RunStarted is emitted.
	fake.QueueAssignment(assignment)
	requireStableEventCount(t, fake, runID, "RunStarted", 1, 500*time.Millisecond)

	cancel2()
	waitAgent(t, errCh2)
}

func TestAgent_AuthFailureRejected(t *testing.T) {
	requireNoGoroutineLeak(t)
	fake := applytest.New(t)
	fake.RequireAuthToken("wrong-token")

	cancel, errCh := startTestAgent(t, fake)
	defer cancel()

	// Registration itself is unauthenticated; the agent should register once
	// and then fail when it tries to open the authenticated control stream.
	fake.WaitForCond(t, 5*time.Second, func() bool { return fake.RegistrationCount() > 0 })

	select {
	case err := <-errCh:
		require.Error(t, err, "agent should exit with an error when authentication fails")
	case <-time.After(10 * time.Second):
		// If runAgent did not return, ensure it never started an assignment.
		require.False(t, fake.HasEventOfType("RunStarted"), "auth failure should prevent assignment execution")
		cancel()
		waitAgent(t, errCh)
	}
}
