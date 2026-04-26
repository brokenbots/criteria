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
	Iter          *IterCursor
	ParentRunID   string
	BranchID      string

	firstStep        bool
	firstStepAttempt int
}
