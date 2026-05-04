// Package engine drives a workflow's FSM. It is pure: dispatcher (adapter
// lookup) and event sink are injected so the engine can be tested without I/O.
package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/zclconf/go-cty/cty"

	"github.com/brokenbots/criteria/internal/adapter"
	engineruntime "github.com/brokenbots/criteria/internal/engine/runtime"
	"github.com/brokenbots/criteria/internal/plugin"
	"github.com/brokenbots/criteria/workflow"
)

// Sink receives engine-level events. Implementations (typically the server
// transport) are responsible for assigning sequence numbers, persisting, and
// streaming. The engine never blocks waiting for the sink. The interpreter
// loop invokes OnRunStarted/OnRunCompleted/OnRunFailed. stepNode invokes
// OnStepEntered/OnStepOutcome/OnStepTransition and StepEventSink.
//
// OnVariableSet and OnStepOutputCaptured were added in W04. This is an
// internal interface; only server and Local sinks implement it.
type Sink interface {
	OnRunStarted(workflowName, initialStep string)
	OnRunCompleted(finalState string, success bool)
	OnRunFailed(reason, step string)
	OnStepEntered(step, adapterName string, attempt int)
	OnStepOutcome(step, outcome string, duration time.Duration, err error)
	OnStepTransition(from, to, viaOutcome string)
	OnStepResumed(step string, attempt int, reason string)
	// OnVariableSet is emitted when a workflow variable value is established (W04).
	OnVariableSet(name, value, source string)
	// OnStepOutputCaptured is emitted after a step produces captured outputs (W04).
	OnStepOutputCaptured(step string, outputs map[string]string)
	// OnRunPaused is called when the engine pauses at a wait or approval node
	// (W05). node is the node name, mode is "duration"|"signal", signal is the
	// pending signal name (empty for duration mode).
	OnRunPaused(node, mode, signal string)
	// OnWaitEntered is emitted when the engine enters a wait node (W05).
	OnWaitEntered(node, mode, duration, signal string)
	// OnWaitResumed is emitted when a wait node resolves (W05). payload is nil
	// for duration-mode waits; carries the resume payload for signal-mode waits.
	OnWaitResumed(node, mode, signal string, payload map[string]string)
	// OnApprovalRequested is emitted when the engine enters an approval node (W05).
	OnApprovalRequested(node string, approvers []string, reason string)
	// OnApprovalDecision is emitted when an approval node resolves via Resume (W05).
	// decision is "approved" or "rejected". actor is audit metadata.
	OnApprovalDecision(node, decision, actor string, payload map[string]string)
	// OnBranchEvaluated is emitted when a branch node selects a transition arm (W06).
	// matchedArm is "arm[<index>]" or "default"; target is the transition target.
	// condition is the source text of the matched arm expression; empty for default.
	OnBranchEvaluated(node, matchedArm, target, condition string)
	// OnForEachEntered is emitted when a step begins iterating (for_each or count) (W07/W10).
	// count is the total number of items.
	OnForEachEntered(node string, count int)
	// OnStepIterationStarted is emitted at the start of each per-item iteration (W10).
	// Formerly OnForEachIteration (W07); renamed for step-level semantics.
	// index is zero-based; value is the string-rendered cty value of the current
	// item; anyFailed reports whether any prior iteration produced a failure outcome.
	OnStepIterationStarted(node string, index int, value string, anyFailed bool)
	// OnStepIterationCompleted is emitted when a step finishes all iterations (W10).
	// Formerly OnForEachOutcome (W07).
	// outcome is "all_succeeded" or "any_failed"; target is the transition target.
	OnStepIterationCompleted(node, outcome, target string)
	// OnStepIterationItem is emitted when the engine is about to execute the
	// step body for the next iteration item (W10). Formerly OnForEachStep (W08).
	// node is the step name, index is the zero-based iteration index.
	// step is reserved for workflow-type steps; empty for non-workflow steps.
	OnStepIterationItem(node string, index int, step string)
	// OnScopeIterCursorSet is emitted whenever the step iteration cursor stack
	// is created, advanced, or cleared (W07/W10). cursorJSON is the JSON-encoded
	// cursor stack; an empty string signals cursor cleared. The server stores
	// this verbatim without interpreting field names.
	OnScopeIterCursorSet(cursorJSON string)
	// OnAdapterLifecycle is emitted at adapter session lifecycle events (W12).
	// status is one of: "started", "exited", "crashed".
	// stepName is the step that owns the event; adapterName is the adapter
	// (e.g. "noop", "copilot"); detail is a one-line description (empty for
	// clean events).
	OnAdapterLifecycle(stepName, adapterName, status, detail string)
	// OnRunOutputs is emitted when a run reaches terminal state with declared outputs (W09).
	// outputs is a list of (name, value, declared_type) tuples in declaration order.
	// This method is called before OnRunCompleted.
	OnRunOutputs(outputs []map[string]string)
	// StepEventSink returns the per-step adapter sink (logs + adapter events).
	StepEventSink(step string) adapter.EventSink
}

