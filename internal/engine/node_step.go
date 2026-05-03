package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/plugin"
	"github.com/brokenbots/criteria/workflow"
)

type stepNode struct {
	graph *workflow.FSMGraph
	step  *workflow.StepNode
}

func (n *stepNode) Name() string {
	return n.step.Name
}

func (n *stepNode) Evaluate(ctx context.Context, st *RunState, deps Deps) (string, error) {
	st.TotalSteps++
	if st.TotalSteps > n.graph.Policy.MaxTotalSteps {
		return "", fmt.Errorf("policy.max_total_steps exceeded (%d)", n.graph.Policy.MaxTotalSteps)
	}

	// Handle step-level iteration (for_each or count).
	if n.step.ForEach != nil || n.step.Count != nil {
		return n.evaluateIterating(ctx, st, deps)
	}

	// Non-iterating step: normal execution path.
	return n.evaluateOnce(ctx, st, deps)
}

// evaluateIterating handles first-entry cursor setup and per-iteration
// execution for steps with for_each or count.
func (n *stepNode) evaluateIterating(ctx context.Context, st *RunState, deps Deps) (string, error) {
	// Check for an existing cursor for this step (re-entry or resumed run).
	cur := st.TopCursor()
	if cur == nil || cur.StepName != n.step.Name {
		// First entry: set up the cursor.
		target, done, err := n.setupIterCursor(ctx, st, deps)
		if err != nil || done {
			return target, err
		}
		// Cursor pushed; cur now points to it.
		cur = st.TopCursor()
	} else if cur.InProgress && len(cur.Items) == 0 {
		// Resumed with a cursor that has no items (crash-resume path).
		// Re-evaluate the expression to repopulate Items.
		if err := n.repopulateCursorItems(ctx, st, cur); err != nil {
			return "", err
		}
	}

	// Run one iteration (first or Nth); cur.InProgress is set by setupIterCursor
	// or routeIteratingStep (on re-entry).
	return n.runOneIteration(ctx, st, deps, cur)
}

// repopulateCursorItems re-evaluates the for_each/count expression and fills
// cur.Items and cur.Keys. This is needed on crash-resume when the cursor was
// serialized without items (items and keys are intentionally not persisted).
func (n *stepNode) repopulateCursorItems(ctx context.Context, st *RunState, cur *workflow.IterCursor) error {
	_ = ctx
	items, keys, err := n.buildIterItems(st)
	if err != nil {
		return fmt.Errorf("step %q: expression error on resume: %w", n.step.Name, err)
	}
	cur.Items = items
	if len(keys) > 0 {
		cur.Keys = keys
	}
	if cur.Total == 0 {
		cur.Total = len(items)
	}
	// Re-bind each.* for the current index.
	var key cty.Value
	if cur.Index < len(cur.Keys) {
		key = cur.Keys[cur.Index]
	} else {
		key = cty.StringVal(fmt.Sprintf("%d", cur.Index))
	}
	if cur.Index < len(items) {
		st.Vars = workflow.WithEachBinding(st.Vars, &workflow.EachBinding{
			Value: items[cur.Index],
			Key:   key,
			Index: cur.Index,
			Total: cur.Total,
			First: cur.Index == 0,
			Last:  cur.Index == cur.Total-1,
			Prev:  cur.Prev,
		})
	}
	return nil
}

// buildIterItems evaluates the for_each/count expression and returns the
// ordered list of iteration items along with the map keys (non-nil only when
// iterating over an HCL object/map). Returns an error if the expression is
// invalid or produces an unexpected type.
func (n *stepNode) buildIterItems(st *RunState) (items, keys []cty.Value, err error) {
	evalCtx := workflow.BuildEvalContextWithOpts(st.Vars, workflow.DefaultFunctionOptions(st.WorkflowDir))
	if n.step.Count != nil {
		return buildCountItems(n.step.Count, evalCtx)
	}
	return buildForEachItems(n.step.ForEach, evalCtx)
}

