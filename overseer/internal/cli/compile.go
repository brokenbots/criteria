package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/brokenbots/overlord/overseer/internal/adapters/shell"
	"github.com/brokenbots/overlord/overseer/internal/plugin"
	"github.com/brokenbots/overlord/workflow"
)

func NewCompileCmd() *cobra.Command {
	var (
		outPath string
		format  string
	)

	cmd := &cobra.Command{
		Use:   "compile <workflow.hcl>",
		Short: "Parse and compile a workflow to JSON or DOT",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			workflowPath := args[0]
			output, err := compileWorkflowOutput(cmd.Context(), workflowPath, format)
			if err != nil {
				return err
			}
			return writeOutput(outPath, os.Stdout, output)
		},
	}

	cmd.Flags().StringVar(&outPath, "out", "", "Write output to file (default stdout)")
	cmd.Flags().StringVar(&format, "format", "json", "Output format: json or dot")
	return cmd
}

func compileWorkflowOutput(ctx context.Context, workflowPath, format string) ([]byte, error) {
	spec, graph, err := parseCompileForCli(ctx, workflowPath)
	if err != nil {
		return nil, err
	}
	_ = spec

	switch strings.ToLower(strings.TrimSpace(format)) {
	case "", "json":
		v := buildCompileJSON(graph)
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return nil, err
		}
		return append(b, '\n'), nil
	case "dot":
		return []byte(renderDOT(graph)), nil
	default:
		return nil, fmt.Errorf("unsupported format %q (want json or dot)", format)
	}
}

type compileJSON struct {
	Name         string            `json:"name"`
	InitialState string            `json:"initial_state"`
	TargetState  string            `json:"target_state"`
	Policy       workflow.Policy   `json:"policy"`
	Agents       []compileAgent    `json:"agents"`
	Steps        []compileStep     `json:"steps"`
	States       []compileState    `json:"states"`
	StepOrder    []string          `json:"step_order"`
	Plugins      []string          `json:"plugins_required"`
	Metadata     compileOutputMeta `json:"metadata"`
}

type compileOutputMeta struct {
	SchemaVersion int `json:"schema_version"`
}

type compileAgent struct {
	Name       string   `json:"name"`
	Adapter    string   `json:"adapter"`
	OnCrash    string   `json:"on_crash"`
	ConfigKeys []string `json:"config_keys"`
}

type compileStep struct {
	Name       string           `json:"name"`
	Adapter    string           `json:"adapter,omitempty"`
	Agent      string           `json:"agent,omitempty"`
	Lifecycle  string           `json:"lifecycle,omitempty"`
	Timeout    string           `json:"timeout,omitempty"`
	InputKeys  []string         `json:"input_keys"`
	AllowTools []string         `json:"allow_tools"`
	Outcomes   []compileOutcome `json:"outcomes"`
}

type compileOutcome struct {
	Name         string `json:"name"`
	TransitionTo string `json:"transition_to"`
}

type compileState struct {
	Name     string `json:"name"`
	Terminal bool   `json:"terminal"`
	Success  bool   `json:"success"`
}

func buildCompileJSON(graph *workflow.FSMGraph) compileJSON {
	agents := make([]compileAgent, 0, len(graph.Agents))
	agentNames := sortedAgentNames(graph)
	for _, name := range agentNames {
		ag := graph.Agents[name]
		agents = append(agents, compileAgent{
			Name:       ag.Name,
			Adapter:    ag.Adapter,
			OnCrash:    ag.OnCrash,
			ConfigKeys: sortedMapKeys(ag.Config),
		})
	}

	steps := make([]compileStep, 0, len(graph.StepOrder()))
	for _, name := range graph.StepOrder() {
		st := graph.Steps[name]
		outcomes := make([]compileOutcome, 0, len(st.Outcomes))
		for _, outcomeName := range sortedMapKeys(st.Outcomes) {
			outcomes = append(outcomes, compileOutcome{Name: outcomeName, TransitionTo: st.Outcomes[outcomeName]})
		}
		var timeout string
		if st.Timeout > 0 {
			timeout = st.Timeout.String()
		}
		steps = append(steps, compileStep{
			Name:       st.Name,
			Adapter:    st.Adapter,
			Agent:      st.Agent,
			Lifecycle:  st.Lifecycle,
			Timeout:    timeout,
			InputKeys:  sortedMapKeys(st.Input),
			AllowTools: append([]string(nil), st.AllowTools...),
			Outcomes:   outcomes,
		})
	}

	stateNames := sortedStateNames(graph)
	states := make([]compileState, 0, len(stateNames))
	for _, name := range stateNames {
		st := graph.States[name]
		states = append(states, compileState{Name: st.Name, Terminal: st.Terminal, Success: st.Success})
	}

	return compileJSON{
		Name:         graph.Name,
		InitialState: graph.InitialState,
		TargetState:  graph.TargetState,
		Policy:       graph.Policy,
		Agents:       agents,
		Steps:        steps,
		States:       states,
		StepOrder:    graph.StepOrder(),
		Plugins:      requiredPlugins(graph),
		Metadata:     compileOutputMeta{SchemaVersion: 1},
	}
}