// Engine executes a single workflow run to a terminal state.
type Engine struct {
	graph               *workflow.FSMGraph
	loader              plugin.Loader
	sink                Sink
	subWorkflowResolver SubWorkflowResolver
	branchScheduler     BranchScheduler
	// resumedVars, when non-nil, overrides SeedVarsFromGraph at run start (W04).
	resumedVars map[string]cty.Value
	// resumedVisits, when non-nil, seeds RunState.Visits at run start (W07).
	// Used during crash-recovery reattach to restore per-step visit counts.
	resumedVisits map[string]int
	// varOverrides, when non-nil, overlays CLI-supplied key=value pairs on top
	// of the graph variable defaults at run start.
	varOverrides map[string]string
	// resumedIterStack, when non-empty, seeds RunState.IterStack at run start
	// (W10). Used during crash-recovery reattach when a step iteration was active.
	resumedIterStack []workflow.IterCursor
	// pendingSignal, when non-empty, is placed into RunState at run start (W05).
	// Used during crash-recovery reattach when the run was paused mid-signal-wait.
	pendingSignal string
	// resumePayload, when non-nil, is placed into RunState at run start (W05).
	// Used when the orchestrator delivers a resume signal to a paused run.
	resumePayload map[string]string
	// lastVars captures the Vars map from RunState when execution pauses so
	// the caller can pass them to the resumed engine via WithResumedVars (W05).
	lastVars map[string]cty.Value
	// lastVisits captures the Visits map from RunState when execution stops so
	// the caller can pass them to a resumed engine via WithResumedVisits (W07).
	lastVisits map[string]int
	// liveRunState is set to the active RunState while runLoop is executing,
	// allowing VisitCounts() to return live values for mid-run checkpoints (W07).
	// Cleared by handleEvalError when the run ends.
	liveRunState *RunState
	// workflowDir is the directory containing the HCL workflow file. Passed to
	// RunState so that file() and fileexists() can resolve relative paths.
	workflowDir string
	// log is an optional structured logger for internal engine warnings.
	// Falls back to slog.Default() when nil.
	log *slog.Logger
	// autoBootstrapAdapters, when true, auto-opens adapters without explicit lifecycle "open" steps.
	// This is a temporary measure pre-W12 to support workflows without explicit lifecycle management.
	// Defaults to true; can be disabled to enforce strict lifecycle semantics.
	autoBootstrapAdapters bool
}

func New(graph *workflow.FSMGraph, loader plugin.Loader, sink Sink, opts ...Option) *Engine {
	// Default to true for now (pre-W12 behavior); will flip to false when W12 lands.
	e := &Engine{graph: graph, loader: loader, sink: sink, autoBootstrapAdapters: true}
	for _, opt := range opts {
		if opt != nil {
			opt(e)
		}
	}
	return e
}

// VarScope returns the variable scope captured when the last run paused.
// Returns nil if the engine has not yet paused. Used by the CLI pause/resume
// loop to carry variable state across a resume boundary (W05).
func (e *Engine) VarScope() map[string]cty.Value { return e.lastVars }

// VisitCounts returns the per-step visit counts. During an active run it
// returns the live values from the current RunState so that mid-run
// checkpoints capture the correct counts. After the run ends it returns the
// snapshot captured by handleEvalError. Returns nil if the engine has not yet
// run. Used by the CLI crash-recovery path to persist visit state across a
// resume boundary (W07).
func (e *Engine) VisitCounts() map[string]int {
	if e.liveRunState != nil {
		return e.liveRunState.Visits
	}
	return e.lastVisits
}

