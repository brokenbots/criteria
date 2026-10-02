package engine

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow"
)

// gateHandle is an adapter handle whose Execute blocks on a per-step gate so a
// test can hold a step mid-flight while a pause request is registered. Steps
// registered through failOn return the recorded error after the gate opens,
// so the run exits through the failure path.
type gateHandle struct {
	mu      sync.Mutex
	blocked map[string]chan struct{}
	errs    map[string]error
}

func (g *gateHandle) block(step string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.blocked == nil {
		g.blocked = map[string]chan struct{}{}
	}
	g.blocked[step] = make(chan struct{})
}

func (g *gateHandle) release(step string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if ch, ok := g.blocked[step]; ok {
		close(ch)
		delete(g.blocked, step)
	}
}

func (g *gateHandle) failOn(step string, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.errs == nil {
		g.errs = map[string]error{}
	}
	g.errs[step] = err
}

func (g *gateHandle) Info(context.Context) (adapterhost.Info, error) {
	return adapterhost.Info{Name: "gate"}, nil
}
func (g *gateHandle) OpenSession(context.Context, string, map[string]string, map[string]string) error {
	return nil
}
func (g *gateHandle) Execute(ctx context.Context, sessionID string, node *workflow.StepNode, es adapter.EventSink, rejection *v2.ExecutionRejection) (adapter.Result, error) {
	g.mu.Lock()
	ch, ok := g.blocked[node.Name]
	err, hasErr := g.errs[node.Name]
	g.mu.Unlock()
	if ok {
		<-ch
	}
	if hasErr {
		return adapter.Result{}, err
	}
	return adapter.Result{Outcome: "success"}, nil
}
func (g *gateHandle) Permit(context.Context, string, string, bool, string) error { return nil }
func (g *gateHandle) CloseSession(context.Context, string) error                 { return nil }
func (g *gateHandle) Kill()                                                      {}
func (g *gateHandle) Pause(context.Context, string) error                        { return nil }
func (g *gateHandle) Resume(context.Context, string) error                       { return nil }
func (g *gateHandle) Inspect(context.Context, string) (*v2.InspectResponse, error) {
	return &v2.InspectResponse{}, nil
}
func (g *gateHandle) Snapshot(context.Context, string) (*v2.SnapshotResponse, error) {
	return &v2.SnapshotResponse{}, nil
}
func (g *gateHandle) Restore(context.Context, string, []byte, uint32) error { return nil }

// boundarySink wraps fakeSink and records run-level pause/resume plus a
// step-entered notification channel for deterministic mid-run coordination.
type boundarySink struct {
	fakeSink
	mu           sync.Mutex
	pausedNode   string
	pausedMode   string
	pausedSignal string
	resumed      []string
	entered      chan string
}

func newBoundarySink() *boundarySink {
	return &boundarySink{entered: make(chan string, 8)}
}

func (s *boundarySink) OnStepEntered(step, runID string, attempt int) {
	s.mu.Lock()
	s.fakeSink.OnStepEntered(step, runID, attempt)
	s.mu.Unlock()
	select {
	case s.entered <- step:
	default:
	}
}

func (s *boundarySink) OnRunPaused(node, mode, signal string) {
	s.mu.Lock()
	s.pausedNode, s.pausedMode, s.pausedSignal = node, mode, signal
	s.mu.Unlock()
}

func (s *boundarySink) OnRunResumed(node string) {
	s.mu.Lock()
	s.resumed = append(s.resumed, node)
	s.mu.Unlock()
}

func (s *boundarySink) pause() (node, mode, signal string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pausedNode, s.pausedMode, s.pausedSignal
}

// boundaryPauseHCL is a linear three-step workflow: a -> b -> c -> done.
const boundaryPauseHCL = `
workflow {
  name = "boundary-pause"
  version = "0.1"
  initial_state = "a"
  target_state  = "done"
}
step "a" {
  target = adapter.gate
  outcome "success" { next = step.b }
}
step "b" {
  target = adapter.gate
  outcome "success" { next = step.c }
}
step "c" {
  target = adapter.gate
  outcome "success" { next = step.done }
}
state "done" { terminal = true }`

