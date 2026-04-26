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

	startAttempt := 1
	if st.firstStep {
		st.firstStep = false
		if st.firstStepAttempt > 1 {
			startAttempt = st.firstStepAttempt
		}
	}

	outcome, err := n.runStepFromAttempt(ctx, deps, startAttempt)
	if err != nil {
		return "", err
	}

	next, ok := n.step.Outcomes[outcome]
	if !ok {
		return "", fmt.Errorf("step %q produced unmapped outcome %q", n.step.Name, outcome)
	}
	deps.Sink.OnStepTransition(n.step.Name, next, outcome)
	return next, nil
}

func (n *stepNode) runStepFromAttempt(ctx context.Context, deps Deps, startAttempt int) (string, error) {
	maxAttempts := 1 + n.graph.Policy.MaxStepRetries
	if startAttempt > maxAttempts {
		return "", fmt.Errorf("step %q has no remaining attempts (start attempt %d exceeds max %d)", n.step.Name, startAttempt, maxAttempts)
	}

	var lastErr error
	for attempt := startAttempt; attempt <= maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		deps.Sink.OnStepEntered(n.step.Name, n.stepAdapterName(), attempt)

		stepCtx := ctx
		var cancel context.CancelFunc
		if n.step.Timeout > 0 {
			stepCtx, cancel = context.WithTimeout(ctx, n.step.Timeout)
		}

		start := time.Now()
		result, err := n.executeStep(stepCtx, deps)
		if cancel != nil {
			cancel()
		}
		dur := time.Since(start)

		if err == nil {
			deps.Sink.OnStepOutcome(n.step.Name, result.Outcome, dur, nil)
			return result.Outcome, nil
		}

		var fatal *plugin.FatalRunError
		if errors.As(err, &fatal) {
			deps.Sink.OnStepOutcome(n.step.Name, "failure", dur, err)
			return "", err
		}

		lastErr = err
		if _, hasFailure := n.step.Outcomes["failure"]; hasFailure {
			deps.Sink.OnStepOutcome(n.step.Name, "failure", dur, err)
			return "failure", nil
		}
		deps.Sink.OnStepOutcome(n.step.Name, "", dur, err)
	}

	return "", fmt.Errorf("step %q failed after %d attempts: %w", n.step.Name, maxAttempts-startAttempt+1, lastErr)
}

func (n *stepNode) executeStep(ctx context.Context, deps Deps) (adapter.Result, error) {
	if n.step.Lifecycle == "open" {
		agent, ok := n.graph.Agents[n.step.Agent]
		if !ok {
			return adapter.Result{Outcome: "failure"}, fmt.Errorf("unknown agent %q", n.step.Agent)
		}
		if err := deps.Sessions.Open(ctx, n.step.Agent, agent.Adapter, n.step.OnCrash, agent.Config); err != nil {
			return adapter.Result{Outcome: "failure"}, err
		}
		return adapter.Result{Outcome: "success"}, nil
	}
	if n.step.Lifecycle == "close" {
		if err := deps.Sessions.Close(ctx, n.step.Agent); err != nil {
			return adapter.Result{Outcome: "failure"}, err
		}
		return adapter.Result{Outcome: "success"}, nil
	}
	if n.step.Agent != "" {
		return deps.Sessions.Execute(ctx, n.step.Agent, n.step, deps.Sink.StepEventSink(n.step.Name))
	}

	anonSessionID := "anon-" + uuid.NewString()
	if err := deps.Sessions.Open(ctx, anonSessionID, n.step.Adapter, plugin.OnCrashFail, nil); err != nil {
		return adapter.Result{Outcome: "failure"}, err
	}
	defer deps.Sessions.Close(context.Background(), anonSessionID)

	return deps.Sessions.Execute(ctx, anonSessionID, n.step, deps.Sink.StepEventSink(n.step.Name))
}

func (n *stepNode) stepAdapterName() string {
	if n.step.Agent != "" {
		if agent, ok := n.graph.Agents[n.step.Agent]; ok {
			return agent.Adapter
		}
	}
	return n.step.Adapter
}
