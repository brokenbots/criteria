package workflowtest

import "fmt"

// Chain creates a workflow with a sequential chain of steps:
//
//	stepNames[0] -> stepNames[1] -> ... -> targetState
//
// Each step handles both "success" (transitions to next step or targetState)
// and "failure" (transitions to failureState, default "failed").
func Chain(stepNames ...string) *WorkflowBuilder {
	if len(stepNames) == 0 {
		return NewStandard()
	}
	initial := stepNames[0]
	target := "done"
	failure := "failed"

	trueVal := true
	falseVal := false
	b := New().
		WithHeader("t", "0.1", initial, target).
		WithAdapterFate("noop", "default", "reuse").
		WithState(target, true, &trueVal).
		WithState(failure, true, &falseVal)

	for i, name := range stepNames {
		var next string
		if i == len(stepNames)-1 {
			next = fmt.Sprintf("state.%s", target)
		} else {
			next = fmt.Sprintf("step.%s", stepNames[i+1])
		}
		b.WithStep(name,
			Outcome("success", next),
			Outcome("failure", fmt.Sprintf("state.%s", failure)),
		)
	}
	return b
}

// LinearConfig configures linear workflow generation for benchmarks and large-scale tests.
type LinearConfig struct {
	Name         string
	AdapterType  string
	AdapterName  string
	Target       string
	RawInputFunc func(i int) string
	Budget       int
}

// LinearOpt configures LinearConfig.
type LinearOpt func(*LinearConfig)

// LinearName sets the workflow name.
func LinearName(name string) LinearOpt {
	return func(c *LinearConfig) {
		c.Name = name
	}
}

// LinearAdapter sets the adapter type and name.
func LinearAdapter(adapterType, adapterName string) LinearOpt {
	return func(c *LinearConfig) {
		c.AdapterType = adapterType
		c.AdapterName = adapterName
	}
}

// LinearTarget sets the step target expression directly.
func LinearTarget(target string) LinearOpt {
	return func(c *LinearConfig) {
		c.Target = target
	}
}

// LinearInput sets a generator for per-step raw input statements.
func LinearInput(fn func(i int) string) LinearOpt {
	return func(c *LinearConfig) {
		c.RawInputFunc = fn
	}
}

// LinearBudget sets the max_total_steps policy budget.
func LinearBudget(b int) LinearOpt {
	return func(c *LinearConfig) {
		c.Budget = b
	}
}

// Linear generates an n-step sequential linear workflow:
//
//	step_0 -> step_1 -> ... -> step_{n-1} -> done
//
// Each step handles both "success" (to step_{i+1} or done) and "failure" (to done).
func Linear(n int, opts ...LinearOpt) *WorkflowBuilder {
	cfg := LinearConfig{
		Name:        fmt.Sprintf("linear_%d_steps", n),
		AdapterType: "noop",
		AdapterName: "default",
		Budget:      n + 10,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.Target == "" {
		if cfg.AdapterName != "" {
			cfg.Target = fmt.Sprintf("adapter.%s.%s", cfg.AdapterType, cfg.AdapterName)
		} else if cfg.AdapterType != "" {
			cfg.Target = fmt.Sprintf("adapter.%s", cfg.AdapterType)
		}
	}

	trueVal := true
	b := New().
		WithHeader(cfg.Name, "0.1", "step_0", "done").
		WithMaxTotalSteps(cfg.Budget).
		WithState("done", true, &trueVal)

	if cfg.AdapterType != "" && cfg.AdapterName != "" {
		b.WithAdapter(cfg.AdapterType, cfg.AdapterName)
	}

	for i := 0; i < n; i++ {
		var next string
		if i == n-1 {
			next = "step.done"
		} else {
			next = fmt.Sprintf("step.step_%d", i+1)
		}
		stepName := fmt.Sprintf("step_%d", i)
		stepOpts := []StepOpt{
			Target(cfg.Target),
			Outcome("success", next),
			Outcome("failure", "step.done"),
		}
		if cfg.RawInputFunc != nil {
			stepOpts = append(stepOpts, StepRaw(cfg.RawInputFunc(i)))
		}
		b.WithStep(stepName, stepOpts...)
	}

	return b
}
