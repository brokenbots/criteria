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
			cmd.SilenceUsage = true
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

// renderDOT renders the FSMGraph as a Graphviz DOT digraph string.
// Subworkflow-targeted steps with compiled bodies are rendered as
// subgraph cluster_ blocks with namespaced node IDs; parent edges are
// rewired to the cluster boundaries. The rendering is recursive for nested
// subworkflows.
func renderDOT(graph *workflow.FSMGraph) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("digraph %q {\n", graph.Name))
	b.WriteString("  rankdir=LR;\n")
	b.WriteString("\n")
	dotWriteNodes(&b, graph, "  ", "")
	b.WriteString("\n")
	dotWriteEdges(&b, graph, "  ", "")
	b.WriteString("}\n")
	return b.String()
}

// dotWriteNodes writes node declarations for graph at the given indent and
// namespace. Non-subworkflow steps, switches, and states are declared as
// regular nodes. Subworkflow steps with compiled bodies become subgraph
// cluster_ blocks whose interior is written by dotWriteClusterBody.
func dotWriteNodes(b *strings.Builder, graph *workflow.FSMGraph, indent, namespace string) {
	dotWriteNodeDecls(b, graph, indent, namespace)
	for _, name := range graph.StepOrder() {
		st := graph.Steps[name]
		if st.SubworkflowRef == "" {
			continue
		}
		swNode := graph.Subworkflows[st.SubworkflowRef]
		if swNode == nil || swNode.Body == nil {
			// No compiled body: fall back to a shape=component placeholder.
			attrs := dotStepAttrs(name, st)
			fmt.Fprintf(b, "%s%q [%s];\n", indent, namespace+name, attrs)
			continue
		}
		clusterNS := namespace + name + "/"
		clusterID := sanitizeDotID(namespace + name)
		fmt.Fprintf(b, "%ssubgraph cluster_%s {\n", indent, clusterID)
		fmt.Fprintf(b, "%s  label=%q;\n", indent, dotClusterLabel(st))
		fmt.Fprintf(b, "%s  style=dashed;\n", indent)
		dotWriteClusterBody(b, swNode.Body, indent+"  ", clusterNS)
		fmt.Fprintf(b, "%s}\n", indent)
	}
}

// dotWriteNodeDecls writes flat node declarations for adapter steps, switches,
// and states. Subworkflow steps are skipped (they are rendered as clusters).
func dotWriteNodeDecls(b *strings.Builder, graph *workflow.FSMGraph, indent, namespace string) {
	for _, name := range graph.StepOrder() {
		st := graph.Steps[name]
		if st.SubworkflowRef != "" {
			continue // rendered as a cluster block by the caller
		}
		attrs := dotStepAttrs(name, st)
		fmt.Fprintf(b, "%s%q [%s];\n", indent, namespace+name, attrs)
	}
	for _, name := range sortedSwitchNames(graph) {
		fmt.Fprintf(b, "%s%q [shape=diamond];\n", indent, namespace+name)
	}
	for _, name := range sortedStateNames(graph) {
		shape := "ellipse"
		if graph.States[name].Terminal {
			shape = "doublecircle"
		}
		fmt.Fprintf(b, "%s%q [shape=%s];\n", indent, namespace+name, shape)
	}
}

// dotWriteClusterBody writes node declarations, nested clusters, and internal
// edges for a subworkflow cluster. Exit edges (from the cluster's terminal
// states to the parent's outcome targets) are NOT written here; the caller
// emits them via dotWriteExitEdges after the cluster block closes.
func dotWriteClusterBody(b *strings.Builder, graph *workflow.FSMGraph, indent, namespace string) {
	dotWriteNodeDecls(b, graph, indent, namespace)
	// Nested cluster subgraphs (second pass over steps).
	for _, name := range graph.StepOrder() {
		st := graph.Steps[name]
		if st.SubworkflowRef == "" {
			continue
		}
		swNode := graph.Subworkflows[st.SubworkflowRef]
		if swNode == nil || swNode.Body == nil {
			attrs := dotStepAttrs(name, st)
			fmt.Fprintf(b, "%s%q [%s];\n", indent, namespace+name, attrs)
			continue
		}
		nestedNS := namespace + name + "/"
		clusterID := sanitizeDotID(namespace + name)
		fmt.Fprintf(b, "%ssubgraph cluster_%s {\n", indent, clusterID)
		fmt.Fprintf(b, "%s  label=%q;\n", indent, dotClusterLabel(st))
		fmt.Fprintf(b, "%s  style=dashed;\n", indent)
		dotWriteClusterBody(b, swNode.Body, indent+"  ", nestedNS)
		fmt.Fprintf(b, "%s}\n", indent)
	}
	// __start__ node and initial edge.
	initialTarget := dotResolveRef(graph, namespace, graph.InitialState)
	fmt.Fprintf(b, "%s%q [shape=point,width=0.12,label=\"\"];\n", indent, namespace+"__start__")
	fmt.Fprintf(b, "%s%q -> %q [label=%q];\n", indent, namespace+"__start__", initialTarget, "initial")
	dotWriteStepEdges(b, graph, indent, namespace)
	dotWriteSwitchEdges(b, graph, indent, namespace)
}

