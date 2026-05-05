package engine

import (
	"context"
	"fmt"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"

	"github.com/brokenbots/criteria/workflow"
)

type switchNode struct {
	node *workflow.SwitchNode
}

func (n *switchNode) Name() string { return n.node.Name }

// Evaluate runs each condition's match expression in declaration order against
// the current run state's eval context. The first condition that evaluates to
// true wins. If no condition matches, the default target is used.
//
// Non-boolean and unknown condition values are skipped (treated as false).
// If a condition fails to evaluate, an error is returned and the engine
// surfaces it via OnRunFailed.
//
// When a matched condition (or the default) declares an output expression, it
// is evaluated and stored as the switch node's step outputs so that subsequent
// nodes can reference switch.<name>.<key> (accessible via steps.<name>.<key>).
//
// When next = "return" is set on a matched condition or the default, the switch
// bubbles the return to the caller (mirrors outcome block behaviour from W15).
func (n *switchNode) Evaluate(ctx context.Context, st *RunState, deps Deps) (string, error) {
	_ = ctx
	ec := workflow.BuildEvalContextWithOpts(st.Vars, workflow.DefaultFunctionOptions(st.WorkflowDir))

	for i, cond := range n.node.Conditions {
		val, diags := cond.Match.Value(ec)
		if diags.HasErrors() {
			return "", fmt.Errorf("switch %q condition[%d]: %s", n.node.Name, i, diags.Error())
		}
		if !val.IsKnown() || val.IsNull() {
			// Unknown or null: skip (treated as false). Sequential execution
			// guarantees prior steps have completed, so unknown values indicate
			// an expression error the author should correct.
			continue
		}
		if val.Type() != cty.Bool {
			return "", fmt.Errorf("switch %q condition[%d]: match must be boolean, got %s", n.node.Name, i, val.Type().FriendlyName())
		}
		if val.True() {
			if err := n.applyOutputProjection(cond.OutputExpr, st, deps); err != nil {
				return "", fmt.Errorf("switch %q condition[%d]: output projection: %w", n.node.Name, i, err)
			}
			matchedLabel := fmt.Sprintf("condition[%d]", i)
			deps.Sink.OnBranchEvaluated(n.node.Name, matchedLabel, cond.Next, cond.MatchSrc)
			return cond.Next, nil
		}
	}

	if err := n.applyOutputProjection(n.node.DefaultOutput, st, deps); err != nil {
		return "", fmt.Errorf("switch %q default: output projection: %w", n.node.Name, err)
	}
	if n.node.DefaultNext == "" {
		return "", fmt.Errorf("switch %q: no condition matched and no default target is configured; add a default block", n.node.Name)
	}
	deps.Sink.OnBranchEvaluated(n.node.Name, "default", n.node.DefaultNext, "")
	return n.node.DefaultNext, nil
}

// applyOutputProjection evaluates the output expression (when non-nil) and
// stores the projected key/value pairs as the switch node's step outputs so
// subsequent nodes can access them via steps.<switchname>.<key>.
func (n *switchNode) applyOutputProjection(expr hcl.Expression, st *RunState, deps Deps) error {
	if expr == nil {
		return nil
	}
	ec := workflow.BuildEvalContextWithOpts(st.Vars, workflow.DefaultFunctionOptions(st.WorkflowDir))
	val, diags := expr.Value(ec)
	if diags.HasErrors() {
		return fmt.Errorf("evaluating output expression: %s", diags.Error())
	}
	if !val.Type().IsObjectType() {
		return fmt.Errorf("output must be an object; got %s", val.Type().FriendlyName())
	}
	projected := make(map[string]string, len(val.Type().AttributeTypes()))
	for k := range val.Type().AttributeTypes() {
		attr := val.GetAttr(k)
		var rendered string
		if attr.Type() == cty.String {
			rendered = attr.AsString()
		} else {
			var err error
			rendered, err = renderCtyValue(attr)
			if err != nil {
				return fmt.Errorf("output key %q: %w", k, err)
			}
		}
		projected[k] = rendered
	}
	st.Vars = workflow.WithStepOutputs(st.Vars, n.node.Name, projected)
	deps.Sink.OnStepOutputCaptured(n.node.Name, projected)
	return nil
}
