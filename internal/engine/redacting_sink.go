package engine

import (
	"errors"
	"time"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapter/secrets"
)

// RedactingSink wraps an engine Sink and redacts all string arguments through
// the provided secrets registry before forwarding. It is used by the engine to
// ensure that any secret value that leaks into an event argument is masked
// before the event reaches persistence or display.
type RedactingSink struct {
	inner Sink
	reg   *secrets.Registry
}

// NewRedactingSink returns a Sink that redacts registered secret values from
// every string argument passed to the underlying sink.
func NewRedactingSink(inner Sink, reg *secrets.Registry) Sink {
	if reg == nil {
		return inner
	}
	return &RedactingSink{inner: inner, reg: reg}
}

func (s *RedactingSink) OnRunStarted(workflowName, initialStep string) {
	s.inner.OnRunStarted(s.reg.Redact(workflowName), s.reg.Redact(initialStep))
}

func (s *RedactingSink) OnRunCompleted(finalState string, success bool) {
	s.inner.OnRunCompleted(s.reg.Redact(finalState), success)
}

func (s *RedactingSink) OnRunFailed(reason, step string) {
	s.inner.OnRunFailed(s.reg.Redact(reason), s.reg.Redact(step))
}

func (s *RedactingSink) OnStepEntered(step, adapterName string, attempt int) {
	s.inner.OnStepEntered(s.reg.Redact(step), s.reg.Redact(adapterName), attempt)
}

func (s *RedactingSink) OnStepOutcome(step, outcome string, duration time.Duration, err error) {
	var redactedErr error
	if err != nil {
		redactedErr = errors.New(s.reg.Redact(err.Error()))
	}
	s.inner.OnStepOutcome(s.reg.Redact(step), s.reg.Redact(outcome), duration, redactedErr)
}

func (s *RedactingSink) OnStepTransition(from, to, viaOutcome string) {
	s.inner.OnStepTransition(s.reg.Redact(from), s.reg.Redact(to), s.reg.Redact(viaOutcome))
}

func (s *RedactingSink) OnStepResumed(step string, attempt int, reason string) {
	s.inner.OnStepResumed(s.reg.Redact(step), attempt, s.reg.Redact(reason))
}

func (s *RedactingSink) OnVariableSet(name, value, source string) {
	s.inner.OnVariableSet(s.reg.Redact(name), s.reg.Redact(value), s.reg.Redact(source))
}

func (s *RedactingSink) OnStepOutputCaptured(step string, outputs map[string]string) {
	redacted := make(map[string]string, len(outputs))
	for k, v := range outputs {
		redacted[s.reg.Redact(k)] = s.reg.Redact(v)
	}
	s.inner.OnStepOutputCaptured(s.reg.Redact(step), redacted)
}

func (s *RedactingSink) OnRunPaused(node, mode, signal string) {
	s.inner.OnRunPaused(s.reg.Redact(node), s.reg.Redact(mode), s.reg.Redact(signal))
}

func (s *RedactingSink) OnWaitEntered(node, mode, duration, signal string) {
	s.inner.OnWaitEntered(s.reg.Redact(node), s.reg.Redact(mode), s.reg.Redact(duration), s.reg.Redact(signal))
}

func (s *RedactingSink) OnWaitResumed(node, mode, signal string, payload map[string]string) {
	redactedPayload := make(map[string]string, len(payload))
	for k, v := range payload {
		redactedPayload[s.reg.Redact(k)] = s.reg.Redact(v)
	}
	s.inner.OnWaitResumed(s.reg.Redact(node), s.reg.Redact(mode), s.reg.Redact(signal), redactedPayload)
}

func (s *RedactingSink) OnApprovalRequested(node string, approvers []string, reason string) {
	rApprovers := make([]string, len(approvers))
	for i, a := range approvers {
		rApprovers[i] = s.reg.Redact(a)
	}
	s.inner.OnApprovalRequested(s.reg.Redact(node), rApprovers, s.reg.Redact(reason))
}

func (s *RedactingSink) OnApprovalDecision(node, decision, actor string, payload map[string]string) {
	redactedPayload := make(map[string]string, len(payload))
	for k, v := range payload {
		redactedPayload[s.reg.Redact(k)] = s.reg.Redact(v)
	}
	s.inner.OnApprovalDecision(s.reg.Redact(node), s.reg.Redact(decision), s.reg.Redact(actor), redactedPayload)
}

func (s *RedactingSink) OnBranchEvaluated(node, matchedArm, target, condition string) {
	s.inner.OnBranchEvaluated(s.reg.Redact(node), s.reg.Redact(matchedArm), s.reg.Redact(target), s.reg.Redact(condition))
}

func (s *RedactingSink) OnForEachEntered(node string, count int) {
	s.inner.OnForEachEntered(s.reg.Redact(node), count)
}

func (s *RedactingSink) OnStepIterationStarted(node string, index int, value string, anyFailed bool) {
	s.inner.OnStepIterationStarted(s.reg.Redact(node), index, s.reg.Redact(value), anyFailed)
}

func (s *RedactingSink) OnStepIterationCompleted(node, outcome, target string) {
	s.inner.OnStepIterationCompleted(s.reg.Redact(node), s.reg.Redact(outcome), s.reg.Redact(target))
}

func (s *RedactingSink) OnStepIterationItem(node string, index int, step string) {
	s.inner.OnStepIterationItem(s.reg.Redact(node), index, s.reg.Redact(step))
}

func (s *RedactingSink) OnScopeIterCursorSet(cursorJSON string) {
	s.inner.OnScopeIterCursorSet(s.reg.Redact(cursorJSON))
}

func (s *RedactingSink) OnAdapterLifecycle(stepName, adapterName, status, detail string) {
	s.inner.OnAdapterLifecycle(s.reg.Redact(stepName), s.reg.Redact(adapterName), s.reg.Redact(status), s.reg.Redact(detail))
}

func (s *RedactingSink) OnRunOutputs(outputs []map[string]string) {
	redacted := make([]map[string]string, len(outputs))
	for i, out := range outputs {
		m := make(map[string]string, len(out))
		for k, v := range out {
			m[s.reg.Redact(k)] = s.reg.Redact(v)
		}
		redacted[i] = m
	}
	s.inner.OnRunOutputs(redacted)
}

func (s *RedactingSink) OnStepOutcomeDefaulted(step, original, mapped string) {
	s.inner.OnStepOutcomeDefaulted(s.reg.Redact(step), s.reg.Redact(original), s.reg.Redact(mapped))
}

func (s *RedactingSink) OnStepOutcomeUnknown(step, outcome string) {
	s.inner.OnStepOutcomeUnknown(s.reg.Redact(step), s.reg.Redact(outcome))
}

func (s *RedactingSink) StepEventSink(step string) adapter.EventSink {
	inner := s.inner.StepEventSink(step)
	if inner == nil {
		return nil
	}
	return &secrets.RedactingEventSink{Inner: inner, Registry: s.reg}
}