func renderDOT(graph *workflow.FSMGraph) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("digraph %q {\n", graph.Name))
	b.WriteString("  rankdir=LR;\n")
	b.WriteString("\n")

	for _, name := range graph.StepOrder() {
		b.WriteString(fmt.Sprintf("  %q [shape=box];\n", name))
	}
	for _, name := range sortedStateNames(graph) {
		state := graph.States[name]
		shape := "ellipse"
		if state.Terminal {
			shape = "doublecircle"
		}
		b.WriteString(fmt.Sprintf("  %q [shape=%s];\n", name, shape))
	}
	b.WriteString("\n")
	b.WriteString(fmt.Sprintf("  %q [shape=point,width=0.12,label=\"\"];\n", "__start__"))
	b.WriteString(fmt.Sprintf("  %q -> %q [label=%q];\n", "__start__", graph.InitialState, "initial"))

	for _, stepName := range graph.StepOrder() {
		step := graph.Steps[stepName]
		for _, outcomeName := range sortedMapKeys(step.Outcomes) {
			target := step.Outcomes[outcomeName]
			b.WriteString(fmt.Sprintf("  %q -> %q [label=%q];\n", step.Name, target, outcomeName))
		}
	}
	b.WriteString("}\n")
	return b.String()
}

func parseCompileForCli(ctx context.Context, workflowPath string) (*workflow.Spec, *workflow.FSMGraph, error) {
	src, err := os.ReadFile(workflowPath)
	if err != nil {
		return nil, nil, err
	}

	spec, diags := workflow.Parse(workflowPath, src)
	if diags.HasErrors() {
		return nil, nil, fmt.Errorf("parse: %s", diags.Error())
	}

	loader := plugin.NewLoader()
	loader.RegisterBuiltin(shell.Name, plugin.BuiltinFactoryForAdapter(shell.New()))
	schemas := collectSchemas(ctx, loader, spec, nil)
	defer loader.Shutdown(ctx)

	graph, diags := workflow.Compile(spec, schemas)
	if diags.HasErrors() {
		return nil, nil, fmt.Errorf("compile: %s", diags.Error())
	}
	return spec, graph, nil
}

func writeOutput(outPath string, stdout io.Writer, payload []byte) error {
	if strings.TrimSpace(outPath) == "" {
		_, err := stdout.Write(payload)
		return err
	}
	return os.WriteFile(outPath, payload, 0o600)
}

func sortedMapKeys[V any](m map[string]V) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedAgentNames(graph *workflow.FSMGraph) []string {
	return sortedMapKeys(graph.Agents)
}

func sortedStateNames(graph *workflow.FSMGraph) []string {
	return sortedMapKeys(graph.States)
}

func requiredPlugins(graph *workflow.FSMGraph) []string {
	seen := map[string]bool{}
	for _, ag := range graph.Agents {
		if ag.Adapter != "" {
			seen[ag.Adapter] = true
		}
	}
	for _, st := range graph.Steps {
		if st.Adapter != "" {
			seen[st.Adapter] = true
		}
	}
	return sortedMapKeys(seen)
}
