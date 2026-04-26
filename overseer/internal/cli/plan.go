package cli

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/brokenbots/overlord/workflow"
)

func NewPlanCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "plan <workflow.hcl>",
		Short: "Render a human-readable execution preview",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out, err := renderPlanOutput(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			_, err = os.Stdout.WriteString(out)
			return err
		},
	}
}

func renderPlanOutput(ctx context.Context, workflowPath string) (string, error) {
	spec, graph, err := parseCompileForCli(ctx, workflowPath)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("workflow: %s  (version %s)\n", graph.Name, spec.Version))
	b.WriteString(fmt.Sprintf("initial_state: %s   target_state: %s\n", graph.InitialState, graph.TargetState))
	b.WriteString(fmt.Sprintf("policy: max_total_steps=%d  max_step_retries=%d\n", graph.Policy.MaxTotalSteps, graph.Policy.MaxStepRetries))
	b.WriteString("\n")

	b.WriteString("variables:\n")
	b.WriteString("  (none)\n\n")

	b.WriteString("agents:\n")
	agentNames := sortedAgentNames(graph)
	if len(agentNames) == 0 {
		b.WriteString("  (none)\n")
	}
	for _, name := range agentNames {
		ag := graph.Agents[name]
		b.WriteString(fmt.Sprintf("  %s   adapter=%s   on_crash=%s\n", ag.Name, ag.Adapter, ag.OnCrash))
		cfg := sortedMapKeys(ag.Config)
		if len(cfg) == 0 {
			b.WriteString("    config: (none)\n")
		} else {
			pairs := make([]string, 0, len(cfg))
			for _, k := range cfg {
				pairs = append(pairs, fmt.Sprintf("%s=%s", k, ag.Config[k]))
			}
			b.WriteString(fmt.Sprintf("    config: %s\n", strings.Join(pairs, ", ")))
		}
	}
	b.WriteString("\n")

	b.WriteString("steps (declaration order):\n")
	for _, stepName := range graph.StepOrder() {
		step := graph.Steps[stepName]
		b.WriteString("  " + formatStepHeader(step) + "\n")

		keys := sortedMapKeys(step.Input)
		if len(keys) == 0 {
			b.WriteString("    input keys: (none)\n")
		} else {
			b.WriteString(fmt.Sprintf("    input keys: %s\n", strings.Join(keys, ", ")))
		}

		if len(step.AllowTools) == 0 {
			b.WriteString("    allow_tools: (none)\n")
		} else {
			b.WriteString(fmt.Sprintf("    allow_tools: %s\n", strings.Join(step.AllowTools, ", ")))
		}

		b.WriteString("    outcomes: ")
		b.WriteString(formatOutcomes(step, spec))
		b.WriteString("\n")
	}
	b.WriteString("\n")

	b.WriteString("states:\n")
	for _, name := range sortedStateNames(graph) {
		state := graph.States[name]
		requires := ""
		if strings.TrimSpace(state.Requires) != "" {
			requires = fmt.Sprintf("   requires=%s", state.Requires)
		}
		b.WriteString(fmt.Sprintf("  %s    terminal=%t   success=%t%s\n", state.Name, state.Terminal, state.Success, requires))
	}
	b.WriteString("\n")

	b.WriteString("plugins required:\n")
	plugs := requiredPlugins(graph)
	if len(plugs) == 0 {
		b.WriteString("  (none)\n")
	} else {
		for _, p := range plugs {
			b.WriteString(fmt.Sprintf("  %s   (search: $OVERLORD_PLUGINS, ~/.overlord/plugins)\n", p))
		}
	}

	return b.String(), nil
}

func formatStepHeader(step *workflow.StepNode) string {
	parts := []string{step.Name}
	if step.Agent != "" {
		parts = append(parts, "agent="+step.Agent)
	}
	if step.Adapter != "" {
		parts = append(parts, "adapter="+step.Adapter)
	}
	if step.Lifecycle != "" {
		parts = append(parts, "lifecycle="+step.Lifecycle)
	}
	if step.Timeout > 0 {
		parts = append(parts, "timeout="+step.Timeout.String())
	}
	return strings.Join(parts, "   ")
}

func formatOutcomes(step *workflow.StepNode, spec *workflow.Spec) string {
	ordered := make([]string, 0, len(step.Outcomes))
	if spec != nil {
		for _, st := range spec.Steps {
			if st.Name != step.Name {
				continue
			}
			for _, o := range st.Outcomes {
				if dst, ok := step.Outcomes[o.Name]; ok {
					ordered = append(ordered, fmt.Sprintf("%s -> %s", o.Name, dst))
				}
			}
			break
		}
	}

	if len(ordered) == 0 {
		names := sortedMapKeys(step.Outcomes)
		for _, name := range names {
			ordered = append(ordered, fmt.Sprintf("%s -> %s", name, step.Outcomes[name]))
		}
	} else {
		missing := make([]string, 0, len(step.Outcomes))
		seen := map[string]bool{}
		for _, line := range ordered {
			parts := strings.SplitN(line, " -> ", 2)
			if len(parts) == 2 {
				seen[parts[0]] = true
			}
		}
		for name := range step.Outcomes {
			if !seen[name] {
				missing = append(missing, name)
			}
		}
		sort.Strings(missing)
		for _, name := range missing {
			ordered = append(ordered, fmt.Sprintf("%s -> %s", name, step.Outcomes[name]))
		}
	}
	return strings.Join(ordered, ", ")
}