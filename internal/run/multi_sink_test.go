package run

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/engine"
)

type recordingSink struct {
	calls   atomic.Int32
	stepFan atomic.Int32 // increments on Log+Adapter via fanout step sink
}

func (r *recordingSink) bump()                                                   { r.calls.Add(1) }
func (r *recordingSink) OnRunStarted(string, string)                             { r.bump() }
func (r *recordingSink) OnRunCompleted(string, bool)                             { r.bump() }
func (r *recordingSink) OnRunFailed(string, string)                              { r.bump() }
func (r *recordingSink) OnStepEntered(string, string, int)                       { r.bump() }
func (r *recordingSink) OnStepOutcome(string, string, time.Duration, error)      { r.bump() }
func (r *recordingSink) OnStepTransition(string, string, string)                 { r.bump() }
func (r *recordingSink) OnStepResumed(string, int, string)                       { r.bump() }
func (r *recordingSink) OnVariableSet(string, string, string)                    { r.bump() }
func (r *recordingSink) OnStepOutputCaptured(string, map[string]string)          { r.bump() }
func (r *recordingSink) OnRunPaused(string, string, string)                      { r.bump() }
func (r *recordingSink) OnWaitEntered(string, string, string, string)            { r.bump() }
func (r *recordingSink) OnWaitResumed(string, string, string, map[string]string) { r.bump() }
func (r *recordingSink) OnApprovalRequested(string, []string, string)            { r.bump() }
func (r *recordingSink) OnApprovalDecision(string, string, string, map[string]string) {
	r.bump()
}
func (r *recordingSink) OnBranchEvaluated(string, string, string, string)  { r.bump() }
func (r *recordingSink) OnForEachEntered(string, int)                      { r.bump() }
func (r *recordingSink) OnStepIterationStarted(string, int, string, bool)  { r.bump() }
func (r *recordingSink) OnStepIterationCompleted(string, string, string)   { r.bump() }
func (r *recordingSink) OnStepIterationItem(string, int, string)           { r.bump() }
func (r *recordingSink) OnScopeIterCursorSet(string)                       { r.bump() }
func (r *recordingSink) OnAdapterLifecycle(string, string, string, string) { r.bump() }
func (r *recordingSink) StepEventSink(step string) adapter.EventSink {
	return &recordingStepSink{parent: r}
}

type recordingStepSink struct{ parent *recordingSink }

func (s *recordingStepSink) Log(string, []byte)  { s.parent.stepFan.Add(1) }
func (s *recordingStepSink) Adapter(string, any) { s.parent.stepFan.Add(1) }

func TestMultiSink_FansEveryEventToAllChildren(t *testing.T) {
	var a, b recordingSink
	var sink engine.Sink = NewMultiSink(&a, &b)

	sink.OnRunStarted("wf", "init")
	sink.OnStepEntered("s", "demo", 1)
	sink.OnStepOutcome("s", "success", time.Millisecond, nil)
	sink.OnStepTransition("s", "done", "success")
	sink.OnRunCompleted("done", true)
	ss := sink.StepEventSink("s")
	ss.Log("stdout", []byte("x"))
	ss.Adapter("agent.message", map[string]any{"content": "y"})

	if got := a.calls.Load(); got != 5 {
		t.Errorf("child a engine calls: got %d want 5", got)
	}
	if got := b.calls.Load(); got != 5 {
		t.Errorf("child b engine calls: got %d want 5", got)
	}
	if got := a.stepFan.Load(); got != 2 {
		t.Errorf("child a step fan calls: got %d want 2", got)
	}
	if got := b.stepFan.Load(); got != 2 {
		t.Errorf("child b step fan calls: got %d want 2", got)
	}
}

func TestMultiSink_NilChildIgnored(t *testing.T) {
	var a recordingSink
	sink := NewMultiSink(nil, &a, nil)
	sink.OnRunStarted("wf", "init")
	if got := a.calls.Load(); got != 1 {
		t.Errorf("expected 1 call to surviving child, got %d", got)
	}
}
