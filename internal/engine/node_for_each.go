package engine

import (
	"context"
	"fmt"

	"github.com/brokenbots/overseer/workflow"
	"github.com/zclconf/go-cty/cty"
)

// forEachNode implements the for_each loop construct (W07). It evaluates the
// items expression, dispatches the do-step once per item, and emits the
// aggregate outcome once all items have been processed (or an item's step
// exits via a non-_continue transition).
type forEachNode struct {
	node *workflow.ForEachNode
}

func (n *forEachNode) Name() string { return n.node.Name }

// Evaluate drives a single step of the for_each state machine:
//
//   - On first entry (Iter == nil or NodeName mismatch): evaluates the items
//     expression, creates the IterCursor, and emits OnForEachEntered.
//   - On re-entry with Items == nil (crash recovery): re-evaluates items
//     expression using the restored cursor's Index and AnyFailed.
//   - When Index < len(Items): binds each.value / each.index in rs.Vars,
//     emits OnForEachIteration, and returns the do-step name.
//   - When Index >= len(Items): emits the aggregate outcome, clears the
//     cursor, and returns the outcome's target.
func (n *forEachNode) Evaluate(ctx context.Context, st *RunState, deps Deps) (string, error) {
	// Entering the for_each fresh (no active cursor for this node).
	if st.Iter == nil || st.Iter.NodeName != n.node.Name {
		v, diags := n.node.Items.Value(workflow.BuildEvalContext(st.Vars))
		if diags.HasErrors() {
			return "", fmt.Errorf("for_each %q: items evaluation failed: %s", n.node.Name, diags.Error())
		}
		if !v.Type().IsListType() && !v.Type().IsTupleType() {
			return "", fmt.Errorf("for_each %q: items must be a list or tuple, got %s", n.node.Name, v.Type().FriendlyName())
		}
		items := v.AsValueSlice()
		if items == nil {
			// AsValueSlice returns nil for an empty list; normalise to an empty
			// slice so that Items == nil unambiguously means "not yet evaluated".
			items = []cty.Value{}
		}
		st.Iter = &workflow.IterCursor{
			NodeName: n.node.Name,
			Items:    items,
			Index:    0,
		}
		deps.Sink.OnForEachEntered(n.node.Name, len(items))
	} else if st.Iter.Items == nil {
		// Crash recovery: cursor was restored from scope JSON without Items.
		// Re-evaluate the items expression with the current var scope.
		v, diags := n.node.Items.Value(workflow.BuildEvalContext(st.Vars))
		if diags.HasErrors() {
			return "", fmt.Errorf("for_each %q: items re-evaluation failed after recovery: %s", n.node.Name, diags.Error())
		}
		if !v.Type().IsListType() && !v.Type().IsTupleType() {
			return "", fmt.Errorf("for_each %q: items must be a list or tuple, got %s", n.node.Name, v.Type().FriendlyName())
		}
		items := v.AsValueSlice()
		if items == nil {
			items = []cty.Value{}
		}
		st.Iter.Items = items
	}

	// Iteration complete: emit aggregate outcome and clear the cursor.
	if st.Iter.Index >= len(st.Iter.Items) {
		outcome := "all_succeeded"
		if st.Iter.AnyFailed {
			outcome = "any_failed"
		}
		target, ok := n.node.Outcomes[outcome]
		if !ok {
			// any_failed not declared; fall back to all_succeeded.
			target, ok = n.node.Outcomes["all_succeeded"]
			if !ok {
				return "", fmt.Errorf("for_each %q: required outcome %q is not declared", n.node.Name, "all_succeeded")
			}
		}
		iterName := st.Iter.NodeName
		st.Iter = nil
		deps.Sink.OnScopeIterCursorSet("") // cursor cleared
		deps.Sink.OnForEachOutcome(iterName, outcome, target)
		return target, nil
	}

	// Dispatch the per-iteration step. Mark InProgress before emitting the
	// cursor-set event so the persisted cursor reflects the running state.
	item := st.Iter.Items[st.Iter.Index]
	itemStr := workflow.CtyValueToString(item)
	st.Vars = workflow.WithEachBinding(st.Vars, item, st.Iter.Index)
	st.Iter.InProgress = true
	if cursorJSON, err := workflow.SerializeIterCursor(st.Iter); err == nil {
		deps.Sink.OnScopeIterCursorSet(cursorJSON)
	}
	deps.Sink.OnForEachIteration(n.node.Name, st.Iter.Index, itemStr, st.Iter.AnyFailed)
	return n.node.Do, nil
}
