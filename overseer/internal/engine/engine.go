// Package engine drives a workflow's FSM. It is pure: dispatcher (adapter
// lookup) and event sink are injected so the engine can be tested without I/O.
package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/brokenbots/overlord/overseer/internal/adapter"
	engineruntime "github.com/brokenbots/overlord/overseer/internal/engine/runtime"
	"github.com/brokenbots/overlord/overseer/internal/plugin"
	"github.com/brokenbots/overlord/workflow"
	"github.com/zclconf/go-cty/cty"
)

// Sink receives engine-level events. Implementations (typically the Castle
// transport) are responsible for assigning sequence numbers, persisting, and
// streaming. The engine never blocks waiting for the sink. The interpreter
// loop invokes OnRunStarted/OnRunCompleted/OnRunFailed. stepNode invokes
// OnStepEntered/OnStepOutcome/OnStepTransition and StepEventSink.
//
// OnVariableSet and OnStepOutputCaptured were added in W04. This is an
// internal interface; only Castle and Local sinks implement it.
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
	vars := workflow.SeedVarsFromGraph(e.graph)
	if e.resumedVars != nil {
		vars = e.resumedVars
	} else {
		// Fresh run: emit OnVariableSet for each variable that has a default.
		for name, node := range e.graph.Variables {
			if node.Default != cty.NilVal {
				e.sink.OnVariableSet(name, workflow.CtyValueToString(node.Default), "default")
			}
		}
	}
	st := &RunState{
		Current:          current,
		Vars:             vars,
		PendingSignal:    e.pendingSignal,
		ResumePayload:    e.resumePayload,
		Iter:             nil,
		ParentRunID:      "",
		BranchID:         "",
		firstStep:        true,
		firstStepAttempt: firstStepAttempt,
	}
	deps := Deps{
		Sessions:            sessions,
		Sink:                e.sink,
		SubWorkflowResolver: e.subWorkflowResolver,
		BranchScheduler:     e.branchScheduler,
	}

	for {
		node, err := nodeFor(e.graph, st.Current)
		if err != nil {
			e.sink.OnRunFailed(err.Error(), st.Current)
			return err
		}

		next, err := node.Evaluate(ctx, st, deps)
		if err == nil {
			st.Current = next
			continue
		}
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
}

func (e *Engine) bootstrapSessionsForResume(ctx context.Context, sessions *plugin.SessionManager, startStep string) error {
	// Sessions are process-local and do not survive Overseer restarts.
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
