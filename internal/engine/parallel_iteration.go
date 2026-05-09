package engine

// parallel_iteration.go — bounded fan-out engine for parallel = [...] steps (W19).
//
// runParallelIterations runs the step body concurrently for every item in the
// provided list, bounded by n.step.ParallelMax goroutines. Results are always
// returned in declaration (index) order regardless of goroutine completion order.
//
// on_failure semantics (evaluated in evaluateParallel by the caller):
//   - "abort" or "" (default): on first failure, cancel outstanding iterations
//     via context cancellation; remaining goroutines that are waiting for the
//     semaphore bail immediately; running goroutines see ctx.Done().
//   - "continue": all iterations complete; caller collects outcomes.
//   - "ignore": all iterations complete; caller treats aggregate as success.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/zclconf/go-cty/cty"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/plugin"
	"github.com/brokenbots/criteria/workflow"
)

// lockedSink wraps a Sink and serializes ALL Sink methods under a single mutex.
// Parallel goroutines may call any Sink method (e.g. via subworkflow fan-out),
// so every method must be protected. The embedding alone is not sufficient —
// unoverridden embedded methods bypass the mutex.
type lockedSink struct {
	Sink
	mu sync.Mutex
}

func (s *lockedSink) OnRunStarted(workflowName, initialStep string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Sink.OnRunStarted(workflowName, initialStep)
}

func (s *lockedSink) OnRunCompleted(finalState string, success bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Sink.OnRunCompleted(finalState, success)
}

func (s *lockedSink) OnRunFailed(reason, step string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Sink.OnRunFailed(reason, step)
}

func (s *lockedSink) OnStepEntered(step, adapterName string, attempt int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Sink.OnStepEntered(step, adapterName, attempt)
}

func (s *lockedSink) OnStepOutcome(step, outcome string, duration time.Duration, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Sink.OnStepOutcome(step, outcome, duration, err)
}

func (s *lockedSink) OnStepTransition(from, to, viaOutcome string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Sink.OnStepTransition(from, to, viaOutcome)
}

func (s *lockedSink) OnStepResumed(step string, attempt int, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Sink.OnStepResumed(step, attempt, reason)
}

func (s *lockedSink) OnVariableSet(name, value, source string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Sink.OnVariableSet(name, value, source)
}

func (s *lockedSink) OnStepOutputCaptured(step string, outputs map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Sink.OnStepOutputCaptured(step, outputs)
}

func (s *lockedSink) OnRunPaused(node, mode, signal string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Sink.OnRunPaused(node, mode, signal)
}

func (s *lockedSink) OnWaitEntered(node, mode, duration, signal string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Sink.OnWaitEntered(node, mode, duration, signal)
}

func (s *lockedSink) OnWaitResumed(node, mode, signal string, payload map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Sink.OnWaitResumed(node, mode, signal, payload)
}

func (s *lockedSink) OnApprovalRequested(node string, approvers []string, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Sink.OnApprovalRequested(node, approvers, reason)
}

func (s *lockedSink) OnApprovalDecision(node, decision, actor string, payload map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Sink.OnApprovalDecision(node, decision, actor, payload)
}

func (s *lockedSink) OnBranchEvaluated(node, matchedArm, target, condition string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Sink.OnBranchEvaluated(node, matchedArm, target, condition)
}

func (s *lockedSink) OnForEachEntered(node string, count int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Sink.OnForEachEntered(node, count)
}

func (s *lockedSink) OnStepIterationStarted(node string, index int, value string, anyFailed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Sink.OnStepIterationStarted(node, index, value, anyFailed)
}

func (s *lockedSink) OnStepIterationCompleted(node, outcome, target string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Sink.OnStepIterationCompleted(node, outcome, target)
}

func (s *lockedSink) OnStepIterationItem(node string, index int, step string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Sink.OnStepIterationItem(node, index, step)
}

func (s *lockedSink) OnScopeIterCursorSet(cursorJSON string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Sink.OnScopeIterCursorSet(cursorJSON)
}

func (s *lockedSink) OnAdapterLifecycle(stepName, adapterName, status, detail string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Sink.OnAdapterLifecycle(stepName, adapterName, status, detail)
}

func (s *lockedSink) OnRunOutputs(outputs []map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Sink.OnRunOutputs(outputs)
}

func (s *lockedSink) OnStepOutcomeDefaulted(step, original, mapped string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Sink.OnStepOutcomeDefaulted(step, original, mapped)
}

func (s *lockedSink) OnStepOutcomeUnknown(step, outcome string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Sink.OnStepOutcomeUnknown(step, outcome)
}

