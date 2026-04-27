package engine

import (
	"github.com/brokenbots/overseer/workflow"
	"github.com/zclconf/go-cty/cty"
)

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
	// Iter tracks the active for_each iteration cursor (W07). Nil when no
	// for_each loop is executing. Set by forEachNode.Evaluate; advanced and
	// cleared by the engine loop's _continue interception.
	Iter *workflow.IterCursor
	// LastOutcome records the most recent step outcome name. Set by stepNode
	// before returning to the engine loop. Used by the _continue interception
	// to determine whether the completed iteration was a failure (W07).
	LastOutcome string
	ParentRunID string
	BranchID    string

	firstStep        bool
	firstStepAttempt int
}
