package cli

// agent_pause_test.go — CRI-254 agent-loop pause routing tests. Pins the
// three handlePause outcomes: delivery to the active run's pause channel, a
// structured run_not_running drop for a pause addressed to a non-active run
// (detectable, never silent — castle's ack timeout sees it), and the
// matched-but-not-routed drop when the run's pause channel is full (the
// router is still processing the previous latch; mid-execution pauses land
// idempotently at the next boundary).

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"

	"github.com/brokenbots/criteria/internal/cli/applytest"
	servertrans "github.com/brokenbots/criteria/internal/transport/server"
)

func pauseRoutingLoop(t *testing.T, runID string, pauseBuf int) (*agentLoop, chan *pb.PauseRun, *bytes.Buffer) {
	t.Helper()
	ch := make(chan *pb.PauseRun, pauseBuf)
	a := &activeRun{}
	a.mu.Lock()
	a.runID = runID
	a.pauseCh = ch
	a.mu.Unlock()
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return &agentLoop{log: logger, active: a}, ch, &logBuf
}

// TestAgentLoop_PauseRoutedToActiveRun covers the matching path: the pause
// lands on the active run's pause channel and the routing is logged.
func TestAgentLoop_PauseRoutedToActiveRun(t *testing.T) {
	l, ch, logBuf := pauseRoutingLoop(t, "run-1", 1)

	l.handlePause(&pb.PauseRun{RunId: "run-1", Reason: "castle hold"})

	select {
	case got := <-ch:
		if got.GetRunId() != "run-1" || got.GetReason() != "castle hold" {
			t.Fatalf("routed pause = %+v, want the sent message", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pause never reached the active run's pause channel")
	}
	if !strings.Contains(logBuf.String(), "routing pause to active run") {
		t.Fatalf("routing log missing:\n%s", logBuf.String())
	}
}

// TestAgentLoop_PauseForInactiveRunRecordedNotRunning pins the CRI-62 rule at
// the agent-loop layer: a pause addressed to a non-active run is dropped with
// the structured run_not_running reason and is never delivered to another
// run's channel.
func TestAgentLoop_PauseForInactiveRunRecordedNotRunning(t *testing.T) {
	l, ch, logBuf := pauseRoutingLoop(t, "run-1", 1)

	l.handlePause(&pb.PauseRun{RunId: "run-other", Reason: "hold"})

	select {
	case got := <-ch:
		t.Fatalf("cross-run delivery: pause %+v reached the active run's channel", got)
	default:
	}
	for _, want := range []string{"drop_reason=run_not_running", "run-other"} {
		if !strings.Contains(logBuf.String(), want) {
			t.Fatalf("run_not_running record missing %q:\n%s", want, logBuf.String())
		}
	}
}

// TestAgentLoop_PauseFullBufferRecordedDrop pins the slot-full path: when the
// run's pause channel is full (the router pump is busy), the drop is logged
// with the active_run full reason — the pause is not lost when the latch is
// already armed, and the log keeps the outcome detectable.
func TestAgentLoop_PauseFullBufferRecordedDrop(t *testing.T) {
	l, ch, logBuf := pauseRoutingLoop(t, "run-1", 1)

	// Pre-fill the slot: the router pump is busy processing this one.
	queued := &pb.PauseRun{RunId: "run-1", Reason: "in flight"}
	ch <- queued

	l.handlePause(&pb.PauseRun{RunId: "run-1", Reason: "double"})

	for _, want := range []string{"drop_reason=active_run_pause_ch_full", "active_run_pause_ch_full"} {
		if !strings.Contains(logBuf.String(), want) {
			t.Fatalf("slot-full record missing %q:\n%s", want, logBuf.String())
		}
	}
	// The queued pause was left untouched (not overwritten or corrupted).
	if got := <-ch; got.GetReason() != "in flight" {
		t.Fatalf("queued pause = %+v, want the original message", got)
	}
	if strings.Count(logBuf.String(), "active_run_pause_ch_full") != 1 {
		t.Fatalf("exactly one slot-full record expected:\n%s", logBuf.String())
	}
}

// TestAgentRunAndDrain_PauseLandsDuringInitialRun is the run-level CRI-254
// regression for the agent path: the pause consumer must be live BEFORE the
// initial engine run, so a castle PauseRun delivered (through the real
// control stream and the agentLoop router) while the run is in flight lands
// at the next boundary instead of racing past a terminal run. Against the
// pre-fix ordering — consumer started after eng.Run — the pause sits
// unconsumed, the run completes, and the late consumer drops it as
// run_not_running: the run would never reach paused status and this test
// would time out waiting for it.
func TestAgentRunAndDrain_PauseLandsDuringInitialRun(t *testing.T) {
	requireNoGoroutineLeak(t)
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())
	fake := applytest.New(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	log := newApplyLogger()
	wfPath := writeWorkflowFile(t, boundaryPauseWorkflow)
	src, graph, loader, err := compileForExecution(ctx, wfPath, log, false, false)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	defer func() { _ = loader.Shutdown(context.WithoutCancel(ctx)) }()

	copts := servertrans.Options{TLSMode: servertrans.TLSDisable}
	client, runID, resumed, _, err := setupServerRun(ctx, log, graph, src, fake.URL(), "cri254-agent", &copts, nil, nil, "")
	if err != nil {
		t.Fatalf("setupServerRun: %v", err)
	}
	if resumed {
		t.Fatal("setupServerRun unexpectedly resumed a matching checkpoint")
	}
	defer client.Close()

	publisher, closePublisher, err := newRunPublisher(ctx, client, runID)
	if err != nil {
		t.Fatalf("newRunPublisher: %v", err)
	}
	defer closePublisher()

	agentOpts := &agentOptions{serverURL: fake.URL()}
	assignment := &pb.WorkflowAssignment{RunId: runID, WorkflowSource: string(src)}
	eng, sink, runSink, state, err := buildAgentRun(ctx, ctx, log, client, assignment, agentOpts, publisher, graph, loader, filepath.Dir(wfPath), wfPath)
	if err != nil {
		t.Fatalf("buildAgentRun: %v", err)
	}

	resumeCh := make(chan *pb.ResumeRun, 1)
	promptCh := make(chan *pb.AgentPrompt, 1)
	pauseCh := make(chan *pb.PauseRun, 1)
	active := &activeRun{}
	active.mu.Lock()
	active.runID = runID
	active.resumeCh = resumeCh
	active.promptCh = promptCh
	active.pauseCh = pauseCh
	active.mu.Unlock()

	// Route control messages through the same primitives agent_pause_test.go
	// covers, wired to the real client so pause/resume cross the network leg.
	loop := &agentLoop{ctx: ctx, log: log, client: client, opts: agentOpts, active: active}
	routingDone := make(chan struct{})
	go func() {
		defer close(routingDone)
		for {
			select {
			case <-ctx.Done():
				return
			case msg := <-client.PauseRunCh():
				loop.handlePause(msg)
			case msg := <-client.ResumeCh():
				loop.handleResume(msg)
			}
		}
	}()

	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()
	runErrCh := make(chan error, 1)
	go func() {
		runErrCh <- runAndDrain(ctx, runCtx, log, eng, loader, sink, runSink, resumeCh, promptCh, pauseCh, state, graph, filepath.Dir(wfPath), runID, publisher, nil, nil)
	}()

	fake.WaitForCond(t, 10*time.Second, func() bool {
		return fake.HasStepEntered("step_two")
	})
	fake.PauseRun(runID, "castle hold")

	// The pause lands at the step_three boundary while the initial run is
	// still in flight; the RunPaused ack flips the fake run row to paused.
	fake.WaitForCond(t, 10*time.Second, func() bool {
		return fake.RunStatus(runID) == "paused"
	})
	var rp *pb.RunPaused
	for _, env := range fake.Events() {
		if rpEnv := env.GetRunPaused(); rpEnv != nil {
			rp = rpEnv
			break
		}
	}
	if rp == nil {
		t.Fatal("RunPaused event not received castle-side")
	}
	if rp.Node != "step_three" || rp.Mode != "external" || rp.Signal != "" {
		t.Fatalf("RunPaused: node=%q mode=%q signal=%q, want step_three/external/\"\"", rp.Node, rp.Mode, rp.Signal)
	}
	if fake.HasEventOfType("RunCompleted") {
		t.Fatal("run must stay in flight while paused")
	}
	if fake.HasStepEntered("step_three") {
		t.Fatal("the post-pause step must never evaluate before resume")
	}

	// Signal-less resume through the same control stream: continue from the
	// checkpoint at step_three without replaying step_two and complete.
	fake.ResumeRun(runID, "")
	fake.WaitForCond(t, 15*time.Second, func() bool {
		return fake.RunStatus(runID) == "succeeded"
	})
	select {
	case err := <-runErrCh:
		if err != nil {
			t.Fatalf("runAndDrain: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("timeout waiting for runAndDrain to return")
	}

	var stepTwo, stepThree int
	for _, env := range fake.Events() {
		if se := env.GetStepEntered(); se != nil {
			switch se.Step {
			case "step_two":
				stepTwo++
			case "step_three":
				stepThree++
			}
		}
	}
	if stepTwo != 1 || stepThree != 1 {
		t.Fatalf("step entries: step_two=%d step_three=%d, want 1/1 (resume continues from the checkpoint, no replay)", stepTwo, stepThree)
	}
	if fake.HasEventOfType("WaitResumed") {
		t.Fatal("a boundary pause must not surface wait/signal resume traffic")
	}
	cancel()
	<-routingDone
}

// agentDoublePauseWorkflow delays both step_two and step_three so each has a
// wide in-flight window; the two pauses of the resume-cycle regression below
// pause mid-step_two (landing at the step_three boundary) and mid-step_three
// of the resumed run (landing at the step_four boundary).
const agentDoublePauseWorkflow = `
workflow {
  name = "agent_double_pause"
  version       = "0.1"
  initial_state = "step_one"
  target_state  = "done"
}

adapter "noop" "default" {}

step "step_one" {
  target = adapter.noop.default
  outcome "success" { next = step.step_two }
  outcome "failure" { next = step.done }
}

step "step_two" {
  target = adapter.noop.default
  input { delay_ms = "500" }
  outcome "success" { next = step.step_three }
  outcome "failure" { next = step.done }
}

step "step_three" {
  target = adapter.noop.default
  input { delay_ms = "500" }
  outcome "success" { next = step.step_four }
  outcome "failure" { next = step.done }
}

step "step_four" {
  target = adapter.noop.default
  outcome "success" { next = step.done }
  outcome "failure" { next = step.done }
}

state "done" {
  terminal = true
  success  = true
}
`

// TestAgentRunAndDrain_PauseLandsDuringResumeCycle is the run-level CRI-254
// regression for the resume-cycle window: after a first pause and a
// signal-less resume, the router must be pinned to the RESUMED engine before
// its RunFrom starts, so a second castle PauseRun delivered while the resumed
// run is executing lands at the next boundary (RunPaused Ack) rather than
// being dropped against the previous engine whose loop exited at the first
// pause. Against the pre-fix ordering — setEngine after RunFrom — the second
// pause resolves the stale engine, RequestPause reports not-running and the
// command is dropped as run_not_running: the resumed run completes and no
// RunPaused(step_four) ever arrives, failing the wait below.
func TestAgentRunAndDrain_PauseLandsDuringResumeCycle(t *testing.T) {
	requireNoGoroutineLeak(t)
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())
	fake := applytest.New(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	log := newApplyLogger()
	wfPath := writeWorkflowFile(t, agentDoublePauseWorkflow)
	src, graph, loader, err := compileForExecution(ctx, wfPath, log, false, false)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	defer func() { _ = loader.Shutdown(context.WithoutCancel(ctx)) }()

	copts := servertrans.Options{TLSMode: servertrans.TLSDisable}
	client, runID, resumed, _, err := setupServerRun(ctx, log, graph, src, fake.URL(), "cri254-agent", &copts, nil, nil, "")
	if err != nil {
		t.Fatalf("setupServerRun: %v", err)
	}
	if resumed {
		t.Fatal("setupServerRun unexpectedly resumed a matching checkpoint")
	}
	defer client.Close()

	publisher, closePublisher, err := newRunPublisher(ctx, client, runID)
	if err != nil {
		t.Fatalf("newRunPublisher: %v", err)
	}
	defer closePublisher()

	agentOpts := &agentOptions{serverURL: fake.URL()}
	assignment := &pb.WorkflowAssignment{RunId: runID, WorkflowSource: string(src)}
	eng, sink, runSink, state, err := buildAgentRun(ctx, ctx, log, client, assignment, agentOpts, publisher, graph, loader, filepath.Dir(wfPath), wfPath)
	if err != nil {
		t.Fatalf("buildAgentRun: %v", err)
	}

	resumeCh := make(chan *pb.ResumeRun, 1)
	promptCh := make(chan *pb.AgentPrompt, 1)
	pauseCh := make(chan *pb.PauseRun, 1)
	active := &activeRun{}
	active.mu.Lock()
	active.runID = runID
	active.resumeCh = resumeCh
	active.promptCh = promptCh
	active.pauseCh = pauseCh
	active.mu.Unlock()

	loop := &agentLoop{ctx: ctx, log: log, client: client, opts: agentOpts, active: active}
	routingDone := make(chan struct{})
	go func() {
		defer close(routingDone)
		for {
			select {
			case <-ctx.Done():
				return
			case msg := <-client.PauseRunCh():
				loop.handlePause(msg)
			case msg := <-client.ResumeCh():
				loop.handleResume(msg)
			}
		}
	}()

	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()
	runErrCh := make(chan error, 1)
	go func() {
		runErrCh <- runAndDrain(ctx, runCtx, log, eng, loader, sink, runSink, resumeCh, promptCh, pauseCh, state, graph, filepath.Dir(wfPath), runID, publisher, nil, nil)
	}()

	// First pause mid-step_two of the initial run; it lands at the step_three
	// boundary exactly as the initial-run regression pins.
	fake.WaitForCond(t, 10*time.Second, func() bool {
		return fake.HasStepEntered("step_two")
	})
	fake.PauseRun(runID, "castle hold 1")
	fake.WaitForCond(t, 10*time.Second, func() bool {
		return fake.RunStatus(runID) == "paused"
	})
	var first *pb.RunPaused
	for _, env := range fake.Events() {
		if rpEnv := env.GetRunPaused(); rpEnv != nil {
			first = rpEnv
			break
		}
	}
	if first == nil {
		t.Fatal("RunPaused event not received castle-side after the first pause")
	}
	if first.Node != "step_three" || first.Mode != "external" || first.Signal != "" {
		t.Fatalf("first RunPaused: node=%q mode=%q signal=%q, want step_three/external/\"\"", first.Node, first.Mode, first.Signal)
	}

	// Signal-less resume: the resumed run enters step_three and holds there
	// (500 ms delay), keeping it in flight for the second pause.
	fake.ResumeRun(runID, "")
	fake.WaitForCond(t, 15*time.Second, func() bool {
		return fake.HasStepEntered("step_three")
	})
	fake.PauseRun(runID, "castle hold 2")

	var second *pb.RunPaused
	deadline := time.Now().Add(15 * time.Second)
	for second == nil && time.Now().Before(deadline) {
		if fake.HasEventOfType("RunCompleted") {
			t.Fatal("the resumed run completed with a pause pending — the pause was dropped against the stale engine, not landed")
		}
		for _, env := range fake.Events() {
			if rpEnv := env.GetRunPaused(); rpEnv != nil && rpEnv.Node == "step_four" {
				second = rpEnv
			}
		}
		if second == nil {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if second == nil {
		t.Fatal("no RunPaused(step_four) within the ack budget; the pause delivered during the resumed run did not land at the boundary")
	}
	if second.Mode != "external" || second.Signal != "" {
		t.Fatalf("second RunPaused: mode=%q signal=%q, want external/\"\"", second.Mode, second.Signal)
	}
	if fake.HasStepEntered("step_four") {
		t.Fatal("the post-pause step must never evaluate before the second resume")
	}

	// Second signal-less resume: continue from the step_four checkpoint and
	// complete; every step is entered exactly once (no replay through either
	// pause).
	fake.ResumeRun(runID, "")
	fake.WaitForCond(t, 15*time.Second, func() bool {
		return fake.RunStatus(runID) == "succeeded"
	})
	select {
	case err := <-runErrCh:
		if err != nil {
			t.Fatalf("runAndDrain: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("timeout waiting for runAndDrain to return")
	}

	entries := map[string]int{}
	for _, env := range fake.Events() {
		if se := env.GetStepEntered(); se != nil {
			entries[se.Step]++
		}
	}
	for _, step := range []string{"step_two", "step_three", "step_four"} {
		if entries[step] != 1 {
			t.Fatalf("step entries: %s=%d, want 1 (resume continues from the checkpoint, no replay)", step, entries[step])
		}
	}
	if fake.HasEventOfType("WaitResumed") {
		t.Fatal("a boundary pause must not surface wait/signal resume traffic")
	}
	cancel()
	<-routingDone
}
