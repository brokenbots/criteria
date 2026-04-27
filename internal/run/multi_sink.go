package run

import (
	"time"

	"github.com/brokenbots/overseer/internal/adapter"
	"github.com/brokenbots/overseer/internal/engine"
)

// MultiSink fans engine events out to multiple child sinks. It is used in
// standalone mode to drive a LocalSink (ND-JSON record) and a ConsoleSink
// (human progress view) from the same engine run.
//
// Children are invoked sequentially in registration order on the engine's
// goroutine. Each child is responsible for its own locking; MultiSink adds
// none of its own.
type MultiSink struct {
	children []engine.Sink
}

// NewMultiSink returns a sink that forwards every event to each of the given
// children. Nil children are skipped.
func NewMultiSink(children ...engine.Sink) *MultiSink {
	out := make([]engine.Sink, 0, len(children))
	for _, c := range children {
		if c != nil {
			out = append(out, c)
		}
	}
	return &MultiSink{children: out}
}

func (m *MultiSink) OnRunStarted(workflowName, initialStep string) {
	for _, c := range m.children {
		c.OnRunStarted(workflowName, initialStep)
	}
}

func (m *MultiSink) OnRunCompleted(finalState string, success bool) {
	for _, c := range m.children {
		c.OnRunCompleted(finalState, success)
	}
}

func (m *MultiSink) OnRunFailed(reason, step string) {
	for _, c := range m.children {
		c.OnRunFailed(reason, step)
	}
}

func (m *MultiSink) OnStepEntered(step, adapterName string, attempt int) {
	for _, c := range m.children {
		c.OnStepEntered(step, adapterName, attempt)
	}
}

func (m *MultiSink) OnStepOutcome(step, outcome string, duration time.Duration, err error) {
	for _, c := range m.children {
		c.OnStepOutcome(step, outcome, duration, err)
	}
}

func (m *MultiSink) OnStepTransition(from, to, viaOutcome string) {
	for _, c := range m.children {
		c.OnStepTransition(from, to, viaOutcome)
	}
}

func (m *MultiSink) OnStepResumed(step string, attempt int, reason string) {
	for _, c := range m.children {
		c.OnStepResumed(step, attempt, reason)
	}
}

func (m *MultiSink) OnVariableSet(name, value, source string) {
	for _, c := range m.children {
		c.OnVariableSet(name, value, source)
	}
}

func (m *MultiSink) OnStepOutputCaptured(step string, outputs map[string]string) {
	for _, c := range m.children {
		c.OnStepOutputCaptured(step, outputs)
	}
}

func (m *MultiSink) OnRunPaused(node, mode, signal string) {
	for _, c := range m.children {
		c.OnRunPaused(node, mode, signal)
	}
}

func (m *MultiSink) OnWaitEntered(node, mode, duration, signal string) {
	for _, c := range m.children {
		c.OnWaitEntered(node, mode, duration, signal)
	}
}

func (m *MultiSink) OnWaitResumed(node, mode, signal string, payload map[string]string) {
	for _, c := range m.children {
		c.OnWaitResumed(node, mode, signal, payload)
	}
}

func (m *MultiSink) OnApprovalRequested(node string, approvers []string, reason string) {
	for _, c := range m.children {
		c.OnApprovalRequested(node, approvers, reason)
	}
}

func (m *MultiSink) OnApprovalDecision(node, decision, actor string, payload map[string]string) {
	for _, c := range m.children {
		c.OnApprovalDecision(node, decision, actor, payload)
	}
}

func (m *MultiSink) OnBranchEvaluated(node, matchedArm, target, condition string) {
	for _, c := range m.children {
		c.OnBranchEvaluated(node, matchedArm, target, condition)
	}
}

func (m *MultiSink) OnForEachEntered(node string, count int) {
	for _, c := range m.children {
		c.OnForEachEntered(node, count)
	}
}

func (m *MultiSink) OnForEachIteration(node string, index int, value string, anyFailed bool) {
	for _, c := range m.children {
		c.OnForEachIteration(node, index, value, anyFailed)
	}
}

func (m *MultiSink) OnForEachOutcome(node, outcome, target string) {
	for _, c := range m.children {
		c.OnForEachOutcome(node, outcome, target)
	}
}

func (m *MultiSink) OnScopeIterCursorSet(cursorJSON string) {
	for _, c := range m.children {
		c.OnScopeIterCursorSet(cursorJSON)
	}
}

func (m *MultiSink) StepEventSink(step string) adapter.EventSink {
	subs := make([]adapter.EventSink, 0, len(m.children))
	for _, c := range m.children {
		subs = append(subs, c.StepEventSink(step))
	}
	return &multiStepSink{children: subs}
}

type multiStepSink struct {
	children []adapter.EventSink
}

func (s *multiStepSink) Log(stream string, chunk []byte) {
	for _, c := range s.children {
		c.Log(stream, chunk)
	}
}

func (s *multiStepSink) Adapter(kind string, data any) {
	for _, c := range s.children {
		c.Adapter(kind, data)
	}
}
