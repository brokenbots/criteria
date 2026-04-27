package engine

import (
	"context"
	"fmt"

	"github.com/brokenbots/overlord/workflow"
	"github.com/zclconf/go-cty/cty"
)

type branchNode struct {
	node *workflow.BranchNode
}

func (n *branchNode) Name() string { return n.node.Name }

// Evaluate runs each arm condition in declaration order against the current
// run state's eval context. The first arm that evaluates to true wins.
// If no arm matches, the default target is used.
//
// Non-boolean and unknown condition values are skipped (treated as false).
// If an arm condition fails to evaluate, an error is returned and the engine
// surfaces it via OnRunFailed.
func (n *branchNode) Evaluate(ctx context.Context, st *RunState, deps Deps) (string, error) {
	ec := workflow.BuildEvalContext(st.Vars)

	for i, arm := range n.node.Arms {
		val, diags := arm.Condition.Value(ec)
		if diags.HasErrors() {
			return "", fmt.Errorf("branch %q arm[%d]: %s", n.node.Name, i, diags.Error())
		}
		if !val.IsKnown() || val.IsNull() {
			// Unknown or null values are silently skipped (treated as false). In
			// W06/Phase 1.5, sequential execution guarantees that referenced step
			// outputs are available by the time the branch is reached. If parallel
			// execution is introduced (W07+ or beyond), unknown values may appear
			// when a preceding step has not yet completed; this skip semantic could
			// lead to unexpected default-branch selection. Revisit for W07.
			continue
		}
		if val.Type() != cty.Bool {
			return "", fmt.Errorf("branch %q arm[%d]: condition must be boolean, got %s", n.node.Name, i, val.Type().FriendlyName())
		}
		if val.True() {
			matchedArm := fmt.Sprintf("arm[%d]", i)
			deps.Sink.OnBranchEvaluated(n.node.Name, matchedArm, arm.Target, arm.ConditionSrc)
			return arm.Target, nil
		}
	}

	deps.Sink.OnBranchEvaluated(n.node.Name, "default", n.node.DefaultTarget, "")
	return n.node.DefaultTarget, nil
}
