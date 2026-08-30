package cli

import (
	"sync"

	"github.com/brokenbots/criteria/internal/engine"
)

// terminalSuccessSink wraps an engine.Sink and records the success value of
// the last OnRunCompleted event. The apply command uses this to map a terminal
// failed run to a non-zero OS exit code without changing event emission or
// console output.
type terminalSuccessSink struct {
	engine.Sink
	mu         sync.Mutex
	finalState string
	success    *bool
}

func (s *terminalSuccessSink) OnRunCompleted(finalState string, success bool) {
	s.mu.Lock()
	s.finalState = finalState
	s.success = &success
	s.mu.Unlock()
	s.Sink.OnRunCompleted(finalState, success)
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
