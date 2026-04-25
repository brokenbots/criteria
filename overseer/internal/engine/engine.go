// Package engine drives a workflow's FSM. It is pure: dispatcher (adapter
// lookup) and event sink are injected so the engine can be tested without I/O.
package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/brokenbots/overlord/overseer/internal/adapter"
	"github.com/brokenbots/overlord/workflow"
)

// Dispatcher resolves an adapter by name.
type Dispatcher interface {
	Adapter(name string) (adapter.Adapter, bool)
}

// Sink receives engine-level events. Implementations (typically the Castle
// transport) are responsible for assigning sequence numbers, persisting, and
// streaming. The engine never blocks waiting for the sink.
type Sink interface {
	OnRunStarted(workflowName, initialStep string)
	OnRunCompleted(finalState string, success bool)
	OnRunFailed(reason, step string)
	OnStepEntered(step, adapterName string, attempt int)
	OnStepOutcome(step, outcome string, duration time.Duration, err error)
	OnStepTransition(from, to, viaOutcome string)
	OnStepResumed(step string, attempt int, reason string)
	// StepEventSink returns the per-step adapter sink (logs + adapter events).
	StepEventSink(step string) adapter.EventSink
}

// Engine executes a single workflow run to a terminal state.
type Engine struct {
	graph      *workflow.FSMGraph
	dispatcher Dispatcher
	sink       Sink
}

func New(graph *workflow.FSMGraph, dispatcher Dispatcher, sink Sink) *Engine {
	return &Engine{graph: graph, dispatcher: dispatcher, sink: sink}
}

// Run executes the workflow until a terminal state is reached, the global
// step limit is exceeded, or ctx is cancelled.
func (e *Engine) Run(ctx context.Context) error {
	current := e.graph.InitialState
	e.sink.OnRunStarted(e.graph.Name, current)
	return e.runLoop(ctx, current, 0)
}

// RunFrom resumes a workflow at startStep with the given initialAttempt
// number as the first attempt for that step (subsequent retries increment
// from there). It does NOT emit OnRunStarted (the run already started).
// If initialAttempt would already exceed max_step_retries, it emits
// OnRunFailed instead of attempting the step.
func (e *Engine) RunFrom(ctx context.Context, startStep string, initialAttempt int) error {
	return e.runLoop(ctx, startStep, initialAttempt-1)
}

// runLoop is the shared execution loop. attemptOffset is added to the
// attempt counter of the first step only; subsequent steps start at attempt 1.
func (e *Engine) runLoop(ctx context.Context, current string, firstStepAttemptOffset int) error {
	totalSteps := 0
	firstStep := true
	for {
		// Reached a state node: terminal => done; non-terminal => awaiting external input (treat as terminal for Phase 0).
		if state, ok := e.graph.States[current]; ok {
			e.sink.OnRunCompleted(state.Name, state.Success)
			return nil
		}
		step, ok := e.graph.Steps[current]
		if !ok {
			err := fmt.Errorf("unknown node %q", current)
			e.sink.OnRunFailed(err.Error(), current)
			return err
		}

		totalSteps++
		if totalSteps > e.graph.Policy.MaxTotalSteps {
			err := fmt.Errorf("policy.max_total_steps exceeded (%d)", e.graph.Policy.MaxTotalSteps)
			e.sink.OnRunFailed(err.Error(), step.Name)
			return err
		}

		var (
			outcome string
			err     error
		)
		if firstStep && firstStepAttemptOffset > 0 {
			firstStep = false
			outcome, err = e.runStepFromAttempt(ctx, step, firstStepAttemptOffset+1)
		} else {
			firstStep = false
			outcome, err = e.runStepFromAttempt(ctx, step, 1)
		}
		if err != nil {
			e.sink.OnRunFailed(err.Error(), step.Name)
			return err
		}

		next, ok := step.Outcomes[outcome]
		if !ok {
			err := fmt.Errorf("step %q produced unmapped outcome %q", step.Name, outcome)
			e.sink.OnRunFailed(err.Error(), step.Name)
			return err
		}
		e.sink.OnStepTransition(step.Name, next, outcome)
		current = next
	}
}

func (e *Engine) runStepFromAttempt(ctx context.Context, step *workflow.StepNode, startAttempt int) (string, error) {
	ad, ok := e.dispatcher.Adapter(step.Adapter)
	if !ok {
		return "", fmt.Errorf("no adapter named %q", step.Adapter)
	}
	maxAttempts := 1 + e.graph.Policy.MaxStepRetries
	if startAttempt > maxAttempts {
		return "", fmt.Errorf("step %q has no remaining attempts (start attempt %d exceeds max %d)", step.Name, startAttempt, maxAttempts)
	}
	var lastErr error
	for attempt := startAttempt; attempt <= maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		e.sink.OnStepEntered(step.Name, step.Adapter, attempt)

		stepCtx := ctx
		var cancel context.CancelFunc
		if step.Timeout > 0 {
			stepCtx, cancel = context.WithTimeout(ctx, step.Timeout)
		}

		start := time.Now()
		result, err := ad.Execute(stepCtx, step, e.sink.StepEventSink(step.Name))
		if cancel != nil {
			cancel()
		}
		dur := time.Since(start)

		if err == nil {
			e.sink.OnStepOutcome(step.Name, result.Outcome, dur, nil)
			return result.Outcome, nil
		}
		lastErr = err
		// On error, treat as "failure" outcome if mapped; otherwise retry.
		if _, hasFailure := step.Outcomes["failure"]; hasFailure {
			e.sink.OnStepOutcome(step.Name, "failure", dur, err)
			return "failure", nil
		}
		e.sink.OnStepOutcome(step.Name, "", dur, err)
		// no-op; loop until retries exhausted
	}
	return "", fmt.Errorf("step %q failed after %d attempts: %w", step.Name, maxAttempts-startAttempt+1, lastErr)
}

// ErrCancelled is returned when the run context is cancelled mid-step.
var ErrCancelled = errors.New("run cancelled")