// buildCountItems expands a count = N expression into N numeric iteration items.
func buildCountItems(expr hcl.Expression, evalCtx *hcl.EvalContext) (items, keys []cty.Value, err error) {
	v, diags := expr.Value(evalCtx)
	if diags.HasErrors() {
		return nil, nil, fmt.Errorf("count expression error: %s", diags.Error())
	}
	if v.IsNull() || !v.IsKnown() {
		return nil, nil, fmt.Errorf("count expression evaluated to null/unknown")
	}
	if !v.Type().Equals(cty.Number) {
		return nil, nil, fmt.Errorf("count expression must be a number; got %s", v.Type().FriendlyName())
	}
	bf := v.AsBigFloat()
	if !bf.IsInt() {
		return nil, nil, fmt.Errorf("count expression must be a whole number; got fractional value")
	}
	n64, _ := bf.Int64()
	if n64 < 0 {
		return nil, nil, fmt.Errorf("count expression must be non-negative; got %d", n64)
	}
	items = make([]cty.Value, n64)
	for i := int64(0); i < n64; i++ {
		items[i] = cty.NumberIntVal(i)
	}
	return items, nil, nil
}

// buildForEachItems expands a for_each = <expr> into ordered items and map keys.
// Keys is non-nil only for map/object iteration.
func buildForEachItems(expr hcl.Expression, evalCtx *hcl.EvalContext) (items, keys []cty.Value, err error) {
	v, diags := expr.Value(evalCtx)
	if diags.HasErrors() {
		return nil, nil, fmt.Errorf("for_each expression error: %s", diags.Error())
	}
	if v.IsNull() || !v.IsKnown() {
		return nil, nil, fmt.Errorf("for_each expression evaluated to null/unknown")
	}
	if !v.CanIterateElements() {
		return nil, nil, fmt.Errorf("for_each expression must evaluate to a list, tuple, or map; got %s", v.Type().FriendlyName())
	}
	isMap := v.Type().IsObjectType() || v.Type().IsMapType()
	for it := v.ElementIterator(); it.Next(); {
		k, elem := it.Element()
		items = append(items, elem)
		if isMap {
			keys = append(keys, k)
		}
	}
	return items, keys, nil
}

// setupIterCursor evaluates the for_each/count expression, initialises the
// IterCursor, and binds each.* for the first item. Returns (target, true, nil)
// when the expression evaluates to an empty collection (no iterations needed).
func (n *stepNode) setupIterCursor(ctx context.Context, st *RunState, deps Deps) (target string, done bool, err error) {
	_ = ctx
	items, keys, err := n.buildIterItems(st)
	if err != nil {
		return "", false, fmt.Errorf("step %q: %w", n.step.Name, err)
	}

	total := len(items)
	deps.Sink.OnForEachEntered(n.step.Name, total)

	if total == 0 {
		// Empty collection: emit all_succeeded immediately.
		t := n.step.Outcomes["all_succeeded"]
		deps.Sink.OnStepIterationCompleted(n.step.Name, "all_succeeded", t)
		return t, true, nil
	}

	// Determine the key for the first item.
	var firstKey cty.Value
	if len(keys) > 0 {
		firstKey = keys[0]
	} else {
		firstKey = cty.StringVal("0")
	}

	// Build and push the cursor.
	cursor := workflow.IterCursor{
		StepName:   n.step.Name,
		Items:      items,
		Keys:       keys,
		Index:      0,
		Total:      total,
		Key:        firstKey,
		InProgress: true,
		OnFailure:  n.step.OnFailure,
	}
	st.PushCursor(&cursor)

	// Persist cursor so that a crash during the first iteration is recoverable.
	if curJSON, serErr := workflow.SerializeIterCursor(st.TopCursor()); serErr == nil {
		deps.Sink.OnScopeIterCursorSet(curJSON)
	}

	// Bind each.* for first item.
	st.Vars = workflow.WithEachBinding(st.Vars, &workflow.EachBinding{
		Value: items[0],
		Key:   firstKey,
		Index: 0,
		Total: total,
		First: true,
		Last:  total == 1,
		Prev:  cty.NilVal,
	})

	deps.Sink.OnStepIterationStarted(n.step.Name, 0, workflow.CtyValueToString(items[0]), false)
	return "", false, nil
}

// runOneIteration executes the step body/adapter for the current iteration
// and returns the raw outcome (which routeIteratingStep will intercept).
func (n *stepNode) runOneIteration(ctx context.Context, st *RunState, deps Deps, cur *workflow.IterCursor) (string, error) {
	if n.step.Type == "workflow" {
		return n.runWorkflowIteration(ctx, st, deps, cur)
	}
	return n.evaluateOnce(ctx, st, deps)
}

