package engine

import "github.com/zclconf/go-cty/cty"

// IterCursor is reserved for for_each iteration support in Phase 1.5+.
type IterCursor struct{}

// RunState carries mutable run-scoped interpreter state.
type RunState struct {
	Current       string
	TotalSteps    int
	Vars          map[string]cty.Value
	PendingSignal string
	// ResumePayload carries the key/value payload delivered by a Resume RPC.
	// Non-nil when the engine is re-entered after a signal wait or approval.
	// The wait/approval node consumes it and clears it. Nil on first entry.
	ResumePayload map[string]string
	Iter          *IterCursor
	ParentRunID   string
	BranchID      string

	firstStep        bool
	firstStepAttempt int
}