func (s *lockedSink) StepEventSink(step string) adapter.EventSink {
	s.mu.Lock()
	inner := s.Sink.StepEventSink(step)
	s.mu.Unlock()
	// Wrap the returned EventSink so that concurrent Log/Adapter calls from
	// parallel goroutines are serialized under the same mutex. Without this,
	// goroutines calling Log/Adapter on their per-iteration EventSinks would
	// race through any shared state in the underlying sink (e.g. ConsoleSink).
	return &lockedEventSink{EventSink: inner, mu: &s.mu}
}

// lockedEventSink wraps an adapter.EventSink and serializes Log and Adapter
// under the same mutex used by lockedSink. Each parallel goroutine receives
// its own lockedEventSink but all share the same *sync.Mutex, so concurrent
// Log/Adapter traffic is serialized and cannot race through shared sink state.
type lockedEventSink struct {
	adapter.EventSink
	mu *sync.Mutex
}

func (e *lockedEventSink) Log(stream string, chunk []byte) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.EventSink.Log(stream, chunk)
}

func (e *lockedEventSink) Adapter(kind string, data any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.EventSink.Adapter(kind, data)
}

// parallelIterResult holds the raw outcome and string outputs for one parallel
// iteration, keyed by its list index.
type parallelIterResult struct {
	index   int
	outcome string
	outputs map[string]string
	err     error
}

// runParallelIterations executes the step body concurrently for each item in
// items, bounded by n.step.ParallelMax goroutines. Results are in index order.
//
// For abort mode (on_failure == "" || "abort"), the per-iteration context is
// cancelled on the first failure so outstanding goroutines exit early. In
// continue/ignore mode, all goroutines run to completion.
// runOneParallelItem runs a single item in the parallel fan-out. It acquires
// the semaphore, binds each.*, runs the step body, and stores the result.
// visitsMu serializes Visits map access across goroutines (see runParallelIterations).
func runOneParallelItem(
	iterCtx context.Context,
	n *stepNode,
	i, total int,
	items, keys []cty.Value,
	st *RunState,
	deps Deps,
	sem chan struct{},
	results []parallelIterResult,
	cancelIter context.CancelFunc,
	cancelOnce *sync.Once,
	visitsMu *sync.Mutex,
) {
	select {
	case sem <- struct{}{}:
	case <-iterCtx.Done():
		results[i] = parallelIterResult{index: i, err: iterCtx.Err()}
		return
	}
	defer func() { <-sem }()

	key := cty.StringVal(strconv.Itoa(i))
	if i < len(keys) {
		key = keys[i]
	}
	iterSt := buildParallelIterState(i, total, items[i], key, st, visitsMu)

	deps.Sink.OnStepIterationStarted(n.step.Name, i, workflow.CtyValueToString(items[i]), false)

	outcome, outputs, err := n.runParallelIterationOnce(iterCtx, iterSt, deps)
	results[i] = parallelIterResult{index: i, outcome: outcome, outputs: outputs, err: err}

	if cancelIter != nil && (err != nil || !isSuccessOutcome(outcome)) {
		cancelOnce.Do(cancelIter)
	}
}

// buildParallelIterState constructs the per-iteration RunState. The Visits map
// is shared by reference so that max_visits is enforced across all goroutines;
// visitsMu serializes concurrent check-and-increment in incrementVisit.
func buildParallelIterState(i, total int, item, key cty.Value, st *RunState, visitsMu *sync.Mutex) *RunState {
	return &RunState{
		Current:        st.Current,
		WorkflowDir:    st.WorkflowDir,
		SharedVarStore: st.SharedVarStore,
		Visits:         st.Visits,
		VisitsMu:       visitsMu,
		Vars: workflow.WithEachBinding(st.Vars, &workflow.EachBinding{
			Value: item,
			Key:   key,
			Index: i,
			Total: total,
			First: i == 0,
			Last:  i == total-1,
			Prev:  cty.NilVal,
		}),
	}
}

func runParallelIterations(ctx context.Context, n *stepNode, items, keys []cty.Value, st *RunState, deps Deps) []parallelIterResult {
	total := len(items)
	results := make([]parallelIterResult, total)

	iterCtx := ctx
	var cancelIter context.CancelFunc
	if n.step.OnFailure == "" || n.step.OnFailure == "abort" {
		iterCtx, cancelIter = context.WithCancel(ctx)
		defer cancelIter()
	}

	sem := make(chan struct{}, n.step.ParallelMax)

	// Ensure the shared Visits map exists so all iterSt copies see the same
	// underlying map for max_visits tracking. visitsMu serializes concurrent
	// check-and-increment calls in incrementVisit across goroutines.
	if st.Visits == nil {
		st.Visits = make(map[string]int)
	}
	var visitsMu sync.Mutex

	var wg sync.WaitGroup
	var cancelOnce sync.Once

	for i := range items {
		wg.Add(1)
		i := i
		go func() {
			defer wg.Done()
			runOneParallelItem(iterCtx, n, i, total, items, keys, st, deps, sem, results, cancelIter, &cancelOnce, &visitsMu)
		}()
	}

	wg.Wait()
	return results
}