// Run executes the workflow until a terminal state is reached, the global
// step limit is exceeded, or ctx is cancelled.
func (e *Engine) Run(ctx context.Context) error {
	sessions := plugin.NewSessionManager(e.loader)
	defer func() { _ = sessions.Shutdown(context.WithoutCancel(ctx)) }()

	// Bootstrap adapter sessions for workflows without explicit lifecycle steps (test-only).
	// Production workflows (W12) require explicit lifecycle management.
	if e.autoBootstrapAdapters {
		if err := e.bootstrapAllAdapters(ctx, sessions); err != nil {
			return err
		}
	}

	current := e.graph.InitialState
	e.sink.OnRunStarted(e.graph.Name, current)
	return e.runLoop(ctx, sessions, current, 1)
}

// bootstrapAllAdapters opens adapters that have no explicit lifecycle "open" steps.
// This is needed for workflows without explicit lifecycle management.
func (e *Engine) bootstrapAllAdapters(ctx context.Context, sessions *plugin.SessionManager) error {
	// Find which adapter instances have explicit lifecycle "open" steps
	adaptersWithLifecycleOpen := make(map[string]bool)
	for _, node := range e.graph.Steps {
		if node.Lifecycle == "open" {
			// The adapter reference is already in dotted form "<type>.<name>"
			adaptersWithLifecycleOpen[node.Adapter] = true
		}
	}

	// Bootstrap only adapters without explicit lifecycle steps
	for _, adapter := range e.graph.Adapters {
		instanceID := adapter.Type + "." + adapter.Name
		if adaptersWithLifecycleOpen[instanceID] {
			// This adapter instance has an explicit lifecycle step; don't bootstrap
			continue
		}
		if err := sessions.Open(ctx, instanceID, adapter.Type, adapter.OnCrash, adapter.Config); err != nil && !errors.Is(err, plugin.ErrSessionAlreadyOpen) {
			return fmt.Errorf("bootstrap adapter %q: %w", instanceID, err)
		}
	}
	return nil
}

// splitAdapterRef parses a dotted adapter reference like "noop.default" into ["noop", "default"]
func splitAdapterRef(ref string) []string {
	return strings.SplitN(ref, ".", 2)
}

// RunFrom resumes a workflow at startStep with the given initialAttempt
// number as the first attempt for that step (subsequent retries increment
// from there). It does NOT emit OnRunStarted (the run already started).
// If initialAttempt would already exceed max_step_retries, it emits
// OnRunFailed instead of attempting the step.
func (e *Engine) RunFrom(ctx context.Context, startStep string, initialAttempt int) error {
	sessions := plugin.NewSessionManager(e.loader)
	defer func() { _ = sessions.Shutdown(context.WithoutCancel(ctx)) }()

	if err := e.bootstrapSessionsForResume(ctx, sessions, startStep); err != nil {
		return err
	}
	// Also bootstrap any adapters that haven't been opened by explicit lifecycle steps (test-only).
	if e.autoBootstrapAdapters {
		if err := e.bootstrapAllAdapters(ctx, sessions); err != nil {
			return err
		}
	}
	return e.runLoop(ctx, sessions, startStep, initialAttempt)
}

// runLoop is the shared execution loop. firstStepAttempt is the attempt index
// used for the initial step when resuming; subsequent steps start at attempt 1.
func (e *Engine) runLoop(ctx context.Context, sessions *plugin.SessionManager, current string, firstStepAttempt int) error {
	vars := e.seedRunVars()
	st := &RunState{
		Current:          current,
		Vars:             vars,
		PendingSignal:    e.pendingSignal,
		ResumePayload:    e.resumePayload,
		IterStack:        append([]workflow.IterCursor{}, e.resumedIterStack...),
		Visits:           cloneVisits(e.resumedVisits),
		WorkflowDir:      e.workflowDir,
		firstStep:        true,
		firstStepAttempt: firstStepAttempt,
	}
	deps := e.buildDeps(sessions)

	e.liveRunState = st
	for {
		node, err := nodeFor(e.graph, st.Current)
		if err != nil {
			e.sink.OnRunFailed(err.Error(), st.Current)
			return err
		}
		next, err := node.Evaluate(ctx, st, deps)
		if err != nil {
			return e.handleEvalError(st, err)
		}
		next = e.routeIteratingStep(st, next)
		e.advanceTo(st, next)
	}
}

// routeIteratingStep handles post-step routing for steps with active iteration
// cursors (W10). Delegates to routeIteratingStepInGraph using the engine's
// own graph and sink. See routeIteratingStepInGraph for full semantics.
func (e *Engine) routeIteratingStep(st *RunState, next string) string {
	return routeIteratingStepInGraph(st, next, e.graph, e.sink)
}

