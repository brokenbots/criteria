package engine_test

import (
	"context"
	"testing"
	"time"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/engine"
	"github.com/brokenbots/criteria/internal/plugin"
	"github.com/brokenbots/criteria/workflow"
)

// emptyLoader is a plugin loader with no adapters registered, suitable for
// wait/approval node tests that don't execute step adapters.
func emptyLoader() *plugin.DefaultLoader {
	return plugin.NewLoader()
}

// minimalWaitGraph builds a graph with a single duration-wait node.
func minimalWaitDurationGraph(dur time.Duration) *workflow.FSMGraph {
	g := &workflow.FSMGraph{
		Name:      "wait_test",
		Steps:     map[string]*workflow.StepNode{},
		States:    map[string]*workflow.StateNode{},
		Waits:     map[string]*workflow.WaitNode{},
		Approvals: map[string]*workflow.ApprovalNode{},
		Variables: map[string]*workflow.VariableNode{},
	}
	g.InitialState = "pause"
	g.TargetState = "done"
	g.Waits["pause"] = &workflow.WaitNode{
		Name:     "pause",
		Duration: dur,
		Outcomes: map[string]string{"elapsed": "done"},
	}
	g.States["done"] = &workflow.StateNode{
		Name:     "done",
		Terminal: true,
		Success:  true,
	}
	return g
}

func minimalWaitSignalGraph(signal string) *workflow.FSMGraph {
	g := &workflow.FSMGraph{
		Name:      "wait_signal_test",
		Steps:     map[string]*workflow.StepNode{},
		States:    map[string]*workflow.StateNode{},
		Waits:     map[string]*workflow.WaitNode{},
		Approvals: map[string]*workflow.ApprovalNode{},
		Variables: map[string]*workflow.VariableNode{},
	}
	g.InitialState = "gating"
	g.TargetState = "done"
	g.Waits["gating"] = &workflow.WaitNode{
		Name:   "gating",
		Signal: signal,
		Outcomes: map[string]string{
			"approved": "done",
			"rejected": "done",
		},
	}
	g.States["done"] = &workflow.StateNode{
		Name:     "done",
		Terminal: true,
		Success:  true,
	}
	return g
}

func minimalApprovalGraph(nodeName string) *workflow.FSMGraph {
	g := &workflow.FSMGraph{
		Name:      "approval_test",
		Steps:     map[string]*workflow.StepNode{},
		States:    map[string]*workflow.StateNode{},
		Waits:     map[string]*workflow.WaitNode{},
		Approvals: map[string]*workflow.ApprovalNode{},
		Variables: map[string]*workflow.VariableNode{},
	}
	g.InitialState = nodeName
	g.TargetState = "done"
	g.Approvals[nodeName] = &workflow.ApprovalNode{
		Name:      nodeName,
		Approvers: []string{"alice"},
		Reason:    "needs review",
		Outcomes: map[string]string{
			"approved": "done",
			"rejected": "done",
		},
	}
	g.States["done"] = &workflow.StateNode{
		Name:     "done",
		Terminal: true,
		Success:  true,
	}
	return g
}

// pauseSink records OnRunPaused calls.
type pauseSink struct {
	pausedNode   string
	pausedMode   string
	pausedSignal string
	completed    bool
	failed       bool
}

func (s *pauseSink) OnRunStarted(string, string)                        {}
func (s *pauseSink) OnRunCompleted(string, bool)                        { s.completed = true }
func (s *pauseSink) OnRunFailed(string, string)                         { s.failed = true }
func (s *pauseSink) OnStepEntered(string, string, int)                  {}
func (s *pauseSink) OnStepOutcome(string, string, time.Duration, error) {}
func (s *pauseSink) OnStepTransition(string, string, string)            {}
func (s *pauseSink) OnStepResumed(string, int, string)                  {}
func (s *pauseSink) OnVariableSet(string, string, string)               {}
func (s *pauseSink) OnStepOutputCaptured(string, map[string]string)     {}
func (s *pauseSink) OnRunPaused(node, mode, signal string) {
	s.pausedNode = node
	s.pausedMode = mode
	s.pausedSignal = signal
}
func (s *pauseSink) OnWaitEntered(string, string, string, string)                 {}
func (s *pauseSink) OnWaitResumed(string, string, string, map[string]string)      {}
func (s *pauseSink) OnApprovalRequested(string, []string, string)                 {}
func (s *pauseSink) OnApprovalDecision(string, string, string, map[string]string) {}
func (s *pauseSink) OnBranchEvaluated(string, string, string, string)             {}
func (s *pauseSink) OnForEachEntered(string, int)                                 {}
func (s *pauseSink) OnForEachIteration(string, int, string, bool)                 {}
func (s *pauseSink) OnForEachOutcome(string, string, string)                      {}
func (s *pauseSink) OnScopeIterCursorSet(string)                                  {}
func (s *pauseSink) StepEventSink(string) adapter.EventSink                       { return noopAdapterSink{} }

