// Package engine drives a workflow's FSM. It is pure: dispatcher (adapter
// lookup) and event sink are injected so the engine can be tested without I/O.
package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/brokenbots/criteria/internal/adapter"
	engineruntime "github.com/brokenbots/criteria/internal/engine/runtime"
	"github.com/brokenbots/criteria/internal/plugin"
	"github.com/brokenbots/criteria/workflow"
	"github.com/zclconf/go-cty/cty"
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
	// OnForEachEntered is emitted when a for_each node begins iterating (W07).
	// count is the total number of items in the list.
	OnForEachEntered(node string, count int)
	// OnForEachIteration is emitted at the start of each per-item iteration (W07).
	// index is zero-based; value is the string-rendered cty value of the current
	// item; anyFailed reports whether any prior iteration produced a failure outcome.
	OnForEachIteration(node string, index int, value string, anyFailed bool)
	// OnForEachOutcome is emitted when a for_each node finishes iterating (W07).
	// outcome is "all_succeeded" or "any_failed"; target is the transition target.
	OnForEachOutcome(node, outcome, target string)
	// OnScopeIterCursorSet is emitted whenever the for_each cursor is created,
	// advanced, or cleared (W07). cursorJSON is the JSON-encoded IterCursor; an
	// empty string signals cursor cleared. The server stores this verbatim without
	// interpreting field names, preserving 1.6 split independence.
	OnScopeIterCursorSet(cursorJSON string)
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
	// varOverrides, when non-nil, overlays CLI-supplied key=value pairs on top
	// of the graph variable defaults at run start.
	varOverrides map[string]string
	// resumedIter, when non-nil, sets RunState.Iter at run start (W07).
	// Used during crash-recovery reattach when a for_each was active.
	resumedIter *workflow.IterCursor
	// pendingSignal, when non-empty, is placed into RunState at run start (W05).
	// Used during crash-recovery reattach when the run was paused mid-signal-wait.
	pendingSignal string
	// resumePayload, when non-nil, is placed into RunState at run start (W05).
	// Used when the orchestrator delivers a resume signal to a paused run.
	resumePayload map[string]string
	// lastVars captures the Vars map from RunState when execution pauses so
	// the caller can pass them to the resumed engine via WithResumedVars (W05).
	lastVars map[string]cty.Value
}

func New(graph *workflow.FSMGraph, loader plugin.Loader, sink Sink, opts ...Option) *Engine {
	e := &Engine{graph: graph, loader: loader, sink: sink}
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

// Run executes the workflow until a terminal state is reached, the global
// step limit is exceeded, or ctx is cancelled.
func (e *Engine) Run(ctx context.Context) error {
	sessions := plugin.NewSessionManager(e.loader)
	defer sessions.Shutdown(context.Background())

	current := e.graph.InitialState
	e.sink.OnRunStarted(e.graph.Name, current)
	return e.runLoop(ctx, sessions, current, 1)
}

// RunFrom resumes a workflow at startStep with the given initialAttempt
// number as the first attempt for that step (subsequent retries increment
// from there). It does NOT emit OnRunStarted (the run already started).
// If initialAttempt would already exceed max_step_retries, it emits
// OnRunFailed instead of attempting the step.
func (e *Engine) RunFrom(ctx context.Context, startStep string, initialAttempt int) error {
	sessions := plugin.NewSessionManager(e.loader)
	defer sessions.Shutdown(context.Background())

	if err := e.bootstrapSessionsForResume(ctx, sessions, startStep); err != nil {
		return err
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
		Iter:             e.resumedIter,
		firstStep:        true,
		firstStepAttempt: firstStepAttempt,
	}
	deps := e.buildDeps(sessions)

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
		next = e.interceptForEachContinue(st, next)
		e.advanceTo(st, next)
	}
}

