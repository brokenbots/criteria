package engine

import (
	"github.com/brokenbots/criteria/workflow"
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
	// IterStack is the active step-level iteration cursor stack (W10).
	// An empty slice means no iteration is in progress.
	// The last element is the innermost (currently-executing) cursor.
	// A non-empty stack with the top cursor's InProgress=true means a step
	// body is currently executing for that cursor.
	IterStack []workflow.IterCursor
	// LastOutcome records the most recent step outcome name. Set by stepNode
	// before returning to the engine loop. Used by routeIteratingStep to
	// determine whether the completed iteration was a failure (W10).
	LastOutcome string
	ParentRunID string
	BranchID    string
	// WorkflowDir is the directory containing the HCL workflow file. Used by
	// file() and fileexists() expression functions to resolve relative paths.
	// Set from Engine.workflowDir at run start.
	WorkflowDir string

	firstStep        bool
	firstStepAttempt int
}

// TopCursor returns a pointer to the innermost IterCursor, or nil when no
// iteration is in progress.
func (rs *RunState) TopCursor() *workflow.IterCursor {
	if len(rs.IterStack) == 0 {
		return nil
	}
	return &rs.IterStack[len(rs.IterStack)-1]
}

// PushCursor appends a new cursor to the stack, making it the active cursor.
func (rs *RunState) PushCursor(c *workflow.IterCursor) {
	rs.IterStack = append(rs.IterStack, *c)
}

// PopCursor removes and returns the innermost cursor. It is a no-op when the
// stack is empty (returns a zero-value IterCursor).
func (rs *RunState) PopCursor() workflow.IterCursor {
	if len(rs.IterStack) == 0 {
		return workflow.IterCursor{}
	}
	top := rs.IterStack[len(rs.IterStack)-1]
	rs.IterStack = rs.IterStack[:len(rs.IterStack)-1]
	return top
}
