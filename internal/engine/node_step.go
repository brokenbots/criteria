package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapter/secrets"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow"
)

type stepNode struct {
	graph *workflow.FSMGraph
	step  *workflow.StepNode
}

// policyLimitError wraps errors that arise from hard policy limits (e.g.
// max_visits exceeded). Unlike transient adapter errors, these must always
// propagate out of while/for_each loops regardless of on_failure mode.
type policyLimitError struct{ err error }

func (e *policyLimitError) Error() string { return e.err.Error() }
func (e *policyLimitError) Unwrap() error { return e.err }

func (n *stepNode) Name() string {
	return n.step.Name
}

func (n *stepNode) Evaluate(ctx context.Context, st *RunState, deps Deps) (string, error) {
	st.TotalSteps++
	if st.TotalSteps > n.graph.Policy.MaxTotalSteps {
		return "", fmt.Errorf("policy.max_total_steps exceeded (%d)", n.graph.Policy.MaxTotalSteps)
	}

	// Refresh the "data" namespace in vars so expressions see the current
	// snapshot of all data block values.
	if st.DataStore != nil {
		st.Vars = workflow.SeedDataSnapshot(st.Vars, st.DataStore.Snapshot())
	}

	// Handle step-level iteration (for_each or count).
	if n.step.ForEach != nil || n.step.Count != nil {
		return n.evaluateIterating(ctx, st, deps)
	}

	// Handle while-driven iteration.
	if n.step.While != nil {
		return n.evaluateWhile(ctx, st, deps)
	}

	// Handle parallel execution.
	if n.step.Parallel != nil {
		return n.evaluateParallel(ctx, st, deps)
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
		co := n.step.Outcomes["all_succeeded"]
		deps.Sink.OnStepIterationCompleted(n.step.Name, "all_succeeded", co.Next)
		return co.Next, true, nil
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

// runOneIteration executes the adapter for the current iteration and returns
// the raw outcome (which routeIteratingStep will intercept).
func (n *stepNode) runOneIteration(ctx context.Context, st *RunState, deps Deps, _ *workflow.IterCursor) (string, error) {
	return n.evaluateOnce(ctx, st, deps)
}

// evaluateOnce executes the step for a single non-iterating invocation (or a
// single iteration invocation for steps).
func (n *stepNode) evaluateOnce(ctx context.Context, st *RunState, deps Deps) (string, error) {
	// Subworkflow steps bypass the adapter execution path entirely.
	if n.step.TargetKind == workflow.StepTargetSubworkflow {
		return n.evaluateSubworkflowStep(ctx, st, deps)
	}

	effectiveStep, resolveErr := n.resolveInput(ctx, st.Vars, st.WorkflowDir, deps.Sessions.RedactionRegistry)
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
		if err := n.applyIterationDataWrites(result.Outcome, result.Outputs, st, deps.Sink); err != nil {
			return "", err
		}
		return result.Outcome, nil
	}

	return n.applyOutcome(result.Outcome, result.Outputs, nil, st, deps)
}

// applyIterationDataWrites applies write blocks from the per-iteration
// outcome (if declared) during a for_each / count step. It is called on every
// adapter result inside the iteration loop — before the aggregate outcome fires.
// projectedCty is computed from the outcome's OutputExpr when present.
func (n *stepNode) applyIterationDataWrites(outcomeName string, rawOutputs map[string]string, st *RunState, sink Sink) error {
	compiled, ok := n.step.Outcomes[outcomeName]
	if !ok || len(compiled.Writes) == 0 || st.DataStore == nil {
		return nil
	}
	var projectedCty map[string]cty.Value
	if compiled.OutputExpr != nil {
		proj, err := evalOutcomeOutputProjection(compiled.OutputExpr, nil, rawOutputs, st)
		if err != nil {
			return fmt.Errorf("step %q outcome %q: output projection: %w", n.step.Name, outcomeName, err)
		}
		projectedCty = proj
	}
	return applyDataWrites(n.step.Name, outcomeName, compiled.Writes, projectedCty, rawOutputs, nil, st, sink)
}

// applyOutcome resolves the compiled outcome for the given adapter outcome name,
// applies any output projection, stores outputs in run vars, and returns the
// next node name (or ReturnSentinel). Separated from evaluateOnce to keep
// cognitive complexity below the lint threshold.
//
// swOutputs carries the cty-typed outputs from a subworkflow step (nil for
// adapter steps). When non-nil they are exposed as the "subworkflow" namespace
// inside any outcome.output expression, so callers can write
// output = { result = subworkflow.val }.
func (n *stepNode) applyOutcome(outcomeName string, rawOutputs map[string]string, swOutputs map[string]cty.Value, st *RunState, deps Deps) (string, error) {
	compiled, ok := n.step.Outcomes[outcomeName]
	if !ok {
		if n.step.DefaultOutcome != nil {
			deps.Sink.OnStepOutcomeDefaulted(n.step.Name, outcomeName, n.step.DefaultOutcome.Name)
			compiled = n.step.DefaultOutcome
			outcomeName = compiled.Name
		} else {
			deps.Sink.OnStepOutcomeUnknown(n.step.Name, outcomeName)
			return "", fmt.Errorf("step %q produced unmapped outcome %q", n.step.Name, outcomeName)
		}
	}

	// Apply output projection if the outcome declares one. Projection returns
	// raw cty values so numeric/bool types survive the return path unaltered.
	stepOutputs := rawOutputs
	var projectedCty map[string]cty.Value
	if compiled.OutputExpr != nil {
		projected, err := evalOutcomeOutputProjection(compiled.OutputExpr, swOutputs, rawOutputs, st)
		if err != nil {
			return "", fmt.Errorf("step %q outcome %q: output projection: %w", n.step.Name, outcomeName, err)
		}
		projectedCty = projected
		stepOutputs, err = ctyValsToStrings(projected)
		if err != nil {
			return "", fmt.Errorf("step %q outcome %q: output projection render: %w", n.step.Name, outcomeName, err)
		}
	}

	if len(stepOutputs) > 0 {
		st.Vars = workflow.WithStepOutputs(st.Vars, n.step.Name, stepOutputs)
		deps.Sink.OnStepOutputCaptured(n.step.Name, stepOutputs)
	}

	// Apply write blocks: update data store with values from the step's outputs.
	// Uses SetBatch to commit the full write set atomically.
	if len(compiled.Writes) > 0 && st.DataStore != nil {
		if err := applyDataWrites(n.step.Name, outcomeName, compiled.Writes, projectedCty, rawOutputs, swOutputs, st, deps.Sink); err != nil {
			return "", err
		}
	}

	if compiled.Next == workflow.ReturnSentinel {
		captureReturnOutputs(rawOutputs, projectedCty, st)
		deps.Sink.OnStepTransition(n.step.Name, workflow.ReturnSentinel, outcomeName)
		return workflow.ReturnSentinel, nil
	}

	deps.Sink.OnStepTransition(n.step.Name, compiled.Next, outcomeName)
	return compiled.Next, nil
}

// captureReturnOutputs stores the step's final outputs into st.ReturnOutputs
// so subworkflow callers can access them via the "subworkflow.*" namespace.
// Prefers typed cty projections over raw adapter string outputs.
func captureReturnOutputs(rawOutputs map[string]string, projectedCty map[string]cty.Value, st *RunState) {
	if projectedCty != nil {
		st.ReturnOutputs = projectedCty
	} else if len(rawOutputs) > 0 {
		retVals := make(map[string]cty.Value, len(rawOutputs))
		for k, v := range rawOutputs {
			retVals[k] = cty.StringVal(v)
		}
		st.ReturnOutputs = retVals
	}
}

// applyDataWrites resolves the write set from the outcome's write blocks
// and commits all entries atomically via SetBatch. Each value expression is
// evaluated against the full outcome eval context (see resolveDataWriteValue):
// a bare output.* traversal is resolved directly from projectedCty/rawOutputs,
// and any richer expression sees var.*, local.*, data.*, step.output.*,
// subworkflow.*, and output.*. swOutputs carries subworkflow return values
// (nil for adapter and aggregate-iteration outcomes). Because resolution reads
// the step-entry data snapshot and SetBatch commits only after every value is
// resolved, the entire write set is atomic against that snapshot.
func applyDataWrites(
	stepName, outcomeName string,
	writes []workflow.CompiledWrite,
	projectedCty map[string]cty.Value,
	rawOutputs map[string]string,
	swOutputs map[string]cty.Value,
	st *RunState,
	sink Sink,
) error {
	batch := make([]DataWrite, 0, len(writes))
	for _, w := range writes {
		v, err := resolveDataWriteValue(w, projectedCty, rawOutputs, swOutputs, st)
		if err != nil {
			msg := fmt.Sprintf("step %q outcome %q: write %q %q: %v", stepName, outcomeName, w.DataKind, w.DataName, err)
			sink.OnRunFailed(msg, stepName)
			return fmt.Errorf("step %q outcome %q: write %q %q: %w", stepName, outcomeName, w.DataKind, w.DataName, err)
		}
		if v == cty.NilVal {
			msg := fmt.Sprintf("step %q outcome %q: write %q %q: value resolved to nil", stepName, outcomeName, w.DataKind, w.DataName)
			sink.OnRunFailed(msg, stepName)
			return fmt.Errorf("%s", msg) //nolint:err113 // msg is already fully contextual
		}
		batch = append(batch, DataWrite{Kind: w.DataKind, Name: w.DataName, Value: v})
	}
	if err := st.DataStore.SetBatch(batch); err != nil {
		msg := fmt.Sprintf("step %q outcome %q: write blocks: %v", stepName, outcomeName, err)
		sink.OnRunFailed(msg, stepName)
		return fmt.Errorf("step %q outcome %q: write blocks: %w", stepName, outcomeName, err)
	}
	return nil
}

// resolveDataWriteValue evaluates the value expression for a single write block.
// It prefers the typed cty value from projectedCty when the expression is a bare
// output.* traversal; falls back to coercing the raw adapter string from
// rawOutputs. Returns (cty.NilVal, nil) if the key is absent, or (cty.NilVal, err)
// if coercion fails.
func resolveDataWriteValue(w workflow.CompiledWrite, projectedCty map[string]cty.Value, rawOutputs map[string]string, swOutputs map[string]cty.Value, st *RunState) (cty.Value, error) {
	if v, ok, err := resolveBareOutputTraversal(w, projectedCty, rawOutputs, st.DataStore); ok || err != nil {
		return v, err
	}
	return resolveExprDataWriteValue(w.ValueExpr, projectedCty, rawOutputs, swOutputs, st)
}

// resolveBareOutputTraversal handles the common case where the write value
// expression is a bare output.* traversal. It returns (value, true, nil) on
// success, (cty.NilVal, false, nil) when the expression is not a bare
// traversal, and (cty.NilVal, false, error) on lookup/coercion failure.
func resolveBareOutputTraversal(w workflow.CompiledWrite, projectedCty map[string]cty.Value, rawOutputs map[string]string, store *DataStore) (cty.Value, bool, error) {
	vars := w.ValueExpr.Variables()
	if len(vars) != 1 || len(vars[0]) != 2 {
		return cty.NilVal, false, nil
	}
	root, ok1 := vars[0][0].(hcl.TraverseRoot)
	if !ok1 || root.Name != "output" {
		return cty.NilVal, false, nil
	}
	attr, ok2 := vars[0][1].(hcl.TraverseAttr)
	if !ok2 {
		return cty.NilVal, false, nil
	}
	key := attr.Name
	if projectedCty != nil {
		if pv, ok := projectedCty[key]; ok {
			return pv, true, nil
		}
	}
	if sv, ok := rawOutputs[key]; ok {
		declaredType, _ := store.TypeOf(w.DataKind, w.DataName)
		v, err := coerceStringToCty(sv, declaredType)
		return v, true, err
	}
	return cty.NilVal, true, fmt.Errorf("output key %q not found", key)
}

// resolveExprDataWriteValue evaluates a non-bare write value expression against
// the full outcome eval context. The expression may reference any namespace
// available to an outcome's output = { ... } projection: var.*, local.*,
// data.*, each.*, steps.*, plus step.output.* (raw adapter outputs),
// subworkflow.* (subworkflow return values), output.* (this outcome's
// projection keys), and the standard functions.
//
// data.* values come from st.Vars, which is seeded once at step entry from
// DataStore.Snapshot(). Writes from the current step are not committed to the
// store until after every write value is resolved (SetBatch), so a write that
// reads data.<kind>.<name> always sees the step-entry snapshot — never a value
// written earlier in the same step. This makes the whole write set atomic
// against the snapshot.
func resolveExprDataWriteValue(expr hcl.Expression, projectedCty map[string]cty.Value, rawOutputs map[string]string, swOutputs map[string]cty.Value, st *RunState) (cty.Value, error) {
	evalOpts := workflow.DefaultFunctionOptions(st.WorkflowDir)
	evalCtx := workflow.BuildEvalContextWithOpts(st.Vars, evalOpts)
	if len(swOutputs) > 0 {
		evalCtx.Variables["subworkflow"] = cty.ObjectVal(swOutputs)
	} else {
		evalCtx.Variables["subworkflow"] = cty.EmptyObjectVal
	}
	evalCtx.Variables["step"] = buildStepOutputVar(rawOutputs)

	outputVars := make(map[string]cty.Value)
	if projectedCty != nil {
		for k, v := range projectedCty {
			outputVars[k] = v
		}
	} else {
		for k, v := range rawOutputs {
			outputVars[k] = cty.StringVal(v)
		}
	}
	evalCtx.Variables["output"] = cty.ObjectVal(outputVars)

	val, diags := expr.Value(evalCtx)
	if diags.HasErrors() {
		return cty.NilVal, fmt.Errorf("evaluating value expression: %s", diags.Error())
	}
	return val, nil
}

// It runs the referenced subworkflow in a nested engine loop and maps the result
// to the step's declared outcomes. Step-level input expressions (from the step's
// input { } block) are evaluated against the parent scope and passed into the
// callee as variable bindings, overriding any declaration-level bindings.
func (n *stepNode) evaluateSubworkflowStep(ctx context.Context, st *RunState, deps Deps) (string, error) {
	swNode, ok := n.graph.Subworkflows[n.step.SubworkflowRef]
	if !ok {
		return "", fmt.Errorf("step %q: subworkflow %q not found", n.step.Name, n.step.SubworkflowRef)
	}

	// Evaluate step-level input expressions against the parent scope.
	var stepInput map[string]cty.Value
	if len(n.step.InputExprs) > 0 {
		evalOpts := workflow.DefaultFunctionOptions(st.WorkflowDir)
		resolved, err := workflow.ResolveInputExprsAsCty(n.step.InputExprs, st.Vars, evalOpts)
		if err != nil {
			return "", fmt.Errorf("step %q: input expression error: %w", n.step.Name, err)
		}
		stepInput = resolved
	}

	outputs, terminalState, runErr := runSubworkflow(ctx, swNode, st, stepInput, deps)

	outcome := "success"
	if runErr != nil || (terminalState != workflow.ReturnSentinel && !swNode.Body.States[terminalState].Success) {
		outcome = "failure"
	}

	// Convert subworkflow cty outputs to a string map for pass-through storage
	// (steps.<name>.*). String values are stored as raw strings to match the
	// adapter output convention (adapters return map[string]string directly).
	// Non-string values are rendered via renderCtyValue (JSON encoding).
	// The raw cty map is passed separately as swOutputs so outcome.output
	// expressions can reference subworkflow.*.
	stringOutputs := make(map[string]string, len(outputs))
	for k, v := range outputs {
		if v.IsKnown() && !v.IsNull() && v.Type() == cty.String {
			stringOutputs[k] = v.AsString()
			continue
		}
		rendered, err := renderCtyValue(v)
		if err != nil {
			return "", fmt.Errorf("step %q: subworkflow output %q: %w", n.step.Name, k, err)
		}
		stringOutputs[k] = rendered
	}

	// Route through applyOutcome so DefaultOutcome mapping, OutputExpr
	// evaluation (including subworkflow.* references), and the return sentinel
	// are all handled uniformly with adapter steps.
	return n.applyOutcome(outcome, stringOutputs, outputs, st, deps)
}

// Per-step environment override (n.step.Environment) takes precedence over
// the adapter's environment and the workflow's DefaultEnvironment.
func (n *stepNode) getStepEnvironment() *workflow.EnvironmentNode {
	if n.step.Environment != "" {
		if env, ok := n.graph.Environments[n.step.Environment]; ok {
			return env
		}
	}
	// Fall back to the adapter's declared environment.
	if n.step.AdapterRef != "" {
		if adapterDecl, ok := n.graph.Adapters[n.step.AdapterRef]; ok && adapterDecl.Environment != "" {
			if env, ok := n.graph.Environments[adapterDecl.Environment]; ok {
				return env
			}
		}
	}
	// Final fallback: workflow-level default.
	if n.graph.DefaultEnvironment != "" {
		return n.graph.Environments[n.graph.DefaultEnvironment]
	}
	return nil
}

// resolveInput returns the step with Input and SecretInputs populated from
// evaluated HCL expressions. It returns an error if any expression fails to
// evaluate so the caller can fail fast rather than silently using a placeholder
// value. It also merges in environment variables if the step has a bound
// environment.
func (n *stepNode) resolveInput(ctx context.Context, vars map[string]cty.Value, workflowDir string, reg *secrets.Registry) (*workflow.StepNode, error) {
	// Start with a copy of the step input.
	merged := make(map[string]string, len(n.step.Input))
	for k, v := range n.step.Input {
		merged[k] = v
	}

	// Resolve HCL input expressions if any.
	if len(n.step.InputExprs) > 0 {
		resolved, err := workflow.ResolveInputExprsWithOpts(n.step.InputExprs, vars, workflow.DefaultFunctionOptions(workflowDir))
		if err != nil {
			return nil, err
		}
		// Expression-resolved values override compiled Input placeholders.
		for k, v := range resolved {
			merged[k] = v
		}
	}

	// Inject environment variables if the step has a bound environment.
	// Environment variables are merged into the "env" input field, which adapters
	// like shell will parse and inject into the subprocess.
	n.mergeEnvironmentVars(merged)

	// Resolve secret_input expressions through the provider stack (WS13).
	secretInputs, err := resolveStepSecretInputs(ctx, n.graph, n.step, vars, reg)
	if err != nil {
		return nil, err
	}

	cp := *n.step
	cp.Input = merged
	cp.SecretInputs = secretInputs
	return &cp, nil
}

// mergeEnvironmentVars merges environment-declared variables into the "env" input field,
// filtering out security-critical variables that the shell adapter controls.
func (n *stepNode) mergeEnvironmentVars(merged map[string]string) {
	env := n.getStepEnvironment()
	if env == nil || len(env.Variables) == 0 {
		return
	}

	// Parse the existing "env" input if present.
	existingEnv := make(map[string]string)
	if rawEnv, ok := merged["env"]; ok && rawEnv != "" {
		_ = json.Unmarshal([]byte(rawEnv), &existingEnv)
	}

	// Merge environment-declared variables, skipping controlled keys and LC_* prefixes.
	// Step-declared env vars take precedence over environment-declared ones.
	for k, v := range env.Variables {
		// Skip controlled vars and LC_* prefixes (controlled by shell adapter for locale).
		// Uses exported ShellControlledEnvVars from workflow package for consistency with compile-time checks.
		if workflow.ShellControlledEnvVars[k] || workflow.IsShellLCPrefix(k) {
			continue
		}
		if _, exists := existingEnv[k]; !exists {
			existingEnv[k] = v
		}
	}

	// Re-encode the merged env as JSON and store it back.
	jsonBytes, _ := json.Marshal(existingEnv)
	merged["env"] = string(jsonBytes)
}

// incrementVisit checks the max_visits limit and increments the visit counter
// for this step. Returns an error if the limit would be exceeded (W07).
// When st.VisitsMu is set (parallel fan-out), the check-and-increment is
// performed under the mutex so concurrent goroutines share one counter.
func (n *stepNode) incrementVisit(st *RunState) error {
	if st.VisitsMu != nil {
		st.VisitsMu.Lock()
		defer st.VisitsMu.Unlock()
	}
	if n.step.MaxVisits > 0 {
		if st.Visits == nil {
			st.Visits = make(map[string]int)
		}
		if st.Visits[n.step.Name] >= n.step.MaxVisits {
			return &policyLimitError{err: fmt.Errorf("step %q exceeded max_visits (%d)", n.step.Name, n.step.MaxVisits)}
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

		var fatal *adapterhost.FatalRunError
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
	// Non-lifecycle step: execute using the referenced adapter.
	if step.AdapterRef != "" {
		adapterType := ""
		if adaptrDecl, ok := n.graph.Adapters[step.AdapterRef]; ok {
			adapterType = adaptrDecl.Type
		}
		deps.Sink.OnAdapterLifecycle(step.Name, adapterType, "started", "")
		result, execErr := deps.Sessions.Execute(ctx, step.AdapterRef, step, deps.Sink.StepEventSink(step.Name))
		if execErr != nil {
			deps.Sink.OnAdapterLifecycle(step.Name, adapterType, "crashed", execErr.Error())
		} else {
			deps.Sink.OnAdapterLifecycle(step.Name, adapterType, "exited", "")
		}
		return result, execErr
	}

	// This case should not happen with proper validation, but provide a fallback.
	// All steps must reference an adapter or be a subworkflow target.
	return adapter.Result{}, fmt.Errorf("step %q has no adapter reference", step.Name)
}

func (n *stepNode) stepAdapterName() string {
	// Extract the adapter type from the dotted "<type>.<name>" reference.
	if n.step.AdapterRef != "" {
		parts := strings.Split(n.step.AdapterRef, ".")
		if len(parts) == 2 {
			return parts[0]
		}
	}
	return ""
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

// evalOutcomeOutputProjection evaluates an outcome's output expression against
// the current run state and returns the raw cty attribute values. The expression
// must evaluate to a cty object; each attribute becomes an output key. Callers
// use ctyValsToStrings when they need a map[string]string for WithStepOutputs
// or OnStepOutputCaptured; st.ReturnOutputs receives the raw cty values so that
// top-level return preserves numeric/bool types in OnRunOutputs, matching the
// encoding produced by the normal output-block evaluation path.
//
// swOutputs, when non-nil, is exposed as the "subworkflow" variable in the eval
// context so that outcome expressions can reference subworkflow.* keys.
//
// adapterOutputs, when non-nil, is exposed as the "step.output" variable in the
// eval context so that outcome expressions can reference step.output.<key>. Each
// value is a cty.String (raw adapter output string). This is the mechanism for
// outcome projections that need to reference the current step's adapter result —
// for example to transform or accumulate values into a data block.
func evalOutcomeOutputProjection(expr hcl.Expression, swOutputs map[string]cty.Value, adapterOutputs map[string]string, st *RunState) (map[string]cty.Value, error) {
	evalOpts := workflow.DefaultFunctionOptions(st.WorkflowDir)
	evalCtx := workflow.BuildEvalContextWithOpts(st.Vars, evalOpts)
	if len(swOutputs) > 0 {
		evalCtx.Variables["subworkflow"] = cty.ObjectVal(swOutputs)
	} else {
		evalCtx.Variables["subworkflow"] = cty.EmptyObjectVal
	}
	evalCtx.Variables["step"] = buildStepOutputVar(adapterOutputs)
	val, diags := expr.Value(evalCtx)
	if diags.HasErrors() {
		return nil, fmt.Errorf("evaluating output expression: %s", diags.Error())
	}
	if !val.Type().IsObjectType() {
		return nil, fmt.Errorf("outcome output must be an object; got %s", val.Type().FriendlyName())
	}
	result := make(map[string]cty.Value, len(val.Type().AttributeTypes()))
	for name := range val.Type().AttributeTypes() {
		result[name] = val.GetAttr(name)
	}
	return result, nil
}

// buildStepOutputVar constructs the cty object exposed as the "step" variable
// in outcome output projection expressions. It has a single "output" attribute
// that is an object of string-valued adapter output keys. When adapterOutputs is
// empty, "step.output" is an empty object (no keys).
func buildStepOutputVar(adapterOutputs map[string]string) cty.Value {
	if len(adapterOutputs) == 0 {
		return cty.ObjectVal(map[string]cty.Value{
			"output": cty.EmptyObjectVal,
		})
	}
	attrs := make(map[string]cty.Value, len(adapterOutputs))
	for k, v := range adapterOutputs {
		attrs[k] = cty.StringVal(v)
	}
	return cty.ObjectVal(map[string]cty.Value{
		"output": cty.ObjectVal(attrs),
	})
}

// ctyValsToStrings converts a map[string]cty.Value to map[string]string using
// renderCtyValue for each value. Used to produce the string form needed by
// WithStepOutputs and OnStepOutputCaptured.
func ctyValsToStrings(vals map[string]cty.Value) (map[string]string, error) {
	result := make(map[string]string, len(vals))
	for k, v := range vals {
		rendered, err := renderCtyValue(v)
		if err != nil {
			return nil, fmt.Errorf("output key %q: %w", k, err)
		}
		result[k] = rendered
	}
	return result, nil
}

// Map for_each iterations use the string key so callers can look up outputs
// via steps.<name>["key"]. List/count iterations use the numeric index.
func iterOutputKey(cur *workflow.IterCursor) cty.Value {
	if len(cur.Keys) > 0 {
		return cur.Key
	}
	return cty.NumberIntVal(int64(cur.Index))
}
