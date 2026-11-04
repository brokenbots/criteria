package engine

import (
	stdjson "encoding/json"
	"fmt"

	"github.com/hashicorp/hcl/v2/ext/typeexpr"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/convert"
	ctyjson "github.com/zclconf/go-cty/cty/json"
	structpb "google.golang.org/protobuf/types/known/structpb"

	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
	"github.com/brokenbots/criteria/workflow"
)

// evalRunOutputs evaluates each declared output expression against the final
// run state and returns the resolved values as RunOutputs.Output messages in
// declaration order. Returns (outputs list, error).
//
// Each Output carries the output name, the display-only value render, the
// declared type string, and the natively typed typed_value form. If a declared
// type is set and the resolved value's type does not match, an error is
// returned with the run terminating in failure.
func evalRunOutputs(g *workflow.FSMGraph, st *RunState) ([]*pb.RunOutputs_Output, error) {
	if len(g.Outputs) == 0 {
		return nil, nil
	}

	result := make([]*pb.RunOutputs_Output, 0, len(g.Outputs))

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
			val = workflow.ApplyDefaultsIfAny(val, on.TypeDefaults)
			converted, err := convert.Convert(val, on.DeclaredType)
			if err != nil {
				return nil, fmt.Errorf("output %q: value of type %s is not assignable to declared type %s: %w",
					name, val.Type().FriendlyName(), on.DeclaredType.FriendlyName(), err)
			}
			val = converted
		}

		// Render the value as a JSON string for the display-only transport.
		valueStr, err := renderCtyValue(val)
		if err != nil {
			return nil, fmt.Errorf("output %q: render failed: %w", name, err)
		}

		// Build declared type string (empty if not set).
		declaredTypeStr := ""
		if on.DeclaredType != cty.NilType {
			// typeexpr.TypeString handles all cty types; types are validated at compile time.
			declaredTypeStr = typeexpr.TypeString(on.DeclaredType)
		}

		result = append(result, &pb.RunOutputs_Output{
			Name:         name,
			Value:        valueStr,
			DeclaredType: declaredTypeStr,
			TypedValue:   ctyValueToStructPb(val),
		})
	}

	return result, nil
}

// evalRunOutputsAsValues evaluates each declared output expression against the
// final run state and returns the resolved cty.Value keyed by output name.
// This is the internal form used by runSubworkflow to return output values to
// the parent caller; use evalRunOutputs for the event (display + typed_value)
// form.
func evalRunOutputsAsValues(g *workflow.FSMGraph, st *RunState) (map[string]cty.Value, error) {
	if len(g.Outputs) == 0 {
		return nil, nil
	}
	evalCtx := workflow.BuildEvalContextWithOpts(st.Vars, workflow.DefaultFunctionOptions(st.WorkflowDir))
	result := make(map[string]cty.Value, len(g.Outputs))
	for _, name := range g.OutputOrder {
		on := g.Outputs[name]
		val, diags := on.Value.Value(evalCtx)
		if diags.HasErrors() {
			return nil, fmt.Errorf("output %q: evaluation failed: %s", name, diags.Error())
		}
		if on.DeclaredType != cty.NilType {
			val = workflow.ApplyDefaultsIfAny(val, on.TypeDefaults)
			converted, err := convert.Convert(val, on.DeclaredType)
			if err != nil {
				return nil, fmt.Errorf("output %q: value of type %s is not assignable to declared type %s: %w",
					name, val.Type().FriendlyName(), on.DeclaredType.FriendlyName(), err)
			}
			val = converted
		}
		result[name] = val
	}
	return result, nil
}

// renderCtyValue converts a cty.Value to a string representation suitable for
// the display-only value field (JSON encoding for most types, friendly
// string for others).
func renderCtyValue(val cty.Value) (string, error) {
	// For unknown values, use null.
	if !val.IsKnown() {
		return "null", nil
	}

	// Marshal as JSON using cty's JSON encoder.
	jsonBytes, err := ctyjson.Marshal(val, val.Type())
	if err != nil {
		return "", err
	}
	return string(jsonBytes), nil
}

// ctyValueToStructPb converts a cty value to the google.protobuf.Value form
// for RunOutputs.Output.typed_value, so typed consumers read the value's
// structure natively instead of parsing the display render. Unknown and
// unconvertible values render null, matching the display render's null. Cty
// numbers lose their big.Float precision in transit (google.protobuf.Value
// numbers are doubles); the display render preserves exactness for consumers
// that need it.
func ctyValueToStructPb(val cty.Value) *structpb.Value {
	if !val.IsKnown() {
		return structpb.NewNullValue()
	}
	jsonBytes, err := ctyjson.Marshal(val, val.Type())
	if err != nil {
		return structpb.NewNullValue()
	}
	var native any
	if err := stdjson.Unmarshal(jsonBytes, &native); err != nil {
		return structpb.NewNullValue()
	}
	pv, err := structpb.NewValue(native)
	if err != nil {
		return structpb.NewNullValue()
	}
	return pv
}
