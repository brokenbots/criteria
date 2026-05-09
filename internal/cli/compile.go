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
		outPath          string
		format           string
		subworkflowRoots []string
	)

	cmd := &cobra.Command{
		Use:   "compile <workflow.hcl|dir>",
		Short: "Parse and compile a workflow to JSON or DOT",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			workflowPath := args[0]
			output, err := compileWorkflowOutput(cmd.Context(), workflowPath, format, subworkflowRoots)
			if err != nil {
				return err
			}
			return writeOutput(outPath, os.Stdout, output)
		},
	}

	cmd.Flags().StringVar(&outPath, "out", "", "Write output to file (default stdout)")
	cmd.Flags().StringVar(&format, "format", "json", "Output format: json or dot")
	cmd.Flags().StringArrayVar(&subworkflowRoots, "subworkflow-root", nil, "Restrict subworkflow source resolution to this root path (repeatable; empty = no restriction)")
	return cmd
}

func compileWorkflowOutput(ctx context.Context, workflowPath, format string, subworkflowRoots []string) ([]byte, error) {
	spec, graph, err := parseCompileForCli(ctx, workflowPath, subworkflowRoots)
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
	Name         string               `json:"name"`
	InitialState string               `json:"initial_state"`
	TargetState  string               `json:"target_state"`
	Policy       workflow.Policy      `json:"policy"`
	Adapters     []compileAdapter     `json:"adapters"`
	Steps        []compileStep        `json:"steps"`
	States       []compileState       `json:"states"`
	Outputs      []compileOutput      `json:"outputs"`
	Switches     []compileSwitch      `json:"switches"`
	Subworkflows []compileSubworkflow `json:"subworkflows,omitempty"`
	StepOrder    []string             `json:"step_order"`
	Plugins      []string             `json:"plugins_required"`
	Metadata     compileOutputMeta    `json:"metadata"`
}

type compileOutputMeta struct {
	SchemaVersion int `json:"schema_version"`
}

type compileAdapter struct {
	Type       string   `json:"type"`
	Name       string   `json:"name"`
	OnCrash    string   `json:"on_crash"`
	ConfigKeys []string `json:"config_keys"`
}

type compileStep struct {
	Name        string           `json:"name"`
	Adapter     string           `json:"adapter,omitempty"`
	Subworkflow string           `json:"subworkflow,omitempty"`
	Timeout     string           `json:"timeout,omitempty"`
	InputKeys   []string         `json:"input_keys"`
	AllowTools  []string         `json:"allow_tools"`
	Outcomes    []compileOutcome `json:"outcomes"`
}

type compileSubworkflow struct {
	Name       string      `json:"name"`
	SourcePath string      `json:"source_path"`
	Body       compileJSON `json:"body"`
}

