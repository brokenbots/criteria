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

// seedChildVars builds the initial vars map for a workflow body run.
//
// It starts from the body's compiled variable defaults (via SeedVarsFromGraph),
// then applies any parentInput bindings to override var.* values. The body's
// compiled locals (always compile-time constants) are seeded from the graph.
// The parent's each.* binding is threaded through so iteration variables
// remain accessible inside the body without explicit input declaration.
//
// Returns an error when a required body variable (no declared default) is
// absent from parentInput. This is the runtime safety net; the compiler also
// catches the case where no input expression is present at all.
func seedChildVars(body *workflow.FSMGraph, parentInput cty.Value, parentVars map[string]cty.Value) (map[string]cty.Value, error) {
	vars := workflow.SeedVarsFromGraph(body)
	if len(body.Locals) > 0 {
		vars["local"] = workflow.SeedLocalsFromGraph(body)
	}
	vars = overrideVarsFromInput(vars, body, parentInput)
	if err := checkRequiredVars(body, parentInput); err != nil {
		return nil, err
	}
	// Thread each.* from parent scope so iteration variables remain accessible
	// inside the body (read-only; no back-propagation to outer scope).
	if each, ok := parentVars["each"]; ok && each != cty.NilVal {
		vars["each"] = each
	}
	return vars, nil
}

// overrideVarsFromInput applies parentInput object bindings to the var.*
// entries in vars. Only keys that match declared body variables are applied.
// Returns an unmodified vars when parentInput is absent or not an object.
func overrideVarsFromInput(vars map[string]cty.Value, body *workflow.FSMGraph, parentInput cty.Value) map[string]cty.Value {
	if parentInput == cty.NilVal || !parentInput.IsKnown() || parentInput.IsNull() || !parentInput.Type().IsObjectType() {
		return vars
	}
	varObj := vars["var"]
	varAttrs := map[string]cty.Value{}
	if varObj.Type().IsObjectType() {
		for k := range varObj.Type().AttributeTypes() {
			varAttrs[k] = varObj.GetAttr(k)
		}
	}
	for name := range body.Variables {
		if parentInput.Type().HasAttribute(name) {
			varAttrs[name] = parentInput.GetAttr(name)
		}
	}
	if len(varAttrs) > 0 {
		vars["var"] = cty.ObjectVal(varAttrs)
	}
	return vars
}

// checkRequiredVars returns an error if any required body variable (no default)
// lacks a binding in parentInput. This is the runtime complement to the
// compile-time check in compileWorkflowStep.
func checkRequiredVars(body *workflow.FSMGraph, parentInput cty.Value) error {
	hasInput := parentInput != cty.NilVal && parentInput.IsKnown() && !parentInput.IsNull() && parentInput.Type().IsObjectType()
	var missing []string
	for name, node := range body.Variables {
		if node.IsRequired() && !(hasInput && parentInput.Type().HasAttribute(name)) {
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
// engine loop. It returns the terminal state name and the child's final vars
// when the body reaches a terminal state, or an error on fatal conditions.
//
//   - body is the compiled FSMGraph of the sub-workflow body.
//   - bodyEntry is the initial state name for the body run.
//   - childVars is the pre-seeded child scope built by seedChildVars.
//   - workflowDir is forwarded for file() resolution in eval contexts.
//   - deps carries the same session manager and event sink as the outer loop.
//
// When the body reaches "_continue" the caller should treat the iteration as
// successfully completed and advance the cursor. Any other terminal state is
// an early-exit from the iteration; the caller should forward that outcome.
//
// The returned child vars represent the body's final execution scope and are
// used by the caller to evaluate output{} block expressions.
func runWorkflowBody(ctx context.Context, body *workflow.FSMGraph, bodyEntry string, childVars map[string]cty.Value, workflowDir string, deps Deps) (terminal string, finalVars map[string]cty.Value, err error) {
	if bodyEntry == "" {
		bodyEntry = body.InitialState
	}
	if bodyEntry == "" {
		return "", nil, fmt.Errorf("workflow body has no initial state")
	}

	childSt := &RunState{
		Current:       bodyEntry,
		Vars:          childVars,
		WorkflowDir:   workflowDir,
		PendingSignal: "",
		ResumePayload: nil,
		firstStep:     false,
	}

	for {
		node, err := nodeFor(body, childSt.Current)
		if err != nil {
			return "", nil, fmt.Errorf("workflow body: %w", err)
		}
		next, err := node.Evaluate(ctx, childSt, deps)
		if err != nil {
			if errors.Is(err, engineruntime.ErrTerminal) {
				return childSt.Current, childSt.Vars, nil
			}
			return "", nil, fmt.Errorf("workflow body step %q: %w", childSt.Current, err)
		}
		// Apply iteration routing for any for_each/count steps inside the body.
		next = routeIteratingStepInGraph(childSt, next, body, deps.Sink)
		childSt.Current = next
	}
}