type noopAdapterSink struct{}

func (noopAdapterSink) Log(string, []byte)  {}
func (noopAdapterSink) Adapter(string, any) {}

// --- Tests ---

func TestNodeWait_Duration_CompletesAfterSleep(t *testing.T) {
	const dur = 10 * time.Millisecond
	g := minimalWaitDurationGraph(dur)
	sink := &pauseSink{}
	eng := engine.New(g, emptyLoader(), sink)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	if err := eng.Run(ctx); err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	elapsed := time.Since(start)

	if !sink.completed {
		t.Error("expected run completed")
	}
	// Wall-clock assertions (exit criteria: within ±200 ms).
	if elapsed < dur {
		t.Errorf("expected elapsed >= %v, got %v (sleep was skipped)", dur, elapsed)
	}
	if elapsed > dur+200*time.Millisecond {
		t.Errorf("expected elapsed <= %v, got %v (CI timeout?)", dur+200*time.Millisecond, elapsed)
	}
}

func TestNodeWait_Signal_PausesRun(t *testing.T) {
	g := minimalWaitSignalGraph("approve")
	sink := &pauseSink{}
	eng := engine.New(g, emptyLoader(), sink)
	if err := eng.Run(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sink.pausedNode != "gating" {
		t.Errorf("expected paused at 'gating', got %q", sink.pausedNode)
	}
	if sink.pausedMode != "signal" {
		t.Errorf("expected mode 'signal', got %q", sink.pausedMode)
	}
}

func TestNodeWait_Duration_CancelledMidSleep(t *testing.T) {
	g := minimalWaitDurationGraph(5 * time.Second)
	sink := &pauseSink{}
	eng := engine.New(g, emptyLoader(), sink)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := eng.Run(ctx)
	if err == nil {
		t.Error("expected context-cancelled error, got nil")
	}
}

func TestNodeWait_Signal_ResumeDeliversOutcome(t *testing.T) {
	g := minimalWaitSignalGraph("approve")
	sink := &pauseSink{}
	// First run: should pause.
	eng := engine.New(g, emptyLoader(), sink)
	if err := eng.Run(context.Background()); err != nil {
		t.Fatalf("first run error: %v", err)
	}
	if sink.pausedNode != "gating" {
		t.Fatalf("expected pause at 'gating', got %q", sink.pausedNode)
	}
	// Resume run with payload.
	sink2 := &pauseSink{}
	resumedEng := engine.New(g, emptyLoader(), sink2,
		engine.WithResumedVars(eng.VarScope()),
		engine.WithResumePayload(map[string]string{"outcome": "approved"}),
	)
	if err := resumedEng.RunFrom(context.Background(), "gating", 1); err != nil {
		t.Fatalf("resume run error: %v", err)
	}
	if !sink2.completed {
		t.Error("expected completed after resume")
	}
}

func TestNodeApproval_PausesRun(t *testing.T) {
	g := minimalApprovalGraph("check")
	sink := &pauseSink{}
	eng := engine.New(g, emptyLoader(), sink)
	if err := eng.Run(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sink.pausedNode != "check" {
		t.Errorf("expected paused at 'check', got %q", sink.pausedNode)
	}
}

func TestNodeApproval_ResumeApproved(t *testing.T) {
	g := minimalApprovalGraph("check")
	sink := &pauseSink{}
	eng := engine.New(g, emptyLoader(), sink)
	if err := eng.Run(context.Background()); err != nil {
		t.Fatalf("first run error: %v", err)
	}
	sink2 := &pauseSink{}
	resumedEng := engine.New(g, emptyLoader(), sink2,
		engine.WithResumedVars(eng.VarScope()),
		engine.WithResumePayload(map[string]string{"decision": "approved"}),
	)
	if err := resumedEng.RunFrom(context.Background(), "check", 1); err != nil {
		t.Fatalf("resume error: %v", err)
	}
	if !sink2.completed {
		t.Error("expected completed after approval")
	}
}

func TestNodeApproval_ResumeRejected(t *testing.T) {
	g := minimalApprovalGraph("check")
	sink := &pauseSink{}
	eng := engine.New(g, emptyLoader(), sink)
	if err := eng.Run(context.Background()); err != nil {
		t.Fatalf("first run error: %v", err)
	}
	sink2 := &pauseSink{}
	resumedEng := engine.New(g, emptyLoader(), sink2,
		engine.WithResumedVars(eng.VarScope()),
		engine.WithResumePayload(map[string]string{"decision": "rejected"}),
	)
	if err := resumedEng.RunFrom(context.Background(), "check", 1); err != nil {
		t.Fatalf("resume error: %v", err)
	}
	if !sink2.completed {
		t.Error("expected completed after rejection")
	}
}
