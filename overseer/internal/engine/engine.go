// Package engine drives a workflow's FSM. It is pure: dispatcher (adapter
// lookup) and event sink are injected so the engine can be tested without I/O.
package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/brokenbots/overlord/overseer/internal/adapter"
	"github.com/brokenbots/overlord/overseer/internal/plugin"
	"github.com/brokenbots/overlord/workflow"
)

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
	graph  *workflow.FSMGraph
	loader plugin.Loader
	sink   Sink
}

func New(graph *workflow.FSMGraph, loader plugin.Loader, sink Sink) *Engine {
	return &Engine{graph: graph, loader: loader, sink: sink}
}

// Run executes the workflow until a terminal state is reached, the global
// step limit is exceeded, or ctx is cancelled.
func (e *Engine) Run(ctx context.Context) error {
	sessions := plugin.NewSessionManager(e.loader)
	defer sessions.Shutdown(context.Background())

	current := e.graph.InitialState
	e.sink.OnRunStarted(e.graph.Name, current)
	return e.runLoop(ctx, sessions, current, 0)
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
	return e.runLoop(ctx, sessions, startStep, initialAttempt-1)
}

// runLoop is the shared execution loop. attemptOffset is added to the
// attempt counter of the first step only; subsequent steps start at attempt 1.
func (e *Engine) runLoop(ctx context.Context, sessions *plugin.SessionManager, current string, firstStepAttemptOffset int) error {
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
			outcome, err = e.runStepFromAttempt(ctx, sessions, step, firstStepAttemptOffset+1)
		} else {
			firstStep = false
			outcome, err = e.runStepFromAttempt(ctx, sessions, step, 1)
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

func (e *Engine) runStepFromAttempt(ctx context.Context, sessions *plugin.SessionManager, step *workflow.StepNode, startAttempt int) (string, error) {
	maxAttempts := 1 + e.graph.Policy.MaxStepRetries
	if startAttempt > maxAttempts {
		return "", fmt.Errorf("step %q has no remaining attempts (start attempt %d exceeds max %d)", step.Name, startAttempt, maxAttempts)
	}
	var lastErr error
	for attempt := startAttempt; attempt <= maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		e.sink.OnStepEntered(step.Name, e.stepAdapterName(step), attempt)

		stepCtx := ctx
		var cancel context.CancelFunc
		if step.Timeout > 0 {
			stepCtx, cancel = context.WithTimeout(ctx, step.Timeout)
		}

		start := time.Now()
		result, err := e.executeStep(stepCtx, sessions, step)
		if cancel != nil {
			cancel()
		}
		dur := time.Since(start)

		if err == nil {
			e.sink.OnStepOutcome(step.Name, result.Outcome, dur, nil)
			return result.Outcome, nil
		}
		var fatal *plugin.FatalRunError
		if errors.As(err, &fatal) {
			e.sink.OnStepOutcome(step.Name, "failure", dur, err)
			return "", err
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

func (e *Engine) executeStep(ctx context.Context, sessions *plugin.SessionManager, step *workflow.StepNode) (adapter.Result, error) {
	if step.Lifecycle == "open" {
		agent, ok := e.graph.Agents[step.Agent]
		if !ok {
			return adapter.Result{Outcome: "failure"}, fmt.Errorf("unknown agent %q", step.Agent)
		}
		if err := sessions.Open(ctx, step.Agent, agent.Adapter, step.OnCrash, step.Config); err != nil {
			return adapter.Result{Outcome: "failure"}, err
		}
		return adapter.Result{Outcome: "success"}, nil
	}
	if step.Lifecycle == "close" {
		if err := sessions.Close(ctx, step.Agent); err != nil {
			return adapter.Result{Outcome: "failure"}, err
		}
		return adapter.Result{Outcome: "success"}, nil
	}
	if step.Agent != "" {
		return sessions.Execute(ctx, step.Agent, step, e.sink.StepEventSink(step.Name))
	}

	anonSessionID := "anon-" + uuid.NewString()
	if err := sessions.Open(ctx, anonSessionID, step.Adapter, plugin.OnCrashFail, nil); err != nil {
		return adapter.Result{Outcome: "failure"}, err
	}
	defer sessions.Close(context.Background(), anonSessionID)

	return sessions.Execute(ctx, anonSessionID, step, e.sink.StepEventSink(step.Name))
}

func (e *Engine) stepAdapterName(step *workflow.StepNode) string {
	if step.Agent != "" {
		if agent, ok := e.graph.Agents[step.Agent]; ok {
			return agent.Adapter
		}
	}
	return step.Adapter
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
			if err := sessions.Open(ctx, step.Agent, agent.Adapter, step.OnCrash, step.Config); err != nil && !errors.Is(err, plugin.ErrSessionAlreadyOpen) {
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
