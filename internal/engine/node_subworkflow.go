package engine

// node_subworkflow.go — Subworkflow invocation at runtime (W13, Phase 3).
//
// runSubworkflow executes a pre-compiled SubworkflowNode in a nested engine loop,
// mirroring the runWorkflowBody pattern from node_workflow.go. The input
// expressions (parent-scope HCL) are evaluated against the parent's current
// vars before entering the child scope, and the callee's declared output
// expressions are evaluated against the final child state before returning.
//
// W14 (universal step target) wires the `target = subworkflow.<name>` step
// attribute to call this entry point. Until W14 lands, subworkflow blocks are
// compiled but not yet invokable from a step.

import (
	"context"
	"fmt"

	"github.com/zclconf/go-cty/cty"

	"github.com/brokenbots/criteria/workflow"
)

// runSubworkflow executes the subworkflow identified by node against the parent
// run state. It evaluates the node's input expressions in the parent scope,
// merges any step-level input overrides (stepInput), seeds the child scope,
// executes the callee FSMGraph to completion, evaluates the callee's declared
// outputs, and returns the output map to the caller.
//
// stepInput contains per-call input bindings (from the step's input { } block)
// that override the declaration-level bindings in node.Inputs. Pass nil when
// there are no step-level overrides.
func runSubworkflow(ctx context.Context, node *workflow.SubworkflowNode, parentSt *RunState, stepInput map[string]cty.Value, deps Deps) (map[string]cty.Value, error) {
	// Evaluate each input expression against the parent scope.
	evalOpts := workflow.DefaultFunctionOptions(parentSt.WorkflowDir)
	inputVals, err := evaluateSubworkflowInputs(node, parentSt.Vars, evalOpts)
	if err != nil {
		return nil, fmt.Errorf("subworkflow %q: input evaluation: %w", node.Name, err)
	}

	// Step-level inputs override declaration-level bindings.
	if len(stepInput) > 0 {
		if inputVals == nil {
			inputVals = make(map[string]cty.Value, len(stepInput))
		}
		for k, v := range stepInput {
			inputVals[k] = v
		}
	}

	// Seed the child scope: start from the callee's variable defaults, then
	// apply the evaluated input bindings.
	childVars, err := seedChildVarsFromBindings(node.Body, inputVals, parentSt.Vars)
	if err != nil {
		return nil, fmt.Errorf("subworkflow %q: %w", node.Name, err)
	}

	// Run the callee FSMGraph to a terminal state using the callee's source
	// directory so that runtime functions (file(), fileexists(), etc.) inside
	// the callee resolve relative paths against the subworkflow directory, not
	// the parent workflow directory.
	calleeDir := node.SourcePath
	terminal, returnOutputs, finalVars, err := runWorkflowBody(ctx, node.Body, node.BodyEntry, childVars, calleeDir, deps)
	if err != nil {
		return nil, fmt.Errorf("subworkflow %q: %w", node.Name, err)
	}
	// When the callee exited via next = "return", return the projected outputs
	// directly. returnOutputs may be nil (legitimate empty projection) — in
	// that case return nil rather than falling through to evalRunOutputsAsValues.
	if terminal == workflow.ReturnSentinel {
		return returnOutputs, nil
	}

	// Evaluate the callee's declared outputs against the final child state,
	// also using the callee's directory for any file-referencing expressions.
	finalSt := &RunState{
		Vars:        finalVars,
		WorkflowDir: calleeDir,
	}
	outputs, err := evalRunOutputsAsValues(node.Body, finalSt)
	if err != nil {
		return nil, fmt.Errorf("subworkflow %q: output evaluation: %w", node.Name, err)
	}
	return outputs, nil
}

// evaluateSubworkflowInputs evaluates each input expression stored in the node
// against the parent's eval context and returns the resulting cty.Value map.
func evaluateSubworkflowInputs(node *workflow.SubworkflowNode, parentVars map[string]cty.Value, opts workflow.FunctionOptions) (map[string]cty.Value, error) {
	if len(node.Inputs) == 0 {
		return nil, nil
	}
	evalCtx := workflow.BuildEvalContextWithOpts(parentVars, opts)
	result := make(map[string]cty.Value, len(node.Inputs))
	for key, expr := range node.Inputs {
		val, diags := expr.Value(evalCtx)
		if diags.HasErrors() {
			return nil, fmt.Errorf("input %q: %s", key, diags.Error())
		}
		result[key] = val
	}
	return result, nil
}

// seedChildVarsFromBindings builds the initial vars map for a subworkflow run.
// It starts from the callee's compiled variable defaults (via SeedVarsFromGraph),
// then applies inputVals (already-evaluated cty.Value bindings) to var.* entries.
func seedChildVarsFromBindings(body *workflow.FSMGraph, inputVals, parentVars map[string]cty.Value) (map[string]cty.Value, error) {
	vars := workflow.SeedVarsFromGraph(body)
	if len(body.Locals) > 0 {
		vars["local"] = workflow.SeedLocalsFromGraph(body)
	}

	// Apply input bindings into the var.* namespace.
	if len(inputVals) > 0 {
		varObj := vars["var"]
		varAttrs := map[string]cty.Value{}
		if varObj.Type().IsObjectType() {
			for k := range varObj.Type().AttributeTypes() {
				varAttrs[k] = varObj.GetAttr(k)
			}
		}
		for name, val := range inputVals {
			if _, declared := body.Variables[name]; declared {
				varAttrs[name] = val
			}
		}
		if len(varAttrs) > 0 {
			vars["var"] = cty.ObjectVal(varAttrs)
		}
	}

	if err := checkRequiredVars(body, buildInputObj(inputVals)); err != nil {
		return nil, err
	}

	// Thread each.* from parent scope so iteration variables remain accessible
	// inside the subworkflow (read-only; no back-propagation to outer scope).
	if each, ok := parentVars["each"]; ok && each != cty.NilVal {
		vars["each"] = each
	}

	return vars, nil
}

// buildInputObj converts a flat string→cty.Value map to a cty object value
// for use with checkRequiredVars (which expects a cty.ObjectVal input).
func buildInputObj(inputVals map[string]cty.Value) cty.Value {
	if len(inputVals) == 0 {
		return cty.NilVal
	}
	return cty.ObjectVal(inputVals)
}