// runWorkflowIteration executes the inline workflow body for one iteration
// and records output block values into vars and cur.Prev (B-05, B-06).
func (n *stepNode) runWorkflowIteration(ctx context.Context, st *RunState, deps Deps, cur *workflow.IterCursor) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	// W07: workflow-type iterations skip runStepFromAttempt; count the visit here.
	if err := n.incrementVisit(st); err != nil {
		return "", err
	}

	if n.step.Body == nil {
		return "", fmt.Errorf("step %q: type=\"workflow\" but body is nil", n.step.Name)
	}

	// Evaluate the optional body input expression to build the child var scope.
	var parentInput cty.Value
	if n.step.BodyInputExpr != nil {
		evalCtx := workflow.BuildEvalContextWithOpts(st.Vars, workflow.DefaultFunctionOptions(st.WorkflowDir))
		var diags hcl.Diagnostics
		parentInput, diags = n.step.BodyInputExpr.Value(evalCtx)
		if diags.HasErrors() {
			return "", fmt.Errorf("step %q: body input expression error: %s", n.step.Name, diags.Error())
		}
	}

	childVars, err := seedChildVars(n.step.Body, parentInput, st.Vars)
	if err != nil {
		return "", fmt.Errorf("step %q: %w", n.step.Name, err)
	}

	bodyOutcome, childFinalVars, err := runWorkflowBody(ctx, n.step.Body, n.step.BodyEntry, childVars, st.WorkflowDir, deps)
	if err != nil {
		return "", err
	}
	// "_continue" is the normal-completion signal from a workflow body.
	// Translate it to "success" so that isSuccessOutcome works correctly in
	// routeIteratingStep. Any other terminal state (e.g. "failed") is an
	// early-exit that signals routeIteratingStep to stop the loop immediately.
	outcome := bodyOutcome
	if outcome == "_continue" {
		outcome = "success"
	} else {
		cur.EarlyExit = true
	}
	st.LastOutcome = outcome

	n.applyWorkflowBodyOutputs(st, cur, childFinalVars)
	return outcome, nil
}

// applyWorkflowBodyOutputs evaluates the output{} block expressions against
// the child's final scope and records the resulting values into the outer
// vars (via WithIndexedStepOutput) and into cur.Prev for each.prev access.
func (n *stepNode) applyWorkflowBodyOutputs(st *RunState, cur *workflow.IterCursor, childFinalVars map[string]cty.Value) {
	if len(n.step.Outputs) == 0 {
		return
	}
	evalCtx := workflow.BuildEvalContextWithOpts(childFinalVars, workflow.DefaultFunctionOptions(st.WorkflowDir))
	stringOuts := make(map[string]string, len(n.step.Outputs))
	ctyOuts := make(map[string]cty.Value, len(n.step.Outputs))
	for k, expr := range n.step.Outputs {
		v, diags := expr.Value(evalCtx)
		if !diags.HasErrors() {
			ctyOuts[k] = v
			stringOuts[k] = workflow.CtyValueToString(v)
		}
	}
	if len(stringOuts) > 0 {
		st.Vars = workflow.WithIndexedStepOutput(st.Vars, n.step.Name, iterOutputKey(cur), stringOuts)
		cur.Prev = cty.ObjectVal(ctyOuts)
	}
}

// evaluateOnce executes the step for a single non-iterating invocation (or a
// single iteration invocation for adapter/agent steps).
func (n *stepNode) evaluateOnce(ctx context.Context, st *RunState, deps Deps) (string, error) {
	effectiveStep, resolveErr := n.resolveInput(st.Vars, st.WorkflowDir)
	if resolveErr != nil {
		return "", fmt.Errorf("step %q: input expression error: %w", n.step.Name, resolveErr)
	}

	startAttempt := 1
	if st.firstStep {
		st.firstStep = false
		if st.firstStepAttempt > 1 {
			startAttempt = st.firstStepAttempt
		}
	}

	result, err := n.runStepFromAttempt(ctx, st, deps, effectiveStep, startAttempt)
	if err != nil {
		return "", err
	}

	st.LastOutcome = result.Outcome

	// For iterating steps: skip the Outcomes lookup — routeIteratingStep
	// handles routing based on st.LastOutcome. Record the step output as this
	// iteration's indexed output and as cur.Prev for the next iteration's
	// each._prev binding (B-05, B-06).
	//
	// WithStepOutputs is intentionally skipped here: it stores a flat
	// (non-indexed) object that would overwrite the accumulated indexed outputs
	// from previous iterations. WithIndexedStepOutput accumulates correctly
	// across iterations and is the only writer for iterating steps.
	if st.TopCursor() != nil && st.TopCursor().StepName == n.step.Name {
		cur := st.TopCursor()
		if len(result.Outputs) > 0 {
			st.Vars = workflow.WithIndexedStepOutput(st.Vars, n.step.Name, iterOutputKey(cur), result.Outputs)
			cur.Prev = stringMapToCtyObject(result.Outputs)
		}
		deps.Sink.OnStepOutputCaptured(n.step.Name, result.Outputs)
		deps.Sink.OnStepTransition(n.step.Name, result.Outcome, result.Outcome)
		return result.Outcome, nil
	}

	// Capture step outputs into run vars and notify sink (W04).
	if len(result.Outputs) > 0 {
		st.Vars = workflow.WithStepOutputs(st.Vars, n.step.Name, result.Outputs)
		deps.Sink.OnStepOutputCaptured(n.step.Name, result.Outputs)
	}

	next, ok := n.step.Outcomes[result.Outcome]
	if !ok {
		return "", fmt.Errorf("step %q produced unmapped outcome %q", n.step.Name, result.Outcome)
	}
	deps.Sink.OnStepTransition(n.step.Name, next, result.Outcome)
	return next, nil
}