// routeIteratingStepInGraph is the graph-agnostic iteration router called by
// both the engine's main loop and the workflow-body sub-loop. It checks
// whether the top cursor belongs to the current step and applies the
// appropriate iteration semantics:
//
//   - No active cursor → return next unchanged.
//   - More iterations remain → re-bind each.*, emit started event, re-enter step.
//   - All iterations done (or on_failure=abort after failure) → pop cursor,
//     emit completed event, return aggregate outcome target from graph.
func routeIteratingStepInGraph(st *RunState, next string, graph *workflow.FSMGraph, sink Sink) string { //nolint:funlen // iteration router is inherently stateful; splitting adds indirection
	cur := st.TopCursor()
	if cur == nil || !cur.InProgress {
		return next
	}

	stepName := cur.StepName
	// Only intercept when the current node is the iterating step itself.
	// When the step has a workflow body (_continue comes from the body's
	// terminal state), next will be "_continue".
	if st.Current != stepName && next != "_continue" {
		return next
	}

	// Record outcome for this iteration.
	outcomeIsSuccess := isSuccessOutcome(st.LastOutcome)
	if !outcomeIsSuccess {
		cur.AnyFailed = true
	}

	// Workflow-body early-exit: body reached a terminal state other than
	// "_continue" — stop the entire iteration immediately.
	if cur.EarlyExit {
		return finishIterationInGraph(st, stepName, graph, sink)
	}

	// on_failure=abort: stop after first failure.
	if cur.OnFailure == "abort" && !outcomeIsSuccess {
		return finishIterationInGraph(st, stepName, graph, sink)
	}

	cur.Index++
	cur.InProgress = false

	if cur.Index < cur.Total {
		// More iterations remain: re-bind each.* and re-enter the step.
		item := cur.Items[cur.Index]
		var key cty.Value
		if cur.Index < len(cur.Keys) {
			key = cur.Keys[cur.Index]
		} else {
			key = cty.StringVal(fmt.Sprintf("%d", cur.Index))
		}
		cur.Key = key
		cur.InProgress = true
		st.Vars = workflow.WithEachBinding(st.Vars, &workflow.EachBinding{
			Value: item,
			Key:   key,
			Index: cur.Index,
			Total: cur.Total,
			First: cur.Index == 0,
			Last:  cur.Index == cur.Total-1,
			Prev:  cur.Prev,
		})
		if curJSON, err := workflow.SerializeIterCursor(cur); err == nil {
			sink.OnScopeIterCursorSet(curJSON)
		}
		sink.OnStepIterationStarted(stepName, cur.Index, workflow.CtyValueToString(item), cur.AnyFailed)
		return stepName // re-enter the same step
	}

	// All iterations done.
	return finishIterationInGraph(st, stepName, graph, sink)
}

// finishIterationInGraph closes out an iteration loop: pops the cursor, clears
// each.* bindings, emits OnStepIterationCompleted, and returns the aggregate
// outcome target looked up from graph.
func finishIterationInGraph(st *RunState, stepName string, graph *workflow.FSMGraph, sink Sink) string {
	cur := st.PopCursor()
	st.Vars = workflow.ClearEachBinding(st.Vars)
	sink.OnScopeIterCursorSet("") // cursor cleared

	step, ok := graph.Steps[stepName]
	if !ok {
		return stepName
	}

	aggregateOutcome := "all_succeeded"
	if cur.AnyFailed && cur.OnFailure != "ignore" {
		aggregateOutcome = "any_failed"
	}

	target, ok := step.Outcomes[aggregateOutcome]
	if !ok {
		// Fall back to all_succeeded (required by compile; missing any_failed is
		// a compile warning, not an error).
		target = step.Outcomes["all_succeeded"]
	}

	sink.OnStepIterationCompleted(stepName, aggregateOutcome, target)
	return target
}

// returns the restored scope unchanged. For fresh runs it seeds from graph
// defaults, applies any CLI overrides, and emits OnVariableSet events.
func (e *Engine) seedRunVars() map[string]cty.Value {
	if e.resumedVars != nil {
		// Locals are compile-time constants that are never persisted in the
		// scope snapshot. Always reseed them from the current graph so that
		// resumed runs have the same local.* bindings as fresh runs.
		resumed := make(map[string]cty.Value, len(e.resumedVars)+1)
		for k, v := range e.resumedVars {
			resumed[k] = v
		}
		resumed["local"] = workflow.SeedLocalsFromGraph(e.graph)
		return resumed
	}
	vars := workflow.SeedVarsFromGraph(e.graph)
	vars["local"] = workflow.SeedLocalsFromGraph(e.graph)
	if len(e.varOverrides) > 0 {
		vars = workflow.ApplyVarOverrides(e.graph, vars, e.varOverrides)
	}
	// Fresh run: emit OnVariableSet for each variable that has a value.
	for name, node := range e.graph.Variables {
		if ov, ok := e.varOverrides[name]; ok {
			e.sink.OnVariableSet(name, ov, "override")
		} else if node.Default != cty.NilVal {
			e.sink.OnVariableSet(name, workflow.CtyValueToString(node.Default), "default")
		}
	}
	return vars
}