// seedRunVars returns the initial variable map for a run. For resumed runs it
// returns the restored scope unchanged. For fresh runs it seeds from graph
// defaults, applies any CLI overrides, and emits OnVariableSet events.
func (e *Engine) seedRunVars() map[string]cty.Value {
	if e.resumedVars != nil {
		return e.resumedVars
	}
	vars := workflow.SeedVarsFromGraph(e.graph)
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

// interceptForEachContinue handles _continue transitions for for_each iteration
// (W07). W08 changes the semantics of this helper; the narrow signature
// (RunState + next string → next string) gives W08 an isolated edit point.
func (e *Engine) interceptForEachContinue(st *RunState, next string) string {
	// _continue: advance the iteration cursor and route back to the for_each node.
	// Guard: only intercept when the current node is NOT the for_each node itself
	// (to avoid matching the dispatch return from forEachNode.Evaluate).
	if next == "_continue" && st.Iter != nil && st.Iter.InProgress && st.Current != st.Iter.NodeName {
		// Non-success outcomes set AnyFailed. Convention: any outcome name that
		// is not exactly "success" (case-insensitive) is treated as a failure.
		if !isSuccessOutcome(st.LastOutcome) {
			st.Iter.AnyFailed = true
		}
		st.Iter.Index++
		st.Iter.InProgress = false
		// Clear each.* bindings now that the iteration step is done.
		st.Vars = workflow.ClearEachBinding(st.Vars)
		if cursorJSON, curErr := workflow.SerializeIterCursor(st.Iter); curErr == nil {
			e.sink.OnScopeIterCursorSet(cursorJSON)
		}
		return st.Iter.NodeName
	}
	// Early-exit: a per-iteration step transitioned to a non-_continue target
	// while Iter is active. Clear the cursor and follow the target directly.
	if st.Iter != nil && st.Iter.InProgress && st.Current != st.Iter.NodeName {
		iterName := st.Iter.NodeName
		st.Iter = nil
		st.Vars = workflow.ClearEachBinding(st.Vars)
		e.sink.OnScopeIterCursorSet("") // cursor cleared
		e.sink.OnForEachOutcome(iterName, "any_failed", next)
	}
	return next
}

// advanceTo sets st.Current to next, moving the run forward to the next node.
func (e *Engine) advanceTo(st *RunState, next string) {
	st.Current = next
}

// handleEvalError dispatches errors from node.Evaluate. It handles ErrTerminal
// and ErrPaused specially; all other errors are propagated as run failures.
func (e *Engine) handleEvalError(st *RunState, err error) error {
	if errors.Is(err, engineruntime.ErrTerminal) {
		state, ok := e.graph.States[st.Current]
		if !ok {
			missing := fmt.Errorf("terminal node %q is not a state", st.Current)
			e.sink.OnRunFailed(missing.Error(), st.Current)
			return missing
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

func (e *Engine) bootstrapSessionsForResume(ctx context.Context, sessions *plugin.SessionManager, startStep string) error {
	// Sessions are process-local and do not survive agent restarts.
	// Crash recovery recreates them by replaying lifecycle steps declared before
	// the resumed step in declaration order.
	for _, name := range e.graph.StepOrder() {
		if name == startStep {
			break
		}
		step, ok := e.graph.Steps[name]
		if !ok || step.Agent == "" {
			continue
		}
		switch step.Lifecycle {
		case "open":
			agent, ok := e.graph.Agents[step.Agent]
			if !ok {
				return fmt.Errorf("unknown agent %q in step %q", step.Agent, step.Name)
			}
			if err := sessions.Open(ctx, step.Agent, agent.Adapter, step.OnCrash, agent.Config); err != nil && !errors.Is(err, plugin.ErrSessionAlreadyOpen) {
				return fmt.Errorf("restore session for agent %q: %w", step.Agent, err)
			}
		case "close":
			if err := sessions.Close(ctx, step.Agent); err != nil {
				return fmt.Errorf("restore close for agent %q: %w", step.Agent, err)
			}
		}
	}
	return nil
}

// ErrCancelled is returned when the run context is cancelled mid-step.
var ErrCancelled = errors.New("run cancelled")
