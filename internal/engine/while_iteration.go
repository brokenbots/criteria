package engine

// while_iteration.go — sequential condition-driven iteration for while-modified
// steps. evaluateWhile drives the loop: it re-evaluates the while expression
// before each iteration and exits when false (or when on_failure=abort fires
// after a failed iteration).
//
// Key invariants (from W19 lessons):
//   - Every iteration is dispatched via runStepFromAttempt so max_visits,
//     timeout, retry, and fatal-error propagation all apply.
//   - cursor.Total is set to -1 (IsWhile sentinel) so crash-resume can
//     distinguish a while cursor from a bounded for_each/count cursor.
//   - Per-iteration shared_writes are applied via applyIterationSharedWrites
//     (same path as for_each) so the next iteration sees the updated shared.*
//     values when the condition is re-evaluated.

import (
	"context"
	"errors"
	"fmt"

	"github.com/zclconf/go-cty/cty"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/plugin"
	"github.com/brokenbots/criteria/workflow"
)

// evaluateWhile is the entry-point called from stepNode.Evaluate when
// n.step.While != nil. It manages the full while loop lifecycle:
// condition check → iteration body → shared_writes → repeat.
func (n *stepNode) evaluateWhile(ctx context.Context, st *RunState, deps Deps) (string, error) {
	cur := n.whileCursor(st, deps)

	cond, err := n.evaluateWhileCondition(cur, st)
	if err != nil {
		return "", err
	}
	if !cond {
		return n.finishWhileOutcome(cur, st, deps)
	}

	// Bind while.* into vars so input expressions see while.index/first/_prev.
	st.Vars = workflow.WithWhileBinding(st.Vars, &workflow.WhileBinding{
		Index: cur.Index,
		First: cur.Index == 0,
		Prev:  cur.Prev,
	})

	deps.Sink.OnStepIterationStarted(n.step.Name, cur.Index, fmt.Sprintf("while[%d]", cur.Index), cur.AnyFailed)

	next, err := n.runWhileIteration(ctx, st, deps, cur)
	return next, err
}

// whileCursor returns the active while cursor, pushing a new one on first entry.
func (n *stepNode) whileCursor(st *RunState, deps Deps) *workflow.IterCursor {
	cur := st.TopCursor()
	if cur != nil && cur.StepName == n.step.Name {
		return cur
	}
	c := workflow.IterCursor{
		StepName:   n.step.Name,
		Index:      0,
		Total:      -1, // unbounded sentinel
		InProgress: true,
		OnFailure:  n.step.OnFailure,
	}
	st.PushCursor(&c)
	cur = st.TopCursor()
	deps.Sink.OnForEachEntered(n.step.Name, -1)
	if curJSON, err := workflow.SerializeIterCursor(cur); err == nil {
		deps.Sink.OnScopeIterCursorSet(curJSON)
	}
	return cur
}

// evaluateWhileCondition evaluates the while expression and returns true if the
// loop should continue. Returns an error if the expression is invalid.
func (n *stepNode) evaluateWhileCondition(cur *workflow.IterCursor, st *RunState) (bool, error) {
	whileVars := workflow.WithWhileBinding(st.Vars, &workflow.WhileBinding{
		Index: cur.Index,
		First: cur.Index == 0,
		Prev:  cur.Prev,
	})
	evalCtx := workflow.BuildEvalContextWithOpts(whileVars, workflow.DefaultFunctionOptions(st.WorkflowDir))

	condVal, condDiags := n.step.While.Value(evalCtx)
	if condDiags.HasErrors() {
		return false, fmt.Errorf("step %q while: condition expression error: %s", n.step.Name, condDiags.Error())
	}
	if condVal.IsNull() || !condVal.IsKnown() {
		return false, fmt.Errorf("step %q while: condition is null or unknown", n.step.Name)
	}
	if condVal.Type() != cty.Bool {
		return false, fmt.Errorf("step %q while: condition must be bool; got %s", n.step.Name, condVal.Type().FriendlyName())
	}
	return condVal.True(), nil
}

// runWhileIteration executes one iteration and advances or finishes the cursor.
func (n *stepNode) runWhileIteration(ctx context.Context, st *RunState, deps Deps, cur *workflow.IterCursor) (string, error) {
	result, execErr := n.runWhileStep(ctx, st, deps)
	if execErr != nil {
		var fatal *plugin.FatalRunError
		if errors.As(execErr, &fatal) {
			return "", execErr
		}
		var policy *policyLimitError
		if errors.As(execErr, &policy) {
			return "", execErr
		}
		cur.AnyFailed = true
		if cur.OnFailure == "abort" {
			return n.finishWhileOutcome(cur, st, deps)
		}
		if cur.OnFailure == "ignore" {
			cur.AnyFailed = false
		}
		cur.Index++
		n.persistWhileCursor(cur, deps.Sink)
		return n.step.Name, nil
	}

	st.LastOutcome = result.Outcome
	if len(result.Outputs) > 0 {
		key := cty.NumberIntVal(int64(cur.Index))
		st.Vars = workflow.WithIndexedStepOutput(st.Vars, n.step.Name, key, result.Outputs)
		cur.Prev = stringMapToCtyObject(result.Outputs)
	}
	deps.Sink.OnStepOutputCaptured(n.step.Name, result.Outputs)
	deps.Sink.OnStepTransition(n.step.Name, result.Outcome, result.Outcome)

	if err := n.applyIterationSharedWrites(result.Outcome, result.Outputs, st, deps.Sink); err != nil {
		return "", err
	}

	outcomeIsSuccess := isSuccessOutcome(result.Outcome)
	if !outcomeIsSuccess {
		cur.AnyFailed = true
	}
	if cur.OnFailure == "abort" && !outcomeIsSuccess {
		return n.finishWhileOutcome(cur, st, deps)
	}
	if cur.OnFailure == "ignore" && !outcomeIsSuccess {
		cur.AnyFailed = false
	}

	cur.Index++
	n.persistWhileCursor(cur, deps.Sink)
	return n.step.Name, nil
}

