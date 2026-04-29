package engine

// node_workflow.go — sub-workflow body execution helper for type="workflow"
// steps (W10). The body is an independently compiled FSMGraph with a synthetic
// "_continue" terminal state. The engine runs the body in a nested loop until
// it reaches a terminal state. If that terminal state is "_continue", the
// caller treats it as a normal iteration-advance; any other terminal state is
// an early-exit and signals the iteration to stop.

import (
	"context"
	"errors"
	"fmt"

	engineruntime "github.com/brokenbots/criteria/internal/engine/runtime"
	"github.com/brokenbots/criteria/workflow"
)

// runWorkflowBody executes the sub-workflow body synchronously in a nested
// engine loop. It returns the terminal state name when the body reaches a
// terminal state, or an error on fatal conditions.
//
//   - body is the compiled FSMGraph of the sub-workflow body.
//   - bodyEntry is the initial state name for the body run.
//   - st is the outer RunState; body execution shares Vars with the outer loop.
//   - deps carries the same session manager and event sink as the outer loop.
//
// When the body reaches "_continue" the caller should treat the iteration as
// successfully completed and advance the cursor. Any other terminal state is
// an early-exit from the iteration; the caller should forward that outcome.
func runWorkflowBody(ctx context.Context, body *workflow.FSMGraph, bodyEntry string, st *RunState, deps Deps) (string, error) {
	if bodyEntry == "" {
		bodyEntry = body.InitialState
	}
	if bodyEntry == "" {
		return "", fmt.Errorf("workflow body has no initial state")
	}

	// Build a child RunState that shares Vars (so body steps can read/write
	// outer vars and each.* bindings) but has its own step counter and
	// execution position.
	childSt := &RunState{
		Current:       bodyEntry,
		Vars:          st.Vars,
		WorkflowDir:   st.WorkflowDir,
		PendingSignal: "",
		ResumePayload: nil,
		firstStep:     false,
	}

	for {
		node, err := nodeFor(body, childSt.Current)
		if err != nil {
			return "", fmt.Errorf("workflow body: %w", err)
		}
		next, err := node.Evaluate(ctx, childSt, deps)
		if err != nil {
			if errors.Is(err, engineruntime.ErrTerminal) {
				// Terminal state reached: propagate vars back to outer scope.
				st.Vars = childSt.Vars
				return childSt.Current, nil
			}
			return "", fmt.Errorf("workflow body step %q: %w", childSt.Current, err)
		}
		childSt.Current = next
	}
}