// resolveInput returns the step with Input populated from evaluated HCL
// expressions. It returns an error if any expression fails to evaluate so
// the caller can fail fast rather than silently using a placeholder value.
func (n *stepNode) resolveInput(vars map[string]cty.Value, workflowDir string) (*workflow.StepNode, error) {
	if len(n.step.InputExprs) == 0 {
		return n.step, nil
	}
	resolved, err := workflow.ResolveInputExprsWithOpts(n.step.InputExprs, vars, workflow.DefaultFunctionOptions(workflowDir))
	if err != nil {
		return nil, err
	}
	if resolved == nil {
		return n.step, nil
	}
	// Merge: expression-resolved values override compiled Input placeholders.
	merged := make(map[string]string, len(n.step.Input))
	for k, v := range n.step.Input {
		merged[k] = v
	}
	for k, v := range resolved {
		merged[k] = v
	}
	cp := *n.step
	cp.Input = merged
	return &cp, nil
}

// incrementVisit checks the max_visits limit and increments the visit counter
// for this step. Returns an error if the limit would be exceeded (W07).
func (n *stepNode) incrementVisit(st *RunState) error {
	if n.step.MaxVisits > 0 {
		if st.Visits == nil {
			st.Visits = make(map[string]int)
		}
		if st.Visits[n.step.Name] >= n.step.MaxVisits {
			return fmt.Errorf("step %q exceeded max_visits (%d)", n.step.Name, n.step.MaxVisits)
		}
	}
	if st.Visits == nil {
		st.Visits = make(map[string]int)
	}
	st.Visits[n.step.Name]++
	return nil
}

func (n *stepNode) runStepFromAttempt(ctx context.Context, st *RunState, deps Deps, step *workflow.StepNode, startAttempt int) (adapter.Result, error) {
	maxAttempts := 1 + n.graph.Policy.MaxStepRetries
	if startAttempt > maxAttempts {
		return adapter.Result{}, fmt.Errorf("step %q has no remaining attempts (start attempt %d exceeds max %d)", step.Name, startAttempt, maxAttempts)
	}

	var lastErr error
	for attempt := startAttempt; attempt <= maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return adapter.Result{}, err
		}

		// W07: each attempt (including retries) counts as one visit toward max_visits.
		if err := n.incrementVisit(st); err != nil {
			return adapter.Result{}, err
		}

		deps.Sink.OnStepEntered(step.Name, n.stepAdapterName(), attempt)

		stepCtx := ctx
		var cancel context.CancelFunc
		if step.Timeout > 0 {
			stepCtx, cancel = context.WithTimeout(ctx, step.Timeout)
		}

		start := time.Now()
		result, err := n.executeStep(stepCtx, deps, step)
		if cancel != nil {
			cancel()
		}
		dur := time.Since(start)

		if err == nil {
			deps.Sink.OnStepOutcome(step.Name, result.Outcome, dur, nil)
			return result, nil
		}

		var fatal *plugin.FatalRunError
		if errors.As(err, &fatal) {
			deps.Sink.OnStepOutcome(step.Name, "failure", dur, err)
			return adapter.Result{}, err
		}

		lastErr = err
		if _, hasFailure := step.Outcomes["failure"]; hasFailure {
			deps.Sink.OnStepOutcome(step.Name, "failure", dur, err)
			return adapter.Result{Outcome: "failure"}, nil
		}
		deps.Sink.OnStepOutcome(step.Name, "", dur, err)
	}

	return adapter.Result{}, fmt.Errorf("step %q failed after %d attempts: %w", step.Name, maxAttempts-startAttempt+1, lastErr)
}

