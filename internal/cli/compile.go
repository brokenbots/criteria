package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/zclconf/go-cty/cty"

	"github.com/brokenbots/criteria/internal/adapters/shell"
	"github.com/brokenbots/criteria/internal/plugin"
	"github.com/brokenbots/criteria/workflow"
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
	Outputs      []compileOutput   `json:"outputs"`
	StepOrder    []string          `json:"step_order"`
	Plugins      []string          `json:"plugins_required"`
	Metadata     compileOutputMeta `json:"metadata"`
}

type compileOutputMeta struct {
	SchemaVersion int `json:"schema_version"`
}

type compileAgent struct {
	Type       string   `json:"type"`
	Name       string   `json:"name"`
	OnCrash    string   `json:"on_crash"`
	ConfigKeys []string `json:"config_keys"`
}

type compileStep struct {
	Name       string           `json:"name"`
	Adapter    string           `json:"adapter,omitempty"`
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

type compileOutput struct {
	Name        string `json:"name"`
	Type        string `json:"type,omitempty"`
	Description string `json:"description,omitempty"`
}

type compileState struct {
	Name     string `json:"name"`
	Terminal bool   `json:"terminal"`
	Success  bool   `json:"success"`
}

func buildCompileJSON(graph *workflow.FSMGraph) compileJSON { //nolint:funlen // W03: serialises entire FSM graph structure; length driven by field count, not complexity
	adapters := make([]compileAgent, 0, len(graph.Adapters))
	adapterNames := sortedAdapterNames(graph)
	for _, name := range adapterNames {
		ad := graph.Adapters[name]
		adapters = append(adapters, compileAgent{
			Type:       ad.Type,
			Name:       ad.Name,
			OnCrash:    ad.OnCrash,
			ConfigKeys: sortedMapKeys(ad.Config),
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

	// TODO(W10): add a Branches field to compileJSON for tooling completeness.
	outputs := make([]compileOutput, 0, len(graph.Outputs))
	for _, name := range graph.OutputOrder {
		on := graph.Outputs[name]
		typeStr := ""
		if on.DeclaredType != cty.NilType {
			// TypeToString only supports types accepted by parseVariableType.
			// This should never error at compile time since declared types come from HCL schema.
			if s, err := workflow.TypeToString(on.DeclaredType); err == nil {
				typeStr = s
			}
		}
		outputs = append(outputs, compileOutput{
			Name:        on.Name,
			Type:        typeStr,
			Description: on.Description,
		})
	}

	return compileJSON{
		Name:         graph.Name,
		InitialState: graph.InitialState,
		TargetState:  graph.TargetState,
		Policy:       graph.Policy,
		Agents:       adapters,
		Steps:        steps,
		States:       states,
		Outputs:      outputs,
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
	for _, name := range sortedBranchNames(graph) {
		b.WriteString(fmt.Sprintf("  %q [shape=diamond];\n", name))
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
			if target == "_continue" {
				// _continue is engine-internal and not a real graph node; suppress it
				// from the DOT output to avoid dangling edges.
				continue
			}
			b.WriteString(fmt.Sprintf("  %q -> %q [label=%q];\n", step.Name, target, outcomeName))
		}
	}
	for _, branchName := range sortedBranchNames(graph) {
		br := graph.Branches[branchName]
		for i, arm := range br.Arms {
			label := fmt.Sprintf("arm[%d]", i)
			b.WriteString(fmt.Sprintf("  %q -> %q [label=%q];\n", branchName, arm.Target, label))
		}
		b.WriteString(fmt.Sprintf("  %q -> %q [label=%q];\n", branchName, br.DefaultTarget, "default"))
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
	defer func() { _ = loader.Shutdown(ctx) }()

	graph, diags := workflow.CompileWithOpts(spec, schemas, workflow.CompileOpts{WorkflowDir: filepath.Dir(workflowPath)})
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

func sortedVariableNames(graph *workflow.FSMGraph) []string {
	return sortedMapKeys(graph.Variables)
}

func sortedAdapterNames(graph *workflow.FSMGraph) []string {
	return sortedMapKeys(graph.Adapters)
}

func sortedStateNames(graph *workflow.FSMGraph) []string {
	return sortedMapKeys(graph.States)
}

func sortedBranchNames(graph *workflow.FSMGraph) []string {
	return sortedMapKeys(graph.Branches)
}

func requiredPlugins(graph *workflow.FSMGraph) []string {
	seen := map[string]bool{}
	for _, ad := range graph.Adapters {
		if ad.Type != "" {
			seen[ad.Type] = true
		}
	}
	for _, st := range graph.Steps {
		// Steps reference adapters; extract the type from "<type>.<name>" reference.
		if st.Adapter != "" {
			parts := strings.Split(st.Adapter, ".")
			if len(parts) == 2 {
				seen[parts[0]] = true
			}
		}
	}
	return sortedMapKeys(seen)
}