// TestEngine_RequestPause_MidRunBoundary holds step "b" mid-flight, requests a
// pause, releases the step, and verifies the pause lands exactly at the next
// step boundary: the current node completes, "c" is never evaluated, the
// engine emits run-paused (mode "external") with "c" as the resume point, and
// the pause-landed channel closes only after durable state was captured. A
// fresh engine then resumes from "c" via RunFrom, emits run-resumed, and
// drives the run to completion.
func TestEngine_RequestPause_MidRunBoundary(t *testing.T) {
	g := compile(t, boundaryPauseHCL)
	gate := &gateHandle{}

	sink := newBoundarySink()
	eng := New(g, &fakeLoader{adapters: map[string]adapterhost.Handle{"gate": gate}}, sink)
	gate.block("b")

	doneRun := make(chan error, 1)
	go func() { doneRun <- eng.Run(context.Background()) }()

	enteredB := false
	for !enteredB {
		select {
		case step := <-sink.entered:
			if step == "b" {
				enteredB = true
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("timeout waiting for step b to start; entered at most %v", sink.fakeSink.stepsRun)
		}
	}

	pauseCh, ok := eng.RequestPause()
	if !ok {
		t.Fatal("RequestPause returned not-ok while step b was in flight")
	}
	secondCh, ok := eng.RequestPause()
	if !ok || secondCh != pauseCh {
		t.Fatal("RequestPause is not idempotent for a pending request")
	}
	gate.release("b")

	select {
	case err := <-doneRun:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for paused run to yield")
	}

	node, mode, signal := sink.pause()
	if node != "c" || mode != "external" || signal != "" {
		t.Fatalf("OnRunPaused: node=%q mode=%q signal=%q, want c/external/\"\"", node, mode, signal)
	}
	for _, step := range sink.fakeSink.stepsRun {
		if step == "c" {
			t.Fatal("step c must not evaluate after the pause lands")
		}
	}
	visits := eng.VisitCounts()
	if visits["a"] != 1 || visits["b"] != 1 {
		t.Fatalf("visit counts not captured at boundary: %v", visits)
	}

	select {
	case <-pauseCh:
	case <-time.After(time.Second):
		t.Fatal("pause-landed channel did not close after the pause acked")
	}

	sink2 := newBoundarySink()
	eng2 := New(g, &fakeLoader{adapters: map[string]adapterhost.Handle{"gate": gate}}, sink2)
	if err := eng2.RunFrom(context.Background(), "c", 1); err != nil {
		t.Fatalf("resume run: %v", err)
	}
	if sink2.fakeSink.terminal != "done" || !sink2.fakeSink.terminalOK {
		t.Fatalf("resumed run terminal: %s ok=%v", sink2.fakeSink.terminal, sink2.fakeSink.terminalOK)
	}
}

// TestEngine_RequestPause_ResumeEventEmitted pins down that the resume
// boundary emits OnRunResumed with the restarting node (run.resumed), giving
// local and server event streams the same vocabulary (CRI-255).
func TestEngine_RequestPause_ResumeEventEmitted(t *testing.T) {
	g := compile(t, boundaryPauseHCL)
	gate := &gateHandle{}

	sink := newBoundarySink()
	eng := New(g, &fakeLoader{adapters: map[string]adapterhost.Handle{"gate": gate}}, sink)
	gate.block("b")

	doneRun := make(chan error, 1)
	go func() { doneRun <- eng.Run(context.Background()) }()
	<-sink.entered
	_, ok := eng.RequestPause()
	if !ok {
		t.Fatal("RequestPause returned not-ok mid-run")
	}
	gate.release("b")
	select {
	case <-doneRun:
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for paused run to yield")
	}

	sink2 := newBoundarySink()
	eng2 := New(g, &fakeLoader{adapters: map[string]adapterhost.Handle{"gate": gate}}, sink2)
	if err := eng2.RunFrom(context.Background(), "c", 1); err != nil {
		t.Fatalf("resume run: %v", err)
	}
	if len(sink2.resumed) != 1 || sink2.resumed[0] != "c" {
		t.Fatalf("OnRunResumed: got %v, want [c]", sink2.resumed)
	}
}

// TestEngine_RequestPause_YieldsAtLoopTopWithoutInFlightStep exercises the
// latch with no in-flight step: the pause lands before the next node
// evaluates, exactly as a node-evaluated-after-transition boundary.
func TestEngine_RequestPause_TerminalBeforeLatch(t *testing.T) {
	g := compile(t, boundaryPauseHCL)
	sink := newBoundarySink()
	eng := New(g, &fakeLoader{adapters: map[string]adapterhost.Handle{"gate": &gateHandle{}}}, sink)
	if err := eng.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, ok := eng.RequestPause(); ok {
		t.Fatal("RequestPause must report not-ok once the run loop has exited")
	}
}

// TestEngine_RequestPause_DroppedOnStepFailure pins the pause-ack drop path
// (CRI-255): a pause request latched while a step is in flight can lose the
// race with the run exiting without honoring it — here the step fails
// terminally, so runLoop returns through the failure path. On such an exit
// the dropped pause-landed channel CLOSES (it never closes on a durable
// ack without the pause tracker also recording a node), so the control
// surface's waiter wakes immediately and re-reads the pause tracker instead
// of waiting out a long timeout on a run that is already gone.
func TestEngine_RequestPause_DroppedOnStepFailure(t *testing.T) {
	g := compile(t, boundaryPauseHCL)
	gate := &gateHandle{}
	gate.failOn("b", errors.New("adapter exploded"))

	sink := newBoundarySink()
	eng := New(g, &fakeLoader{adapters: map[string]adapterhost.Handle{"gate": gate}}, sink)
	gate.block("b")

	doneRun := make(chan error, 1)
	go func() { doneRun <- eng.Run(context.Background()) }()

	enteredB := false
	for !enteredB {
		select {
		case step := <-sink.entered:
			if step == "b" {
				enteredB = true
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("timeout waiting for step b to start; entered at most %v", sink.fakeSink.stepsRun)
		}
	}

	pauseCh, ok := eng.RequestPause()
	if !ok {
		t.Fatal("RequestPause returned not-ok while step b was in flight")
	}
	gate.release("b")

	select {
	case err := <-doneRun:
		if err == nil {
			t.Fatal("expected the run to fail after the failing step completed")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for the failing run to exit")
	}

	// The dropped request must close its channel on runLoop exit: a waiter
	// blocked on it wakes immediately instead of waiting out its timeout.
	select {
	case <-pauseCh:
	case <-time.After(2 * time.Second):
		t.Fatal("dropped pause-landed channel did not close on run exit")
	}
	// No external pause event must have fired: the run never paused.
	if node, mode, _ := sink.pause(); node != "" || mode != "" {
		t.Fatalf("dropped pause must not emit OnRunPaused, got node=%q mode=%q", node, mode)
	}
}