// buildDeps constructs the Deps bundle injected into each node's Evaluate call.
func (e *Engine) buildDeps(sessions *plugin.SessionManager) Deps {
	return Deps{
		Sessions:            sessions,
		Sink:                e.sink,
		SubWorkflowResolver: e.subWorkflowResolver,
		BranchScheduler:     e.branchScheduler,
	}
}

// advanceTo sets st.Current to next, moving the run forward to the next node.
func (e *Engine) advanceTo(st *RunState, next string) {
	st.Current = next
}

// handleEvalError dispatches errors from node.Evaluate. It handles ErrTerminal
// and ErrPaused specially; all other errors are propagated as run failures.
func (e *Engine) handleEvalError(st *RunState, err error) error {
	// Capture the visit state and clear the live pointer so VisitCounts()
	// returns a stable snapshot after the run ends (W07).
	e.liveRunState = nil
	e.lastVisits = st.Visits
	if errors.Is(err, engineruntime.ErrTerminal) {
		state, ok := e.graph.States[st.Current]
		if !ok {
			missing := fmt.Errorf("terminal node %q is not a state", st.Current)
			e.sink.OnRunFailed(missing.Error(), st.Current)
			return missing
		}
		// Evaluate outputs at terminal state (W09).
		outputs, outErr := evalRunOutputs(e.graph, st)
		if outErr != nil {
			// Output evaluation failed; emit error and fail the run.
			e.sink.OnRunFailed(outErr.Error(), st.Current)
			return outErr
		}
		// Emit outputs before run.completed if present.
		if len(outputs) > 0 {
			e.sink.OnRunOutputs(outputs)
		}
		e.sink.OnRunCompleted(state.Name, state.Success)
		return nil
	}
	if errors.Is(err, engineruntime.ErrPaused) {
		// The node has already set st.PendingSignal and emitted WaitEntered/
		// ApprovalRequested. Notify the sink so it can update run status and
		// then yield control back to the orchestrator.
		mode := "signal"
		if wait, ok := e.graph.Waits[st.Current]; ok && wait.Duration > 0 {
			mode = "duration"
		}
		e.lastVars = st.Vars
		e.sink.OnRunPaused(st.Current, mode, st.PendingSignal)
		return nil
	}
	e.sink.OnRunFailed(err.Error(), st.Current)
	return err
}

// cloneVisits returns a shallow copy of the visits map, or nil if the input is nil.
func cloneVisits(v map[string]int) map[string]int {
	if v == nil {
		return nil
	}
	out := make(map[string]int, len(v))
	for k, c := range v {
		out[k] = c
	}
	return out
}

func (e *Engine) bootstrapSessionsForResume(ctx context.Context, sessions *plugin.SessionManager, startStep string) error {
	// Sessions are process-local and do not survive adapter restarts.
	// Crash recovery recreates them by replaying lifecycle steps declared before
	// the resumed step in declaration order.
	for _, name := range e.graph.StepOrder() {
		if name == startStep {
			break
		}
		step, ok := e.graph.Steps[name]
		if !ok || step.Adapter == "" {
			continue
		}
		switch step.Lifecycle {
		case "open":
			adapter, ok := e.graph.Adapters[step.Adapter]
			if !ok {
				return fmt.Errorf("unknown adapter %q in step %q", step.Adapter, step.Name)
			}
			if err := sessions.Open(ctx, step.Adapter, adapter.Type, step.OnCrash, adapter.Config); err != nil && !errors.Is(err, plugin.ErrSessionAlreadyOpen) {
				return fmt.Errorf("restore session for adapter %q: %w", step.Adapter, err)
			}
		case "close":
			if err := sessions.Close(ctx, step.Adapter); err != nil {
				return fmt.Errorf("restore close for adapter %q: %w", step.Adapter, err)
			}
		}
	}
	return nil
}

// ErrCancelled is returned when the run context is cancelled mid-step.
var ErrCancelled = errors.New("run cancelled")