// runParallelIterationOnce executes a single parallel iteration for either an
// adapter or a subworkflow step. It does not use the sequential cursor machinery
// or outcome routing — it returns the raw outcome string and string-encoded
// outputs so evaluateParallel can aggregate them.
func (n *stepNode) runParallelIterationOnce(ctx context.Context, st *RunState, deps Deps) (outcome string, outputs map[string]string, err error) {
	if n.step.TargetKind == workflow.StepTargetSubworkflow {
		return n.runParallelSubworkflowIteration(ctx, st, deps)
	}
	return n.runParallelAdapterIteration(ctx, st, deps)
}

// runParallelAdapterIteration resolves the step's input expressions against the
// per-iteration vars and executes the iteration through runStepFromAttempt.
// This preserves the same step-execution semantics as non-parallel adapter steps:
// max_visits enforcement (via the shared Visits map + VisitsMu in st), per-step
// timeout wrapping, retry behaviour, and fatal-error propagation.
func (n *stepNode) runParallelAdapterIteration(ctx context.Context, st *RunState, deps Deps) (outcome string, outputs map[string]string, err error) {
	effectiveStep, resolveErr := n.resolveInput(st.Vars, st.WorkflowDir)
	if resolveErr != nil {
		return "", nil, fmt.Errorf("step %q: input expression error: %w", n.step.Name, resolveErr)
	}

	result, execErr := n.runStepFromAttempt(ctx, st, deps, effectiveStep, 1)
	if execErr != nil {
		return "failure", nil, execErr
	}
	return result.Outcome, result.Outputs, nil
}

// runParallelSubworkflowIteration evaluates the step's input expressions and
// spawns a fresh subworkflow execution per iteration. Each subworkflow gets its
// own child scope and SharedVarStore, matching the sequential subworkflow step
// semantics. Returns the raw outcome string and string-encoded outputs.
func (n *stepNode) runParallelSubworkflowIteration(ctx context.Context, st *RunState, deps Deps) (outcome string, outputs map[string]string, err error) {
	swNode, ok := n.graph.Subworkflows[n.step.SubworkflowRef]
	if !ok {
		return "", nil, fmt.Errorf("step %q: subworkflow %q not found", n.step.Name, n.step.SubworkflowRef)
	}

	var stepInput map[string]cty.Value
	if len(n.step.InputExprs) > 0 {
		evalOpts := workflow.DefaultFunctionOptions(st.WorkflowDir)
		stepInput, err = workflow.ResolveInputExprsAsCty(n.step.InputExprs, st.Vars, evalOpts)
		if err != nil {
			return "", nil, fmt.Errorf("step %q: input expression error: %w", n.step.Name, err)
		}
	}

	// Per-iteration session isolation: each parallel goroutine receives its own
	// SessionManager so that initScopeAdapters inside runWorkflowBody opens
	// fresh adapter sessions rather than colliding on the parent scope's sessions.
	// runWorkflowBody's deferred tearDownScopeAdapters closes and kills all
	// sessions it opened, so no explicit Shutdown is needed here.
	iterDeps := deps
	iterDeps.Sessions = plugin.NewSessionManager(deps.Loader)

	swOutputs, runErr := runSubworkflow(ctx, swNode, st, stepInput, iterDeps)
	if runErr != nil {
		return "failure", nil, runErr
	}

	stringOutputs, renderErr := ctyOutputsToStrings(n.step.Name, swOutputs)
	if renderErr != nil {
		return "", nil, renderErr
	}
	return "success", stringOutputs, nil
}

// parallelOutputKey returns the output accumulation key for a parallel
// iteration. Parallel steps only support list inputs (map/object syntax is
// rejected at compile time and at runtime), so the key is always the integer
// index of the item in the list.
func parallelOutputKey(index int) cty.Value {
	return cty.NumberIntVal(int64(index))
}

// ctyOutputsToStrings converts a map[string]cty.Value (subworkflow outputs) to
// map[string]string using renderCtyValue. Non-string cty values are JSON-encoded.
// Returns an error if any value cannot be rendered, matching the non-parallel
// subworkflow output path in evaluateSubworkflowStep.
func ctyOutputsToStrings(stepName string, outputs map[string]cty.Value) (map[string]string, error) {
	result := make(map[string]string, len(outputs))
	for k, v := range outputs {
		if v.IsKnown() && v.Type() == cty.String {
			result[k] = v.AsString()
			continue
		}
		rendered, err := renderCtyValue(v)
		if err != nil {
			return nil, fmt.Errorf("step %q: subworkflow output %q: %w", stepName, k, err)
		}
		result[k] = rendered
	}
	return result, nil
}

