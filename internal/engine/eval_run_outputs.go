package engine

import (
	"fmt"

	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/convert"
	"github.com/zclconf/go-cty/cty/json"

	"github.com/brokenbots/criteria/workflow"
)

// evalRunOutputs evaluates each declared output expression against the final
// run state and returns the resolved values keyed by output name in
// declaration order. Returns (outputs list, error).
//
// Each output is a map with keys: "name", "value" (string-rendered), "declared_type".
// If a declared type is set and the resolved value's type does not match,
// an error is returned with the run terminating in failure.
func evalRunOutputs(g *workflow.FSMGraph, st *RunState) ([]map[string]string, error) {
	if len(g.Outputs) == 0 {
		return nil, nil
	}

	result := make([]map[string]string, 0, len(g.Outputs))

	// Build evaluation context with current run state.
	// st.Vars carries var.*, steps.*, local.*, and each.* (when in scope);
	// BuildEvalContextWithOpts unpacks them into the eval context.
	evalCtx := workflow.BuildEvalContextWithOpts(st.Vars, workflow.DefaultFunctionOptions(st.WorkflowDir))

	// Evaluate each output in declaration order.
	for _, name := range g.OutputOrder {
		on := g.Outputs[name]

		// Evaluate the value expression.
		val, diags := on.Value.Value(evalCtx)
		if diags.HasErrors() {
			return nil, fmt.Errorf("output %q: evaluation failed: %s", name, diags.Error())
		}

		// Check type match if declared type is set, using cty conversion semantics.
		if on.DeclaredType != cty.NilType {
			converted, err := convert.Convert(val, on.DeclaredType)
			if err != nil {
				return nil, fmt.Errorf("output %q: value of type %s is not assignable to declared type %s: %w",
					name, val.Type().FriendlyName(), on.DeclaredType.FriendlyName(), err)
			}
			val = converted
		}

		// Render the value as a JSON string for transport.
		valueStr, err := renderCtyValue(val)
		if err != nil {
			return nil, fmt.Errorf("output %q: render failed: %w", name, err)
		}

		// Build declared type string (empty if not set).
		declaredTypeStr := ""
		if on.DeclaredType != cty.NilType {
			declaredTypeStr = workflow.TypeToString(on.DeclaredType)
		}

		result = append(result, map[string]string{
			"name":          name,
			"value":         valueStr,
			"declared_type": declaredTypeStr,
		})
	}

	return result, nil
}

// renderCtyValue converts a cty.Value to a string representation suitable for
// transport (JSON encoding for most types, friendly string for others).
func renderCtyValue(val cty.Value) (string, error) {
	// For unknown values, use null.
	if !val.IsKnown() {
		return "null", nil
	}

	// Marshal as JSON using cty's JSON encoder.
	jsonBytes, err := json.Marshal(val, val.Type())
	if err != nil {
		return "", err
	}
	return string(jsonBytes), nil
}
