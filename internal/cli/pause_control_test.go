package cli

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/internal/engine"
	"github.com/brokenbots/criteria/internal/run"
	"github.com/brokenbots/criteria/workflow"
)

// CRI-254 router unit tests: controlPauseRouter.handle must turn one consumed
// pause_run command into an observable ack — RunPaused at the boundary, an
// idempotent already-paused answer, or a structured drop / retryable timeout
// (CRI-62). The engine here is a real one driving a gate-blocked step so the
// pause latch arms mid-step and lands at the next checkpoint boundary, exactly
// like the local-control path (drain-first, durable checkpoint, no adapter
// kill).

// routerPauseHCL is a linear three-step workflow: a -> b -> c -> done. The
// "gate" adapter lets tests block a step mid-flight.
const routerPauseHCL = `
workflow {
  name = "router_pause"
  version       = "0.1"
  initial_state = "a"
  target_state  = "done"
}

adapter "gate" "default" {}

step "a" {
  target = adapter.gate.default
  outcome "success" { next = step.b }
}
step "b" {
  target = adapter.gate.default
  outcome "success" { next = step.c }
}
step "c" {
  target = adapter.gate.default
  outcome "success" { next = step.done }
}
state "done" { terminal = true }`

// pauseGateHandle blocks select steps mid-flight so a pause latch can be
// armed while a step is in flight; steps return success after their gate
// opens (mirrors the engine package's gateHandle).
type pauseGateHandle struct {
	mu      sync.Mutex
	blocked map[string]chan struct{}
}

func (g *pauseGateHandle) block(step string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.blocked == nil {
		g.blocked = map[string]chan struct{}{}
	}
	g.blocked[step] = make(chan struct{})
}

func (g *pauseGateHandle) release(step string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if ch, ok := g.blocked[step]; ok {
		close(ch)
		delete(g.blocked, step)
	}
}

func (g *pauseGateHandle) Info(context.Context) (adapterhost.Info, error) {
	return adapterhost.Info{Name: "gate"}, nil
}
func (g *pauseGateHandle) OpenSession(context.Context, string, map[string]string, map[string]string) error {
	return nil
}
func (g *pauseGateHandle) Execute(ctx context.Context, _ string, node *workflow.StepNode, _ adapter.EventSink, rejection *v2.ExecutionRejection) (adapter.Result, error) {
	g.mu.Lock()
	ch, ok := g.blocked[node.Name]
	g.mu.Unlock()
	if ok {
		<-ch
		if ctx.Err() != nil {
			return adapter.Result{}, ctx.Err()
		}
	}
	return adapter.Result{Outcome: "success"}, nil
}
func (g *pauseGateHandle) Permit(context.Context, string, string, bool, string) error { return nil }
func (g *pauseGateHandle) CloseSession(context.Context, string) error                 { return nil }
func (g *pauseGateHandle) Kill()                                                      {}
func (g *pauseGateHandle) Pause(context.Context, string) error                        { return nil }
func (g *pauseGateHandle) Resume(context.Context, string) error                       { return nil }
func (g *pauseGateHandle) Inspect(context.Context, string) (*v2.InspectResponse, error) {
	return &v2.InspectResponse{}, nil
}
func (g *pauseGateHandle) Snapshot(context.Context, string) (*v2.SnapshotResponse, error) {
	return &v2.SnapshotResponse{}, nil
}
func (g *pauseGateHandle) Restore(context.Context, string, []byte, uint32) error { return nil }

type staticGateLoader struct{ handle adapterhost.Handle }

func (l *staticGateLoader) Resolve(context.Context, string) (adapterhost.Handle, error) {
	return l.handle, nil
}
func (l *staticGateLoader) Shutdown(context.Context) error { return nil }

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// eventPublisher captures envelopes published through a run.Sink and surfaces
// the RunPaused ack.
type eventPublisher struct {
	mu   sync.Mutex
	envs []*pb.Envelope
}

func (p *eventPublisher) Publish(_ context.Context, env *pb.Envelope) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.envs = append(p.envs, env)
}

func (p *eventPublisher) runPaused() *pb.RunPaused {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := len(p.envs) - 1; i >= 0; i-- {
		if rp := p.envs[i].GetRunPaused(); rp != nil {
			return rp
		}
	}
	return nil
}

func (p *eventPublisher) entered(step string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, env := range p.envs {
		if se := env.GetStepEntered(); se != nil && se.Step == step {
			return true
		}
	}
	return false
}

func waitStepEntered(t *testing.T, p *eventPublisher, step string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if p.entered(step) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for StepEntered %q", step)
}