// evaluateParallel implements the parallel = [...] step modifier. It evaluates
// the list expression, fans out the step body concurrently up to ParallelMax
// goroutines, aggregates indexed outputs, applies per-iteration shared_writes,
// classifyIterError interprets a parallelIterResult error for aggregation.
// Returns (isRunError, isFailure) where:
//   - isRunError means the error must propagate as a run failure (not outcome routing).
//   - isFailure means the iteration should count as failed for outcome aggregation.
//
// outcome=="" + non-context error: internal engine error (render failure, input error) → run failure.
// outcome=="" + context error: abort-mode cancellation (semaphore-wait bail) → failed iteration.
// outcome=="" + nil err: unreachable (handled by caller's else branch).
// outcome!="" + err: step-level error with a known outcome → check for FatalRunError.
func classifyIterError(r parallelIterResult) (isRunError, isFailure bool) {
	if r.outcome == "" {
		if !errors.Is(r.err, context.Canceled) && !errors.Is(r.err, context.DeadlineExceeded) {
			return true, false
		}
		return false, true
	}
	var fatal *plugin.FatalRunError
	if errors.As(r.err, &fatal) {
		return true, false
	}
	return false, true
}

// aggregateParallelResults iterates over results, accumulates per-iteration
// outputs and shared_writes, and returns whether any iteration failed.
func (n *stepNode) aggregateParallelResults(results []parallelIterResult, st *RunState, sink Sink) (anyFailed bool, err error) {
	for _, r := range results {
		if r.err != nil {
			isRunErr, isFailed := classifyIterError(r)
			if isRunErr {
				return true, r.err
			}
			if isFailed {
				anyFailed = true
			}
		} else if !isSuccessOutcome(r.outcome) {
			anyFailed = true
		}
		if len(r.outputs) > 0 {
			key := parallelOutputKey(r.index)
			st.Vars = workflow.WithIndexedStepOutput(st.Vars, n.step.Name, key, r.outputs)
		}
		if r.err == nil && r.outcome != "" {
			if writeErr := n.applyIterationSharedWrites(r.outcome, r.outputs, st, sink); writeErr != nil {
				return anyFailed, writeErr
			}
		}
	}
	return anyFailed, nil
}

// finishParallelOutcome resolves the aggregate outcome, emits sink events, and
// applies output projection and aggregate-level shared_writes.
func (n *stepNode) finishParallelOutcome(anyFailed bool, st *RunState, deps Deps) (string, error) {
	aggregateOutcome := "all_succeeded"
	if anyFailed && n.step.OnFailure != "ignore" {
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

// evaluateParallel implements the parallel = [...] step modifier. It evaluates
// the list expression, fans out the step body concurrently up to ParallelMax
// goroutines, aggregates indexed outputs, applies per-iteration shared_writes,
// and routes via the aggregate outcome (all_succeeded / any_failed).
//
// The function mirrors finishIterationInGraph for aggregate outcome projection
// and shared_writes so that parallel steps behave consistently with for_each/count.
func (n *stepNode) evaluateParallel(ctx context.Context, st *RunState, deps Deps) (string, error) {
	evalCtx := workflow.BuildEvalContextWithOpts(st.Vars, workflow.DefaultFunctionOptions(st.WorkflowDir))
	items, keys, err := buildForEachItems(n.step.Parallel, evalCtx)
	if err != nil {
		return "", fmt.Errorf("step %q: parallel expression error: %w", n.step.Name, err)
	}
	// Reject map/object at runtime as a safety net (compile-time check covers
	// foldable expressions; runtime-computed maps are caught here).
	if keys != nil {
		return "", fmt.Errorf("step %q: parallel must be a list [...]; map and object syntax are not supported", n.step.Name)
	}

	total := len(items)
	deps.Sink.OnForEachEntered(n.step.Name, total)

	if total == 0 {
		co := n.step.Outcomes["all_succeeded"]
		deps.Sink.OnStepIterationCompleted(n.step.Name, "all_succeeded", co.Next)
		return co.Next, nil
	}

	// Serialize sink calls from goroutines to prevent data races on the sink.
	parallelDeps := deps
	parallelDeps.Sink = &lockedSink{Sink: deps.Sink}

	results := runParallelIterations(ctx, n, items, keys, st, parallelDeps)

	// If the parent context was cancelled (not just an internal abort), propagate
	// the error rather than treating it as a normal failure outcome.
	if ctx.Err() != nil {
		return "", ctx.Err()
	}

	anyFailed, aggregateErr := n.aggregateParallelResults(results, st, deps.Sink)
	if aggregateErr != nil {
		return "", aggregateErr
	}

	return n.finishParallelOutcome(anyFailed, st, deps)
}

// parallelAdapterResult is a thin shim so the adapter package is visible inside
// this file's tests without a separate import; not used in production paths.
var _ = adapter.Result{}
