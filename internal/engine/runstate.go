package engine

import (
	"sync"

	"github.com/zclconf/go-cty/cty"

	"github.com/brokenbots/criteria/workflow"
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
	// Visits tracks per-step visit counts for max_visits enforcement (W07).
	// Nil-safe: a nil map is treated as all-zero counts.
	Visits map[string]int
	// VisitsMu serializes concurrent access to Visits during parallel step
	// fan-out (W19). Nil in sequential paths — no locking overhead there.
	VisitsMu *sync.Mutex

	// ReturnOutputs holds the projected output values when a step exits via
	// next = step.return. Set by stepNode.evaluateOnce; consumed by
	// handleReturnExit (top-level) or runSubworkflow (nested). Nil means
	// no return exit has occurred.
	ReturnOutputs map[string]cty.Value

	// DataStore is the workflow-scoped store for data block values.
	// Each top-level workflow and each subworkflow body receives its own fresh
	// store populated from the compiled graph's Data. Nil when the workflow
	// declares no data blocks.
	DataStore *DataStore

	// WorkflowName is the name of the workflow body currently executing. Used
	// for engine-version compatibility diagnostics in nested subworkflows.
	WorkflowName string
	// Ancestors is the chain of parent workflow names leading to the current
	// workflow body. Empty for a root workflow.
	Ancestors []string
	// ParallelCeiling is the maximum concurrency inherited from any enclosing
	// parallel step. Zero means no ancestor imposed a ceiling. A parallel step
	// uses min(step.ParallelMax, st.ParallelCeiling) as its effective cap and
	// propagates that effective cap to its descendants so that the subtree-wide
	// concurrency never exceeds the smallest parallel_max along the path from
	// the root workflow.
	ParallelCeiling int
	// ParallelSem is the shared token pool for the current subtree ceiling.
	// When a nested parallel step's effective cap equals the inherited ceiling,
	// it reuses this semaphore so that leaf executions across all parent
	// iterations contend for the same global limit. Nil means there is no
	// inherited parallel step and the next parallel step should create its own.
	ParallelSem chan struct{}
	// ParallelSemCache holds per-step leaf semaphores that are lower than the
	// inherited ceiling. The same compiled parallel step can run in many parent
	// iterations (e.g. a subworkflow invoked by a parallel parent), so the
	// semaphore must be shared across all of those instances. The key identifies
	// the compiled step and the ancestor context (inherited ceiling and parent
	// leaf semaphore) so that semaphores are not accidentally shared across
	// unrelated call paths.
	ParallelSemCache map[parallelSemKey]chan struct{}
	// ParallelSemMu serializes access to ParallelSemCache across concurrent
	// parent iterations.
	ParallelSemMu *sync.Mutex

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
