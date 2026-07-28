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
	"sort"
	"strings"

	"github.com/zclconf/go-cty/cty"

	engineruntime "github.com/brokenbots/criteria/internal/engine/runtime"
	"github.com/brokenbots/criteria/workflow"
)

// checkRequiredVars returns an error if any required body variable (no default)
// lacks a binding in parentInput, or if its binding is null. This is the
// runtime complement to the compile-time check in compileWorkflowStep.
func checkRequiredVars(body *workflow.FSMGraph, parentInput cty.Value) error {
	hasInput := parentInput != cty.NilVal && parentInput.IsKnown() && !parentInput.IsNull() && parentInput.Type().IsObjectType()
	var missing []string
	for name, node := range body.Variables {
		if !node.IsRequired() {
			continue
		}
		supplied := hasInput && parentInput.Type().HasAttribute(name) && !parentInput.GetAttr(name).IsNull()
		if !supplied {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("workflow body missing required input(s): %s", strings.Join(missing, ", "))
	}
	return nil
}

// runWorkflowBody executes the sub-workflow body synchronously in a nested
// engine loop. It returns the terminal state name, any return-projected outputs
// (non-nil when the body exited via "return" sentinel), and the child's final
// vars when the body reaches a terminal state, or an error on fatal conditions.
//
//   - body is the compiled FSMGraph of the sub-workflow body.
//   - bodyEntry is the initial state name for the body run.
//   - childVars is the pre-seeded child scope built by seedChildVarsFromBindings.
//   - workflowDir is forwarded for file() resolution in eval contexts.
//   - deps carries the same session manager and event sink as the outer loop.
//
// When the body reaches "_continue" the caller should treat the iteration as
// successfully completed and advance the cursor. Any other terminal state is
// an early-exit from the iteration; the caller should forward that outcome.
// When terminal == "return", returnOutputs carries the projected output values;
// the caller should skip normal output evaluation.
//
// The returned child vars represent the body's final execution scope and are
// used by the caller to evaluate output{} block expressions.
func runWorkflowBody(ctx context.Context, body *workflow.FSMGraph, bodyEntry string, childVars map[string]cty.Value, workflowDir string, deps Deps) (terminal string, returnOutputs, finalVars map[string]cty.Value, err error) {
	if bodyEntry == "" {
		bodyEntry = body.InitialState
	}
	if bodyEntry == "" {
		return "", nil, nil, fmt.Errorf("workflow body has no initial state")
	}

	// Body-scope adapter provisioning (W12): each body declares its own adapters.
	bodyOrder, err := initScopeAdapters(ctx, body, deps, childVars, workflowDir)
	if err != nil {
		return "", nil, nil, fmt.Errorf("workflow body init adapters: %w", err)
	}
	defer func() { tearDownScopeAdapters(ctx, bodyOrder, deps) }()

	childSt := &RunState{
		Current:       bodyEntry,
		Vars:          childVars,
		WorkflowDir:   workflowDir,
		PendingSignal: "",
		ResumePayload: nil,
		DataStore:     NewDataStore(body),
		firstStep:     false,
	}

	for {
		node, err := nodeFor(body, childSt.Current)
		if err != nil {
			return "", nil, nil, fmt.Errorf("workflow body: %w", err)
		}
		next, err := node.Evaluate(ctx, childSt, deps)
		if err != nil {
			if errors.Is(err, engineruntime.ErrTerminal) {
				if childSt.DataStore != nil {
					childSt.Vars = workflow.SeedDataSnapshot(childSt.Vars, childSt.DataStore.Snapshot())
				}
				return childSt.Current, nil, childSt.Vars, nil
			}
			return "", nil, nil, fmt.Errorf("workflow body step %q: %w", childSt.Current, err)
		}
		// Apply iteration routing for any for_each/count steps inside the body.
		next, err = routeIteratingStepInGraph(childSt, next, body, deps.Sink)
		if err != nil {
			return "", nil, nil, fmt.Errorf("workflow body step %q: iteration: %w", childSt.Current, err)
		}
		if next == workflow.ReturnSentinel {
			return workflow.ReturnSentinel, childSt.ReturnOutputs, childSt.Vars, nil
		}
		childSt.Current = next
	}
}
