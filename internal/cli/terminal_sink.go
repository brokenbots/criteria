package cli

import (
	"sync"

	"github.com/brokenbots/criteria/internal/engine"
)

// terminalSuccessSink wraps an engine.Sink and records the success value of
// the last OnRunCompleted event. The apply command uses this to map a terminal
// failed run to a non-zero OS exit code without changing event emission or
// console output. It also captures the last rejected approval decision (CRI-256)
// so the rejection reason can surface in the run's failure error.
type terminalSuccessSink struct {
	engine.Sink
	mu           sync.Mutex
	finalState   string
	success      *bool
	decisionNode string
	decision     string
	reason       string
}

func (s *terminalSuccessSink) OnRunCompleted(finalState string, success bool) {
	s.mu.Lock()
	s.finalState = finalState
	s.success = &success
	s.mu.Unlock()
	s.Sink.OnRunCompleted(finalState, success)
}

func (s *terminalSuccessSink) OnApprovalDecision(node, decision, actor string, payload map[string]string) {
	s.mu.Lock()
	s.decisionNode = node
	s.decision = decision
	if payload != nil {
		s.reason = payload["reason"]
	}
	s.mu.Unlock()
	s.Sink.OnApprovalDecision(node, decision, actor, payload)
}

// TerminalSuccess reports whether OnRunCompleted has been observed and, if so,
// the final state name and its success bit.
func (s *terminalSuccessSink) TerminalSuccess() (finalState string, success, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.success == nil {
		return "", false, false
	}
	return s.finalState, *s.success, true
}

// Rejection reports the last rejected approval decision with its reason
// (CRI-256): rejection reasons must survive from the prompt or answers file
// into the run's failure error. ok=false when no rejected decision was
// observed (approved or undecided).
func (s *terminalSuccessSink) Rejection() (node, reason string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.decision != "rejected" {
		return "", "", false
	}
	return s.decisionNode, s.reason, true
}