func waitRunPaused(t *testing.T, p *eventPublisher, d time.Duration) *pb.RunPaused {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if rp := p.runPaused(); rp != nil {
			return rp
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timeout waiting for RunPaused event")
	return nil
}

func compileRouterGraph(t *testing.T, src string) *workflow.FSMGraph {
	t.Helper()
	spec, diags := workflow.Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := workflow.Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	return g
}

// TestControlPauseRouter_LandsAtBoundaryAndAcks holds step "b" mid-flight and
// lands a pause: the router waits for the boundary, the sink publishes the
// RunPaused ack (mode "external", resume point "c"), and the run loop yields
// without evaluating step "c". The latch is primed directly through the
// engine's idempotent RequestPause before the gate opens, so the router's own
// RequestPause inside handle observes the identical pending request and the
// landing is deterministic.
func TestControlPauseRouter_LandsAtBoundaryAndAcks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	g := compileRouterGraph(t, routerPauseHCL)
	gate := &pauseGateHandle{}
	publisher := &eventPublisher{}
	sink := &run.Sink{RunID: "run-pause-1", Client: publisher, Log: discardLog()}
	eng := engine.New(g, &staticGateLoader{handle: gate}, sink)

	router := newControlPauseRouter("run-pause-1", sink, discardLog())
	router.setEngine(eng)

	gate.block("b")
	doneRun := make(chan error, 1)
	go func() { doneRun <- eng.Run(ctx) }()

	waitStepEntered(t, publisher, "b")
	if _, ok := eng.RequestPause(); !ok {
		t.Fatal("priming RequestPause returned not-ok while step b was in flight")
	}

	resCh := make(chan pauseAckResult, 1)
	go func() { resCh <- router.handle(ctx, &pb.PauseRun{RunId: "run-pause-1", Reason: "castle hold"}) }()

	// With the latch armed the pause must land as soon as step "b" completes,
	// without evaluating step "c".
	gate.release("b")

	select {
	case res := <-resCh:
		if res != pauseAckLanded {
			t.Fatalf("pause ack: got %q, want %q", res, pauseAckLanded)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for the pause ack")
	}
	if node := sink.PausedAt(); node != "c" {
		t.Fatalf("paused node: got %q, want c", node)
	}
	rp := waitRunPaused(t, publisher, time.Second)
	if rp.Node != "c" || rp.Mode != "external" || rp.Signal != "" {
		t.Fatalf("RunPaused: node=%q mode=%q signal=%q, want c/external/\"\"", rp.Node, rp.Mode, rp.Signal)
	}
	if publisher.entered("c") {
		t.Fatal("step c must not evaluate after the pause lands")
	}

	select {
	case err := <-doneRun:
		if err != nil {
			t.Fatalf("engine run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for the paused run loop to yield")
	}
}

// TestControlPauseRouter_AlreadyPausedAcksIdempotently seeds a node pause and
// verifies a later pause_run acks with the recorded node instead of touching
// the engine (already-paused is acked, mirroring the local-control guard).
func TestControlPauseRouter_AlreadyPausedAcksIdempotently(t *testing.T) {
	publisher := &eventPublisher{}
	sink := &run.Sink{RunID: "run-pause-2", Client: publisher, Log: discardLog()}
	sink.OnRunPaused("approval", "signal", "resume")

	router := newControlPauseRouter("run-pause-2", sink, discardLog())
	router.setEngine(nil)

	res := router.handle(context.Background(), &pb.PauseRun{RunId: "run-pause-2"})
	if res != pauseAckLanded {
		t.Fatalf("pause ack: got %q, want %q (already paused)", res, pauseAckLanded)
	}
}

// TestControlPauseRouter_DropsForeignAndEmptyRunIDs pins the run_id guard:
// only the owning run is addressed; anything else is a structured drop.
func TestControlPauseRouter_DropsForeignAndEmptyRunIDs(t *testing.T) {
	publisher := &eventPublisher{}
	sink := &run.Sink{RunID: "run-pause-3", Client: publisher, Log: discardLog()}

	router := newControlPauseRouter("run-pause-3", sink, discardLog())
	router.setEngine(nil)

	if res := router.handle(context.Background(), &pb.PauseRun{RunId: "unrelated-run"}); res != pauseAckEnded {
		t.Fatalf("foreign run ack: got %q, want %q", res, pauseAckEnded)
	}
	if res := router.handle(context.Background(), &pb.PauseRun{}); res != pauseAckEnded {
		t.Fatalf("empty run ack: got %q, want %q", res, pauseAckEnded)
	}
	if rp := publisher.runPaused(); rp != nil {
		t.Fatalf("no RunPaused may be published for a dropped pause, got %+v", rp)
	}
}

// TestControlPauseRouter_NotRunningIsDetected verifies the not-running branch:
// an engine whose run loop is gone (never started / already wound down)
// answers RequestPause not-ok and the drop is logged with run_not_running —
// a detectable outcome, not a silence.
func TestControlPauseRouter_NotRunningIsDetected(t *testing.T) {
	g := compileRouterGraph(t, routerPauseHCL)
	publisher := &eventPublisher{}
	sink := &run.Sink{RunID: "run-pause-4", Client: publisher, Log: discardLog()}
	eng := engine.New(g, &staticGateLoader{handle: &pauseGateHandle{}}, sink)

	router := newControlPauseRouter("run-pause-4", sink, discardLog())
	router.setEngine(eng)

	res := router.handle(context.Background(), &pb.PauseRun{RunId: "run-pause-4"})
	if res != pauseAckEnded {
		t.Fatalf("pause ack: got %q, want %q", res, pauseAckEnded)
	}

	// nil engine provider (unattached router) drops too.
	detached := newControlPauseRouter("run-pause-4", sink, discardLog())
	if res := detached.handle(context.Background(), &pb.PauseRun{RunId: "run-pause-4"}); res != pauseAckEnded {
		t.Fatalf("detached router ack: got %q, want %q", res, pauseAckEnded)
	}
	if rp := publisher.runPaused(); rp != nil {
		t.Fatalf("no RunPaused may be published when the run is not running, got %+v", rp)
	}
}

// TestControlPauseRouter_AckTimeoutStaysRetryable shortens the ack budget so
// the timeout path fires while step "b" is still in flight: the router
// reports the retryable ack timeout with the latch still armed, and the
// later boundary landing still publishes the RunPaused ack.
func TestControlPauseRouter_AckTimeoutStaysRetryable(t *testing.T) {
	prevWait := pauseAckWait
	pauseAckWait = 75 * time.Millisecond
	defer func() { pauseAckWait = prevWait }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	g := compileRouterGraph(t, routerPauseHCL)
	gate := &pauseGateHandle{}
	publisher := &eventPublisher{}
	sink := &run.Sink{RunID: "run-pause-5", Client: publisher, Log: discardLog()}
	eng := engine.New(g, &staticGateLoader{handle: gate}, sink)

	router := newControlPauseRouter("run-pause-5", sink, discardLog())
	router.setEngine(eng)

	gate.block("b")
	doneRun := make(chan error, 1)
	go func() { doneRun <- eng.Run(ctx) }()
	waitStepEntered(t, publisher, "b")

	if res := router.handle(ctx, &pb.PauseRun{RunId: "run-pause-5", Reason: "hold"}); res != pauseAckTimed {
		t.Fatalf("pause ack: got %q, want %q", res, pauseAckTimed)
	}

	// The latch stays armed: releasing the step lets the pause land at "c"
	// and the ack is published late, exactly what a retrying orchestrator
	// needs to observe.
	gate.release("b")
	if rp := waitRunPaused(t, publisher, 5*time.Second); rp.Node != "c" || rp.Mode != "external" {
		t.Fatalf("late RunPaused: node=%q mode=%q, want c/external", rp.Node, rp.Mode)
	}
	select {
	case err := <-doneRun:
		if err != nil {
			t.Fatalf("engine run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for the paused run loop to yield")
	}
}

// TestControlPauseRouter_ConsumeDrainsUntilClose exercises the consume loop:
// foreign-run and empty-run commands are dropped (their ack outcome is
// observable only via logs) and the loop exits when the channel closes.
func TestControlPauseRouter_ConsumeDrainsUntilClose(t *testing.T) {
	publisher := &eventPublisher{}
	sink := &run.Sink{RunID: "run-pause-6", Client: publisher, Log: discardLog()}
	router := newControlPauseRouter("run-pause-6", sink, discardLog())

	pauseCh := make(chan *pb.PauseRun, 3)
	pauseCh <- &pb.PauseRun{RunId: "unrelated-run"}
	pauseCh <- &pb.PauseRun{}
	close(pauseCh)

	done := make(chan struct{})
	go func() { router.consume(context.Background(), pauseCh); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("consume did not exit after the pause channel closed")
	}
	if rp := publisher.runPaused(); rp != nil {
		t.Fatalf("no RunPaused may be published for dropped pauses, got %+v", rp)
	}
}

// pauseCancelHCL is a minimal linear workflow for the run-ended-without-pause
// regression: the first step is the only blocking point the test needs.
const pauseCancelHCL = `
workflow {
  name = "router_cancel"
  version       = "0.1"
  initial_state = "a"
  target_state  = "done"
}

adapter "gate" "default" {}

step "a" {
  target = adapter.gate.default
  outcome "success" { next = step.b }
}
step "b" {
  target = adapter.gate.default
  outcome "success" { next = step.c }
}
step "c" {
  target = adapter.gate.default
  outcome "success" { next = step.done }
}
state "done" { terminal = true }`

// TestControlPauseRouter_RunEndedWithoutPausingIsAcked pins the
// run_ended_without_pausing arm (CRI-62 no-silent-drop, CRI-254): the latch
// is armed while a step is in flight, the run loop exits without pausing
// (run-cancel teardown mid-step returns through clearPauseRequest, which
// closes the ack with no paused node), and the ack outcome must be
// run_ended — a detectable drop with no RunPaused published — never a fake
// "paused" ack.
func TestControlPauseRouter_RunEndedWithoutPausingIsAcked(t *testing.T) {
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())

	runCtx, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()

	g := compileRouterGraph(t, pauseCancelHCL)
	gate := &pauseGateHandle{}
	publisher := &eventPublisher{}
	sink := &run.Sink{RunID: "run-pause-7", Client: publisher, Log: discardLog()}
	eng := engine.New(g, &staticGateLoader{handle: gate}, sink)

	router := newControlPauseRouter("run-pause-7", sink, discardLog())
	router.setEngine(eng)

	gate.block("a")
	doneRun := make(chan error, 1)
	go func() { doneRun <- eng.Run(runCtx) }()
	waitStepEntered(t, publisher, "a")

	// Arm the latch mid-step, synchronously: with the gate holding the only
	// step, RequestPause cannot land and returns the ack channel. Waiting on
	// it directly is the deterministic way into the run_ended_without_pausing
	// arm — routing the command through handle would race the arm-taking
	// against the teardown below, and on an unlucky schedule the run_not_running
	// drop would satisfy the assertion without exercising this arm.
	ack, ok := eng.RequestPause()
	if !ok {
		t.Fatal("the gate holds the only step, so the pause latch must arm mid-step")
	}
	resCh := make(chan pauseAckResult, 1)
	go func() {
		resCh <- router.waitForPauseAck(context.Background(), ack, "castle hold")
	}()

	// The run is torn down mid-step: releasing the gate under a cancelled
	// run context makes the in-flight Execute return the context error and
	// the run loop exits without a pause; clearPauseRequest then closes the
	// ack with no paused node.
	cancelRun()
	gate.release("a")

	select {
	case res := <-resCh:
		if res != pauseAckEnded {
			t.Fatalf("ack for a run that ended without pausing: got %q, want %q", res, pauseAckEnded)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for the run-ended ack outcome")
	}
	if node := sink.PausedAt(); node != "" {
		t.Fatalf("PausedAt: got %q, want empty (the run ended without pausing)", node)
	}
	if rp := publisher.runPaused(); rp != nil {
		t.Fatalf("no RunPaused may be published when the run ends without pausing, got %+v", rp)
	}
	if err := <-doneRun; err == nil {
		t.Fatal("expected the cancelled mid-step run to end with an error")
	}
}

// TestControlPauseRouter_ConsumeCtxCancelledStaysRetryable exercises the
// context_cancelled arm: with the latch armed mid-step and the consumer's
// context torn down before any boundary, the wait reports the retryable
// ack-timeout outcome — the latch stays armed and the pause still lands late;
// nothing is silently dropped.
func TestControlPauseRouter_ConsumeCtxCancelledStaysRetryable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	g := compileRouterGraph(t, routerPauseHCL)
	gate := &pauseGateHandle{}
	publisher := &eventPublisher{}
	sink := &run.Sink{RunID: "run-pause-8", Client: publisher, Log: discardLog()}
	eng := engine.New(g, &staticGateLoader{handle: gate}, sink)

	router := newControlPauseRouter("run-pause-8", sink, discardLog())
	router.setEngine(eng)

	gate.block("b")
	doneRun := make(chan error, 1)
	go func() { doneRun <- eng.Run(ctx) }()
	waitStepEntered(t, publisher, "b")

	ack, ok := eng.RequestPause()
	if !ok {
		t.Fatal("RequestPause did not arm the latch while step b was in flight")
	}

	// Teardown arrives before any boundary: the consumer context is
	// cancelled while the latch is still pending.
	waitCtx, cancelWait := context.WithCancel(context.Background())
	waitDone := make(chan pauseAckResult, 1)
	go func() { waitDone <- router.waitForPauseAck(waitCtx, ack, "") }()
	cancelWait()

	select {
	case res := <-waitDone:
		if res != pauseAckTimed {
			t.Fatalf("cancelled pause wait: got %q, want %q", res, pauseAckTimed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for the cancelled pause wait")
	}

	// The latch stays armed: releasing the gate lands the pause at "c" and
	// the run loop yields (the late RunPaused is the retryable-landing case,
	// covered by TestControlPauseRouter_AckTimeoutStaysRetryable).
	gate.release("b")
	select {
	case err := <-doneRun:
		if err != nil {
			t.Fatalf("engine run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for the paused run loop to yield")
	}
}
