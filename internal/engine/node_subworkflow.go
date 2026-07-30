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
	"github.com/brokenbots/criteria/workflow/lockfile"
)

// nullOutputsForSubworkflow returns a map with every declared subworkflow
// output set to a known null value. This keeps the subworkflow.* namespace
// defined when the callee fails before producing real outputs, so parent
// outcome expressions can reference declared outputs without a try() guard.
func nullOutputsForSubworkflow(body *workflow.FSMGraph) map[string]cty.Value {
	if len(body.OutputOrder) == 0 {
		return nil
	}
	out := make(map[string]cty.Value, len(body.OutputOrder))
	for _, name := range body.OutputOrder {
		out[name] = cty.NullVal(cty.DynamicPseudoType)
	}
	return out
}

// runSubworkflow executes the subworkflow identified by node against the parent
// run state. It evaluates the node's input expressions in the parent scope,
// merges any step-level input overrides (stepInput), seeds the child scope,
// executes the callee FSMGraph to completion, evaluates the callee's declared
// outputs, and returns the output map to the caller.
//
// stepName is the parent step that owns this invocation; it is forwarded to
// adapter lifecycle events so the parent event stream can attribute child
// adapter init failures to the calling step.
//
// stepInput contains per-call input bindings (from the step's input { } block)
// that override the declaration-level bindings in node.Inputs. Pass nil when
// there are no step-level overrides.
func runSubworkflow(ctx context.Context, stepName string, node *workflow.SubworkflowNode, parentSt *RunState, stepInput map[string]cty.Value, deps Deps) (outputs map[string]cty.Value, terminal string, err error) {
	// Evaluate each input expression against the parent scope.
	evalOpts := workflow.DefaultFunctionOptions(parentSt.WorkflowDir)
	inputVals, err := evaluateSubworkflowInputs(node, parentSt.Vars, evalOpts)
	if err != nil {
		return nil, "", fmt.Errorf("subworkflow %q: input evaluation: %w", node.Name, err)
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
		return nil, "", fmt.Errorf("subworkflow %q: %w", node.Name, err)
	}

	// Run the callee FSMGraph to a terminal state using the callee's source
	// directory so that runtime functions (file(), fileexists(), etc.) inside
	// the callee resolve relative paths against the subworkflow directory, not
	// the parent workflow directory.
	calleeDir := node.SourcePath

	// Merge the subworkflow's lockfile into the session manager so that
	// digest-addressed adapter binary resolution works for adapters declared
	// in the subworkflow. The subworkflow has its own lockfile with its own
	// adapter entries (e.g. claude-agent.reviewer) that are absent from the
	// parent's lockfile.
	if prevLF := deps.Sessions.GetLockfile(); deps.Sessions != nil {
		if subLF, readErr := lockfile.ReadFromDir(calleeDir); readErr == nil && subLF != nil {
			merged := mergeLockfiles(prevLF, subLF)
			deps.Sessions.SetLockfile(merged)
			defer deps.Sessions.SetLockfile(prevLF)
		}
	}

	terminal, returnOutputs, finalVars, err := runWorkflowBody(ctx, node.Body, node.BodyEntry, childVars, calleeDir, deps, stepName)
	if err != nil {
		// The caller (evaluateSubworkflowStep in node_step.go) surfaces this
		// error on the parent's event stream as a step-level OnStepOutcome event,
		// naming the parent step, so the failure is scoped to the step and can
		// be routed through the parent's failure arm without publishing a
		// whole-run RunFailed event from this helper.
		//
		// Return a defined output object with every declared output set to null so
		// the parent can read subworkflow.* on the failure path without a try()
		// guard and without getting a bare "unsupported attribute" error.
		return nullOutputsForSubworkflow(node.Body), "", fmt.Errorf("subworkflow %q: %w", node.Name, err)
	}
	// When the callee exited via next = step.return, return the projected outputs
	// directly. returnOutputs may be nil (legitimate empty projection) — in
	// that case return nil rather than falling through to evalRunOutputsAsValues.
	if terminal == workflow.ReturnSentinel {
		return returnOutputs, terminal, nil
	}

	// Evaluate the callee's declared outputs against the final child state,
	// also using the callee's directory for any file-referencing expressions.
	finalSt := &RunState{
		Vars:        finalVars,
		WorkflowDir: calleeDir,
	}
	outputs, err = evalRunOutputsAsValues(node.Body, finalSt)
	if err != nil {
		return nil, "", fmt.Errorf("subworkflow %q: output evaluation: %w", node.Name, err)
	}
	return outputs, terminal, nil
}

// evaluateSubworkflowInputs evaluates each input expression stored in the node
// against the parent's eval context and returns the resulting cty.Value map.
func evaluateSubworkflowInputs(node *workflow.SubworkflowNode, parentVars map[string]cty.Value, opts *workflow.FunctionOptions) (map[string]cty.Value, error) {
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

	// Apply input bindings into the var.* namespace. Null bindings are treated as
	// "not supplied" so the child variable falls back to its declared default;
	// a null binding for a required child variable is caught by checkRequiredVars
	// below rather than failing during conversion.
	if len(inputVals) > 0 {
		varAttrs := cloneVarAttrs(vars["var"])
		for name, val := range inputVals {
			node, declared := body.Variables[name]
			if !declared || val.IsNull() {
				continue
			}
			converted, err := workflow.ConvertVarOverrideValue(val, node)
			if err != nil {
				return nil, fmt.Errorf("subworkflow input %q: %w", name, err)
			}
			varAttrs[name] = converted
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

// cloneVarAttrs returns the attributes of vars["var"] as a mutable map, or an
// empty map when the var scope is missing or not an object.
func cloneVarAttrs(varObj cty.Value) map[string]cty.Value {
	out := map[string]cty.Value{}
	if varObj != cty.NilVal && varObj.Type().IsObjectType() {
		for k := range varObj.Type().AttributeTypes() {
			out[k] = varObj.GetAttr(k)
		}
	}
	return out
}

// buildInputObj converts a flat string→cty.Value map to a cty object value
// for use with checkRequiredVars (which expects a cty.ObjectVal input).
func buildInputObj(inputVals map[string]cty.Value) cty.Value {
	if len(inputVals) == 0 {
		return cty.NilVal
	}
	return cty.ObjectVal(inputVals)
}

// mergeLockfiles returns a new lockfile whose adapter set is the union of base
// and overlay. Entries in overlay take precedence when (type, name) collide.
// base may be nil, in which case overlay is returned directly.
func mergeLockfiles(base, overlay *lockfile.Lockfile) *lockfile.Lockfile {
	if base == nil {
		return overlay
	}
	if overlay == nil {
		return base
	}
	// Index overlay entries by "type.name" for O(1) lookup.
	overKeys := make(map[string]bool, len(overlay.Adapters))
	for i := range overlay.Adapters {
		a := &overlay.Adapters[i]
		overKeys[a.Type+"."+a.Name] = true
	}
	merged := &lockfile.Lockfile{
		SchemaVersion: base.SchemaVersion,
		Adapters:      make([]lockfile.LockedAdapter, 0, len(base.Adapters)+len(overlay.Adapters)),
	}
	for i := range base.Adapters {
		a := &base.Adapters[i]
		if !overKeys[a.Type+"."+a.Name] {
			merged.Adapters = append(merged.Adapters, *a)
		}
	}
	merged.Adapters = append(merged.Adapters, overlay.Adapters...)
	return merged
}