// dotWriteEdges writes the top-level edge declarations for the root digraph:
// the __start__ node, the initial state edge (rewired if the initial state is
// a subworkflow step), step outcome edges, exit edges from subworkflow clusters,
// and switch edges.
func dotWriteEdges(b *strings.Builder, graph *workflow.FSMGraph, indent, namespace string) {
	initialTarget := dotResolveRef(graph, namespace, graph.InitialState)
	fmt.Fprintf(b, "%s%q [shape=point,width=0.12,label=\"\"];\n", indent, namespace+"__start__")
	fmt.Fprintf(b, "%s%q -> %q [label=%q];\n", indent, namespace+"__start__", initialTarget, "initial")
	dotWriteStepEdges(b, graph, indent, namespace)
	dotWriteSwitchEdges(b, graph, indent, namespace)
}

// dotWriteStepEdges writes outcome edges for adapter steps and, for subworkflow
// steps with compiled bodies, the exit edges from the cluster's terminal states.
func dotWriteStepEdges(b *strings.Builder, graph *workflow.FSMGraph, indent, namespace string) {
	for _, stepName := range graph.StepOrder() {
		step := graph.Steps[stepName]
		if step.SubworkflowRef != "" {
			swNode := graph.Subworkflows[step.SubworkflowRef]
			if swNode != nil && swNode.Body != nil {
				clusterNS := namespace + stepName + "/"
				dotWriteExitEdges(b, indent, graph, namespace, step, swNode.Body, clusterNS)
			}
			continue
		}
		for _, outcomeName := range sortedMapKeys(step.Outcomes) {
			co := step.Outcomes[outcomeName]
			if co.Next == "_continue" || co.Next == workflow.ReturnSentinel {
				continue
			}
			nextRef := dotResolveRef(graph, namespace, co.Next)
			fmt.Fprintf(b, "%s%q -> %q [label=%q];\n", indent, namespace+stepName, nextRef, outcomeName)
		}
	}
}

// dotWriteSwitchEdges writes all outgoing edges from switch nodes in graph.
func dotWriteSwitchEdges(b *strings.Builder, graph *workflow.FSMGraph, indent, namespace string) {
	for _, switchName := range sortedSwitchNames(graph) {
		sw := graph.Switches[switchName]
		for i, cond := range sw.Conditions {
			label := fmt.Sprintf("condition[%d]", i)
			if cond.Next != workflow.ReturnSentinel {
				nextRef := dotResolveRef(graph, namespace, cond.Next)
				fmt.Fprintf(b, "%s%q -> %q [label=%q];\n", indent, namespace+switchName, nextRef, label)
			}
		}
		if sw.DefaultNext != workflow.ReturnSentinel {
			nextRef := dotResolveRef(graph, namespace, sw.DefaultNext)
			fmt.Fprintf(b, "%s%q -> %q [label=%q];\n", indent, namespace+switchName, nextRef, "default")
		}
	}
}

// dotWriteExitEdges emits edges from each terminal state in clusterBody to
// each of the parent step's outcome targets. These edges connect the cluster
// boundary back to nodes in the parent graph.
func dotWriteExitEdges(b *strings.Builder, indent string, parentGraph *workflow.FSMGraph, parentNS string, step *workflow.StepNode, clusterBody *workflow.FSMGraph, clusterNS string) {
	for _, termName := range sortedStateNames(clusterBody) {
		termState := clusterBody.States[termName]
		if !termState.Terminal {
			continue
		}
		for _, outcomeName := range sortedMapKeys(step.Outcomes) {
			co := step.Outcomes[outcomeName]
			if co.Next == "_continue" || co.Next == workflow.ReturnSentinel {
				continue
			}
			nextRef := dotResolveRef(parentGraph, parentNS, co.Next)
			fmt.Fprintf(b, "%s%q -> %q [label=%q];\n", indent, clusterNS+termName, nextRef, outcomeName)
		}
	}
}

// dotResolveRef returns the DOT node ID for a reference within the current
// graph level. If name is a subworkflow step with a compiled body, the
// cluster's __start__ node is returned so that edges are routed into the
// cluster boundary rather than a now-absent placeholder node.
func dotResolveRef(graph *workflow.FSMGraph, namespace, name string) string {
	if st, ok := graph.Steps[name]; ok && st.SubworkflowRef != "" {
		if swNode, ok := graph.Subworkflows[st.SubworkflowRef]; ok && swNode != nil && swNode.Body != nil {
			return namespace + name + "/__start__"
		}
	}
	return namespace + name
}

// sanitizeDotID replaces characters that are not valid in an unquoted DOT
// identifier with underscores, for use in subgraph cluster names.
func sanitizeDotID(s string) string {
	var buf strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			buf.WriteRune(r)
		} else {
			buf.WriteRune('_')
		}
	}
	return buf.String()
}

// dotClusterLabel returns the Graphviz label string for a subworkflow cluster.
// The label is the subworkflow reference name plus any iteration annotation
// (e.g. "\n[for_each]") when the step iterates.
func dotClusterLabel(st *workflow.StepNode) string {
	label := st.SubworkflowRef
	if st.ForEach != nil {
		label += "\n[for_each]"
	} else if st.Count != nil {
		label += "\n[count]"
	} else if st.Parallel != nil {
		label += "\n[parallel]"
	}
	return label
}

// dotStepAttrs returns the Graphviz attribute string for a step node.
// Plain adapter steps get "shape=box". Iterating or subworkflow steps gain
// a label annotation (e.g. "step\n[for_each]") and subworkflow steps use
// shape=component (fallback when no compiled body is available).
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
		return nil, nil, fmt.Errorf("parse errors in %s:\n%w", workflowPath, newDiagsError(diags))
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
		return nil, nil, fmt.Errorf("compile errors in %s:\n%w", workflowPath, newDiagsError(diags))
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
