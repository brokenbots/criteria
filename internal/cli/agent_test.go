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

// makeAssignment builds a WorkflowAssignment for the given source HCL.
func makeAssignment(runID, workflowName, source string) *pb.WorkflowAssignment {
	return &pb.WorkflowAssignment{
		RunId:          runID,
		WorkflowName:   workflowName,
		WorkflowSource: source,
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
