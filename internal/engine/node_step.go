package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/zclconf/go-cty/cty"

	"github.com/brokenbots/overseer/internal/adapter"
	"github.com/brokenbots/overseer/internal/plugin"
	"github.com/brokenbots/overseer/workflow"
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

	// Resolve input HCL expressions against current run vars (W04).
	effectiveStep, resolveErr := n.resolveInput(st.Vars)
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

	result, err := n.runStepFromAttempt(ctx, deps, effectiveStep, startAttempt)
	if err != nil {
		return "", err
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
	// Record the outcome name so the engine loop can inspect it when
	// intercepting a _continue transition (W07 for_each support).
	st.LastOutcome = result.Outcome
	deps.Sink.OnStepTransition(n.step.Name, next, result.Outcome)
	return next, nil
}

// resolveInput returns the step with Input populated from evaluated HCL
// expressions. It returns an error if any expression fails to evaluate so
// the caller can fail fast rather than silently using a placeholder value.
func (n *stepNode) resolveInput(vars map[string]cty.Value) (*workflow.StepNode, error) {
	if len(n.step.InputExprs) == 0 {
		return n.step, nil
	}
	resolved, err := workflow.ResolveInputExprs(n.step.InputExprs, vars)
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

func (n *stepNode) runStepFromAttempt(ctx context.Context, deps Deps, step *workflow.StepNode, startAttempt int) (adapter.Result, error) {
	maxAttempts := 1 + n.graph.Policy.MaxStepRetries
	if startAttempt > maxAttempts {
		return adapter.Result{}, fmt.Errorf("step %q has no remaining attempts (start attempt %d exceeds max %d)", step.Name, startAttempt, maxAttempts)
	}

	var lastErr error
	for attempt := startAttempt; attempt <= maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
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
		if err := deps.Sessions.Open(ctx, step.Agent, agent.Adapter, step.OnCrash, agent.Config); err != nil {
			return adapter.Result{Outcome: "failure"}, err
		}
		return adapter.Result{Outcome: "success"}, nil
	}
	if step.Lifecycle == "close" {
		if err := deps.Sessions.Close(ctx, step.Agent); err != nil {
			return adapter.Result{Outcome: "failure"}, err
		}
		return adapter.Result{Outcome: "success"}, nil
	}
	if step.Agent != "" {
		return deps.Sessions.Execute(ctx, step.Agent, step, deps.Sink.StepEventSink(step.Name))
	}

	anonSessionID := "anon-" + uuid.NewString()
	if err := deps.Sessions.Open(ctx, anonSessionID, step.Adapter, plugin.OnCrashFail, nil); err != nil {
		return adapter.Result{Outcome: "failure"}, err
	}
	defer deps.Sessions.Close(context.Background(), anonSessionID)

	return deps.Sessions.Execute(ctx, anonSessionID, step, deps.Sink.StepEventSink(step.Name))
}

func (n *stepNode) stepAdapterName() string {
	if n.step.Agent != "" {
		if agent, ok := n.graph.Agents[n.step.Agent]; ok {
			return agent.Adapter
		}
	}
	return n.step.Adapter
}