// finishWhileOutcome closes out a while loop: pops the cursor, clears while.*
// bindings, emits OnStepIterationCompleted, applies the aggregate outcome's
// output projection and shared_writes, and returns the next node.
func (n *stepNode) finishWhileOutcome(cur *workflow.IterCursor, st *RunState, deps Deps) (string, error) {
	st.PopCursor()
	st.Vars = workflow.ClearWhileBinding(st.Vars)
	deps.Sink.OnScopeIterCursorSet("") // cursor cleared

	aggregateOutcome := "all_succeeded"
	if cur.AnyFailed && cur.OnFailure != "ignore" {
		aggregateOutcome = "any_failed"
	}

	co, ok := n.step.Outcomes[aggregateOutcome]
	if !ok {
		co = n.step.Outcomes["all_succeeded"]
	}

	deps.Sink.OnStepIterationCompleted(n.step.Name, aggregateOutcome, co.Next)

	var projectedCty map[string]cty.Value
	if co.OutputExpr != nil {
		projected, projErr := evalOutcomeOutputProjection(co.OutputExpr, nil, nil, st)
		if projErr != nil {
			return "", fmt.Errorf("step %q aggregate outcome %q: output projection: %w", n.step.Name, aggregateOutcome, projErr)
		}
		projectedCty = projected
		if co.Next == workflow.ReturnSentinel {
			st.ReturnOutputs = projected
		}
	}

	if len(co.SharedWrites) > 0 && st.SharedVarStore != nil {
		if writeErr := applySharedWrites(n.step.Name, aggregateOutcome, co.SharedWrites, projectedCty, nil, st, deps.Sink); writeErr != nil {
			return "", writeErr
		}
	}

	return co.Next, nil
}

// persistWhileCursor serialises the current cursor state and notifies the sink.
func (n *stepNode) persistWhileCursor(cur *workflow.IterCursor, sink Sink) {
	if curJSON, err := workflow.SerializeIterCursor(cur); err == nil {
		sink.OnScopeIterCursorSet(curJSON)
	}
}

// runWhileStep executes one iteration of a while-modified step. For subworkflow
// targets it calls runSubworkflow directly; for adapter targets it delegates to
// runStepFromAttempt (which handles max_visits, timeout, and retry).
func (n *stepNode) runWhileStep(ctx context.Context, st *RunState, deps Deps) (adapter.Result, error) {
	if n.step.TargetKind == workflow.StepTargetSubworkflow {
		return n.runWhileSubworkflowStep(ctx, st, deps)
	}
	effectiveStep, err := n.resolveInput(st.Vars, st.WorkflowDir)
	if err != nil {
		return adapter.Result{}, fmt.Errorf("step %q: input expression error: %w", n.step.Name, err)
	}
	return n.runStepFromAttempt(ctx, st, deps, effectiveStep, 1)
}

// runWhileSubworkflowStep runs one while iteration for a subworkflow-targeted
// step. It increments the visit counter, evaluates step-level input expressions,
// and invokes the callee in a nested engine loop. The outcome is "success" when
// the callee completes without error, "failure" otherwise.
func (n *stepNode) runWhileSubworkflowStep(ctx context.Context, st *RunState, deps Deps) (adapter.Result, error) {
	swNode, ok := n.graph.Subworkflows[n.step.SubworkflowRef]
	if !ok {
		return adapter.Result{}, fmt.Errorf("step %q: subworkflow %q not found", n.step.Name, n.step.SubworkflowRef)
	}
	if err := n.incrementVisit(st); err != nil {
		return adapter.Result{}, err
	}

	var stepInput map[string]cty.Value
	if len(n.step.InputExprs) > 0 {
		evalOpts := workflow.DefaultFunctionOptions(st.WorkflowDir)
		resolved, err := workflow.ResolveInputExprsAsCty(n.step.InputExprs, st.Vars, evalOpts)
		if err != nil {
			return adapter.Result{}, fmt.Errorf("step %q: input expression error: %w", n.step.Name, err)
		}
		stepInput = resolved
	}

	outputs, runErr := runSubworkflow(ctx, swNode, st, stepInput, deps)
	outcome := "success"
	if runErr != nil {
		outcome = "failure"
	}

	stringOutputs := make(map[string]string, len(outputs))
	for k, v := range outputs {
		if v.IsKnown() && v.Type() == cty.String {
			stringOutputs[k] = v.AsString()
			continue
		}
		rendered, err := renderCtyValue(v)
		if err != nil {
			return adapter.Result{}, fmt.Errorf("step %q: subworkflow output %q: %w", n.step.Name, k, err)
		}
		stringOutputs[k] = rendered
	}
	return adapter.Result{Outcome: outcome, Outputs: stringOutputs}, nil
}
