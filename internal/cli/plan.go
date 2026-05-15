package cli

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/zclconf/go-cty/cty"

	"github.com/brokenbots/criteria/workflow"
)

func NewPlanCmd() *cobra.Command {
	var varOverrides []string
	cmd := &cobra.Command{
		Use:   "plan <workflow.hcl|dir>",
		Short: "Render a human-readable execution preview",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			out, err := renderPlanOutput(cmd.Context(), args[0], parseVarOverrides(varOverrides))
			if err != nil {
				return err
			}
			_, err = os.Stdout.WriteString(out)
			return err
		},
	}
	cmd.Flags().StringArrayVar(&varOverrides, "var", nil, "Override a workflow variable: key=value (repeatable)")
	return cmd
}

func renderPlanOutput(ctx context.Context, workflowPath string, overrides map[string]string) (string, error) { //nolint:funlen,gocognit,gocyclo // renders full plan tree with agent/step/outcome formatting across multiple output paths
	spec, graph, err := parseCompileForCli(ctx, workflowPath, nil)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("workflow: %s  (version %s)\n", graph.Name, spec.Header.Version))
	b.WriteString(fmt.Sprintf("initial_state: %s   target_state: %s\n", graph.InitialState, graph.TargetState))
	b.WriteString(fmt.Sprintf("policy: max_total_steps=%d  max_step_retries=%d\n", graph.Policy.MaxTotalSteps, graph.Policy.MaxStepRetries))
	b.WriteString("\n")

	b.WriteString("variables:\n")
	varNames := sortedVariableNames(graph)
	if len(varNames) == 0 {
		b.WriteString("  (none)\n")
	} else {
		for _, name := range varNames {
			v := graph.Variables[name]
			typeName := v.Type.FriendlyName()
			displayVal := "(required)"
			if ov, ok := overrides[name]; ok {
				displayVal = ov + "  (override)"
			} else if v.Default != cty.NilVal {
				displayVal = workflow.CtyValueToString(v.Default)
			}
			b.WriteString(fmt.Sprintf("  %s: %s = %s\n", name, typeName, displayVal))
		}
	}
	b.WriteString("\n")

	b.WriteString("adapters:\n")
	adapterNames := sortedAdapterNames(graph)
	if len(adapterNames) == 0 {
		b.WriteString("  (none)\n")
	}
	for _, name := range adapterNames {
		ad := graph.Adapters[name]
		b.WriteString(fmt.Sprintf("  %s.%s   type=%s   on_crash=%s\n", ad.Type, ad.Name, ad.Type, ad.OnCrash))
		cfg := sortedMapKeys(ad.Config)
		if len(cfg) == 0 {
			b.WriteString("    config: (none)\n")
		} else {
			pairs := make([]string, 0, len(cfg))
			for _, k := range cfg {
				pairs = append(pairs, fmt.Sprintf("%s=%s", k, ad.Config[k]))
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

	if len(graph.Switches) > 0 {
		b.WriteString("switches:\n")
		for _, name := range sortedSwitchNames(graph) {
			sw := graph.Switches[name]
			defaultNext := sw.DefaultNext
			if defaultNext == "" {
				defaultNext = "(none)"
			}
			b.WriteString(fmt.Sprintf("  %s    conditions=%d   default=%s\n", name, len(sw.Conditions), defaultNext))
		}
		b.WriteString("\n")
	}

	b.WriteString("adapters required:\n")
	adapts := requiredAdapters(graph)
	if len(adapts) == 0 {
		b.WriteString("  (none)\n")
	} else {
		for _, p := range adapts {
			b.WriteString(fmt.Sprintf("  %s   (search: $CRITERIA_PLUGINS, ~/.criteria/plugins)\n", p))
		}
	}

	return b.String(), nil
}

func formatStepHeader(step *workflow.StepNode) string {
	parts := []string{step.Name}
	if step.AdapterRef != "" {
		parts = append(parts, "adapter="+step.AdapterRef)
	}
	if step.Timeout > 0 {
		parts = append(parts, "timeout="+step.Timeout.String())
	}
	return strings.Join(parts, "   ")
}

func formatOutcomes(step *workflow.StepNode, spec *workflow.Spec) string {
	ordered := buildOrderedOutcomes(step, spec)
	if len(ordered) == 0 {
		names := sortedMapKeys(step.Outcomes)
		for _, name := range names {
			ordered = append(ordered, fmt.Sprintf("%s -> %s", name, step.Outcomes[name].Next))
		}
	} else {
		ordered = appendMissingOutcomes(step, ordered)
	}
	return strings.Join(ordered, ", ")
}

// buildOrderedOutcomes returns spec-ordered outcome strings for the given step,
// or nil if spec is nil or doesn't contain the step.
func buildOrderedOutcomes(step *workflow.StepNode, spec *workflow.Spec) []string {
	if spec == nil {
		return nil
	}
	for i := range spec.Steps {
		st := &spec.Steps[i]
		if st.Name != step.Name {
			continue
		}
		ordered := make([]string, 0, len(st.Outcomes))
		for _, o := range st.Outcomes {
			if co, ok := step.Outcomes[o.Name]; ok {
				ordered = append(ordered, fmt.Sprintf("%s -> %s", o.Name, co.Next))
			}
		}
		return ordered
	}
	return nil
}

// appendMissingOutcomes appends any step outcomes not already covered by the
// spec-ordered list (matched by name prefix before " -> ").
func appendMissingOutcomes(step *workflow.StepNode, ordered []string) []string {
	seen := make(map[string]bool, len(ordered))
	for _, line := range ordered {
		if parts := strings.SplitN(line, " -> ", 2); len(parts) == 2 {
			seen[parts[0]] = true
		}
	}
	missing := make([]string, 0, len(step.Outcomes))
	for name := range step.Outcomes {
		if !seen[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	for _, name := range missing {
		ordered = append(ordered, fmt.Sprintf("%s -> %s", name, step.Outcomes[name].Next))
	}
	return ordered
}