func (n *stepNode) executeStep(ctx context.Context, deps Deps, step *workflow.StepNode) (adapter.Result, error) {
	if step.Lifecycle == "open" {
		agent, ok := n.graph.Agents[step.Agent]
		if !ok {
			return adapter.Result{Outcome: "failure"}, fmt.Errorf("unknown agent %q", step.Agent)
		}
		// Plugin process startup is infrastructure, not step execution. Use an
		// uncancellable context so a short step timeout does not race the process
		// launch on a loaded host. Step timeouts govern plugin RPC execution;
		// they must not cancel the session open itself.
		if err := deps.Sessions.Open(context.WithoutCancel(ctx), step.Agent, agent.Adapter, step.OnCrash, agent.Config); err != nil {
			return adapter.Result{Outcome: "failure"}, err
		}
		return adapter.Result{Outcome: "success"}, nil
	}
	if step.Lifecycle == "close" {
		// Same rationale as "open": plugin teardown must complete regardless of
		// any step-level deadline that may have already expired.
		if err := deps.Sessions.Close(context.WithoutCancel(ctx), step.Agent); err != nil {
			return adapter.Result{Outcome: "failure"}, err
		}
		return adapter.Result{Outcome: "success"}, nil
	}
	if step.Agent != "" {
		adapterName := ""
		if agent, ok := n.graph.Agents[step.Agent]; ok {
			adapterName = agent.Adapter
		}
		deps.Sink.OnAdapterLifecycle(step.Name, adapterName, "started", "")
		result, execErr := deps.Sessions.Execute(ctx, step.Agent, step, deps.Sink.StepEventSink(step.Name))
		if execErr != nil {
			deps.Sink.OnAdapterLifecycle(step.Name, adapterName, "crashed", execErr.Error())
		} else {
			deps.Sink.OnAdapterLifecycle(step.Name, adapterName, "exited", "")
		}
		return result, execErr
	}

	anonSessionID := "anon-" + uuid.NewString()
	// Anonymous sessions follow the same lifecycle semantics as named-agent
	// open steps: process startup must not be bounded by the step deadline.
	if err := deps.Sessions.Open(context.WithoutCancel(ctx), anonSessionID, step.Adapter, plugin.OnCrashFail, nil); err != nil {
		return adapter.Result{Outcome: "failure"}, err
	}
	deps.Sink.OnAdapterLifecycle(step.Name, step.Adapter, "started", "")
	defer func() { _ = deps.Sessions.Close(context.WithoutCancel(ctx), anonSessionID) }()

	result, execErr := deps.Sessions.Execute(ctx, anonSessionID, step, deps.Sink.StepEventSink(step.Name))
	if execErr != nil {
		deps.Sink.OnAdapterLifecycle(step.Name, step.Adapter, "crashed", execErr.Error())
	} else {
		deps.Sink.OnAdapterLifecycle(step.Name, step.Adapter, "exited", "")
	}
	return result, execErr
}

func (n *stepNode) stepAdapterName() string {
	if n.step.Agent != "" {
		if agent, ok := n.graph.Agents[n.step.Agent]; ok {
			return agent.Adapter
		}
	}
	return n.step.Adapter
}

// stringMapToCtyObject converts a string-keyed map to a cty object value.
// Used to store adapter step outputs as cur.Prev for each._prev binding.
func stringMapToCtyObject(m map[string]string) cty.Value {
	if len(m) == 0 {
		return cty.EmptyObjectVal
	}
	vals := make(map[string]cty.Value, len(m))
	for k, v := range m {
		vals[k] = cty.StringVal(v)
	}
	return cty.ObjectVal(vals)
}

// iterOutputKey returns the key to use with WithIndexedStepOutput.
// Map for_each iterations use the string key so callers can look up outputs
// via steps.<name>["key"]. List/count iterations use the numeric index.
func iterOutputKey(cur *workflow.IterCursor) cty.Value {
	if len(cur.Keys) > 0 {
		return cur.Key
	}
	return cty.NumberIntVal(int64(cur.Index))
}
