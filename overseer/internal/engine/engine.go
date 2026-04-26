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
)

// Sink receives engine-level events. Implementations (typically the Castle
// transport) are responsible for assigning sequence numbers, persisting, and
// streaming. The engine never blocks waiting for the sink. The interpreter
// loop invokes OnRunStarted/OnRunCompleted/OnRunFailed. stepNode invokes
// OnStepEntered/OnStepOutcome/OnStepTransition and StepEventSink.
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
	graph               *workflow.FSMGraph
	loader              plugin.Loader
	sink                Sink
	subWorkflowResolver SubWorkflowResolver
	branchScheduler     BranchScheduler
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
	st := &RunState{
		Current:          current,
		Vars:             nil,
		PendingSignal:    "",
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