type compileOutcome struct {
	Name string `json:"name"`
	Next string `json:"next"`
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

type compileSwitch struct {
	Name        string             `json:"name"`
	Conditions  []compileSwitchArm `json:"conditions"`
	DefaultNext string             `json:"default_next,omitempty"`
}

type compileSwitchArm struct {
	Match string `json:"match"`
	Next  string `json:"next"`
}

func buildCompileJSON(graph *workflow.FSMGraph) compileJSON { //nolint:funlen // W03: serialises entire FSM graph structure; length driven by field count, not complexity
	adapters := make([]compileAdapter, 0, len(graph.Adapters))
	adapterNames := sortedAdapterNames(graph)
	for _, name := range adapterNames {
		ad := graph.Adapters[name]
		adapters = append(adapters, compileAdapter{
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
			outcomes = append(outcomes, compileOutcome{Name: outcomeName, Next: st.Outcomes[outcomeName].Next})
		}
		var timeout string
		if st.Timeout > 0 {
			timeout = st.Timeout.String()
		}
		inputKeySet := make(map[string]struct{}, len(st.Input)+len(st.InputExprs))
		for k := range st.Input {
			inputKeySet[k] = struct{}{}
		}
		for k := range st.InputExprs {
			inputKeySet[k] = struct{}{}
		}
		steps = append(steps, compileStep{
			Name:        st.Name,
			Adapter:     st.AdapterRef,
			Subworkflow: st.SubworkflowRef,
			Timeout:     timeout,
			InputKeys:   sortedMapKeys(inputKeySet),
			AllowTools:  append([]string(nil), st.AllowTools...),
			Outcomes:    outcomes,
		})
	}

	stateNames := sortedStateNames(graph)
	states := make([]compileState, 0, len(stateNames))
	for _, name := range stateNames {
		st := graph.States[name]
		states = append(states, compileState{Name: st.Name, Terminal: st.Terminal, Success: st.Success})
	}

	switches := make([]compileSwitch, 0, len(graph.Switches))
	for _, name := range sortedMapKeys(graph.Switches) {
		sw := graph.Switches[name]
		arms := make([]compileSwitchArm, 0, len(sw.Conditions))
		for _, c := range sw.Conditions {
			arms = append(arms, compileSwitchArm{Match: c.MatchSrc, Next: c.Next})
		}
		switches = append(switches, compileSwitch{Name: sw.Name, Conditions: arms, DefaultNext: sw.DefaultNext})
	}

	outputs := buildCompileOutputs(graph)

	subworkflows := make([]compileSubworkflow, 0, len(graph.SubworkflowOrder))
	for _, swName := range graph.SubworkflowOrder {
		sw := graph.Subworkflows[swName]
		subworkflows = append(subworkflows, compileSubworkflow{
			Name:       sw.Name,
			SourcePath: sw.SourcePath,
			Body:       buildCompileJSON(sw.Body),
		})
	}

	return compileJSON{
		Name:         graph.Name,
		InitialState: graph.InitialState,
		TargetState:  graph.TargetState,
		Policy:       graph.Policy,
		Adapters:     adapters,
		Steps:        steps,
		States:       states,
		Outputs:      outputs,
		Switches:     switches,
		Subworkflows: subworkflows,
		StepOrder:    graph.StepOrder(),
		Plugins:      requiredPlugins(graph),
		Metadata:     compileOutputMeta{SchemaVersion: 1},
	}
}

func buildCompileOutputs(graph *workflow.FSMGraph) []compileOutput {
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
	return outputs
}

func renderDOT(graph *workflow.FSMGraph) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("digraph %q {\n", graph.Name))
	b.WriteString("  rankdir=LR;\n")
	b.WriteString("\n")

	for _, name := range graph.StepOrder() {
		st := graph.Steps[name]
		attrs := dotStepAttrs(name, st)
		b.WriteString(fmt.Sprintf("  %q [%s];\n", name, attrs))
	}
	for _, name := range sortedSwitchNames(graph) {
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
			co := step.Outcomes[outcomeName]
			if co.Next == "_continue" || co.Next == workflow.ReturnSentinel {
				// _continue and "return" are engine-internal; suppress from DOT output.
				continue
			}
			b.WriteString(fmt.Sprintf("  %q -> %q [label=%q];\n", step.Name, co.Next, outcomeName))
		}
	}
	for _, switchName := range sortedSwitchNames(graph) {
		sw := graph.Switches[switchName]
		for i, cond := range sw.Conditions {
			label := fmt.Sprintf("condition[%d]", i)
			if cond.Next != workflow.ReturnSentinel {
				b.WriteString(fmt.Sprintf("  %q -> %q [label=%q];\n", switchName, cond.Next, label))
			}
		}
		if sw.DefaultNext != workflow.ReturnSentinel {
			b.WriteString(fmt.Sprintf("  %q -> %q [label=%q];\n", switchName, sw.DefaultNext, "default"))
		}
	}
	b.WriteString("}\n")
	return b.String()
}

// dotStepAttrs returns the Graphviz attribute string for a step node.
// Plain adapter steps get "shape=box". Iterating or subworkflow steps gain
// a label annotation (e.g. "step\n[for_each]") and subworkflow steps use
// shape=component.
func dotStepAttrs(name string, st *workflow.StepNode) string {
	var annotations []string
	if st.ForEach != nil {
		annotations = append(annotations, "[for_each]")
	} else if st.Count != nil {
		annotations = append(annotations, "[count]")
	} else if st.Parallel != nil {
		annotations = append(annotations, "[parallel]")
	}
	if st.SubworkflowRef != "" {
		annotations = append(annotations, fmt.Sprintf("[→ %s]", st.SubworkflowRef))
	}

	shape := "shape=box"
	if st.SubworkflowRef != "" {
		shape = "shape=component"
	}
	if len(annotations) == 0 {
		return shape
	}
	label := name + "\n" + strings.Join(annotations, "\n")
	return fmt.Sprintf("%s, label=%q", shape, label)
}

func parseCompileForCli(ctx context.Context, workflowPath string, subworkflowRoots []string) (*workflow.Spec, *workflow.FSMGraph, error) {
	spec, diags := workflow.ParseFileOrDir(workflowPath)
	if diags.HasErrors() {
		return nil, nil, fmt.Errorf("parse: %s", diags.Error())
	}

	loader := plugin.NewLoader()
	loader.RegisterBuiltin(shell.Name, plugin.BuiltinFactoryForAdapter(shell.New()))
	schemas := collectSchemas(ctx, loader, spec, nil)
	defer func() { _ = loader.Shutdown(ctx) }()

	workflowDir := workflowPath
	if info, err := os.Stat(workflowPath); err == nil && !info.IsDir() {
		workflowDir = filepath.Dir(workflowPath)
	}

	graph, diags := workflow.CompileWithOpts(spec, schemas, workflow.CompileOpts{
		WorkflowDir:         workflowDir,
		SubWorkflowResolver: &workflow.LocalSubWorkflowResolver{AllowedRoots: subworkflowRoots},
		Schemas:             schemas,
	})
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

func sortedSwitchNames(graph *workflow.FSMGraph) []string {
	return sortedMapKeys(graph.Switches)
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
		if st.AdapterRef != "" {
			parts := strings.Split(st.AdapterRef, ".")
			if len(parts) == 2 {
				seen[parts[0]] = true
			}
		}
	}
	return sortedMapKeys(seen)
}
