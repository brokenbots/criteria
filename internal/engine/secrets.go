package engine

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/zclconf/go-cty/cty"

	"github.com/brokenbots/criteria/internal/adapter/secrets"
	"github.com/brokenbots/criteria/workflow"
)

// resolveAdapterSecrets evaluates the adapter's secret expressions against the
// provided variable scope and resolves each value through the provider stack
// derived from the adapter's environment. Resolved values are registered with
// the redaction registry.
func resolveAdapterSecrets(
	ctx context.Context,
	g *workflow.FSMGraph,
	adapter *workflow.AdapterNode,
	vars map[string]cty.Value,
	reg *secrets.Registry,
) (map[string]string, error) {
	if len(adapter.Secrets) == 0 {
		return nil, nil
	}

	// Evaluate HCL expressions (e.g., var.api_key, env:FOO).
	evaluated, err := workflow.ResolveInputExprs(adapter.Secrets, vars)
	if err != nil {
		return nil, fmt.Errorf("adapter %q secrets evaluation: %w", adapter.Name, err)
	}

	// Build provider stack from the adapter's environment.
	envNode := getEnvironmentNode(g, adapter.Environment)
	stack, err := secrets.StackFromEnvironment(envNode)
	if err != nil {
		return nil, fmt.Errorf("adapter %q secrets stack: %w", adapter.Name, err)
	}

	resolved := make(map[string]string, len(evaluated))
	for name, val := range evaluated {
		if val == "" {
			slog.Debug("adapter secret resolved to empty string", "adapter", adapter.Name, "secret", name)
			continue
		}
		resolvedVal, resolveErr := secrets.ResolveString(ctx, val, stack)
		if resolveErr != nil {
			return nil, fmt.Errorf("adapter %q secret %q: %w", adapter.Name, name, resolveErr)
		}
		resolved[name] = resolvedVal
		if reg != nil {
			reg.Register(resolvedVal)
		}
	}
	return resolved, nil
}

// resolveStepSecretInputs evaluates the step's secret_input expressions and
// resolves each through the provider stack. Resolved values are registered
// with the redaction registry.
func resolveStepSecretInputs(
	ctx context.Context,
	g *workflow.FSMGraph,
	step *workflow.StepNode,
	vars map[string]cty.Value,
	reg *secrets.Registry,
) (map[string]string, error) {
	if len(step.SecretInputExprs) == 0 {
		return step.SecretInputs, nil
	}

	evaluated, err := workflow.ResolveInputExprs(step.SecretInputExprs, vars)
	if err != nil {
		return nil, fmt.Errorf("step %q secret_input evaluation: %w", step.Name, err)
	}

	// Merge with compiled SecretInputs (which contains empty placeholders for
	// dynamic expressions).
	merged := make(map[string]string, len(step.SecretInputs))
	for k, v := range step.SecretInputs {
		merged[k] = v
	}
	for k, v := range evaluated {
		merged[k] = v
	}

	envNode := getEnvironmentNode(g, step.Environment)
	stack, err := secrets.StackFromEnvironment(envNode)
	if err != nil {
		return nil, fmt.Errorf("step %q secrets stack: %w", step.Name, err)
	}

	resolved := make(map[string]string, len(merged))
	for name, val := range merged {
		if val == "" {
			slog.Debug("step secret_input resolved to empty string", "step", step.Name, "secret", name)
			continue
		}
		resolvedVal, resolveErr := secrets.ResolveString(ctx, val, stack)
		if resolveErr != nil {
			return nil, fmt.Errorf("step %q secret_input %q: %w", step.Name, name, resolveErr)
		}
		resolved[name] = resolvedVal
		if reg != nil {
			reg.Register(resolvedVal)
		}
	}
	return resolved, nil
}

func getEnvironmentNode(g *workflow.FSMGraph, envKey string) *workflow.EnvironmentNode {
	if envKey == "" {
		envKey = g.DefaultEnvironment
	}
	if envKey == "" {
		return nil
	}
	return g.Environments[envKey]
}
