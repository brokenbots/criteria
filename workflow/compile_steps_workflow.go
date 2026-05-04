package workflow

// compile_steps_workflow.go — compile path for type="workflow" steps (inline
// workflow body and workflow_file variants) plus workflow body helpers.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/zclconf/go-cty/cty"
)

// compileWorkflowStep compiles a type="workflow" step, including any optional
// for_each/count modifier. It populates the step's Body and BodyEntry fields
// in addition to the common step fields.
//
//nolint:funlen // W11: function length unavoidable due to comprehensive workflow and adapter validation
func compileWorkflowStep(g *FSMGraph, sp *StepSpec, spec *Spec, schemas map[string]AdapterInfo, opts CompileOpts) hcl.Diagnostics {
	var diags hcl.Diagnostics

	ok, d := validateStepRegistration(g, sp)
	diags = append(diags, d...)
	if !ok {
		return diags
	}

	diags = append(diags, validateStepKindSelectionDiags(sp)...)
	diags = append(diags, validateLegacyConfig(sp)...)
	diags = append(diags, validateAdapterAndAgent(g, sp)...)
	diags = append(diags, validateOnFailureValue(sp)...)

	// Workflow steps do not have adapters, so allow_tools and lifecycle are not valid
	diags = append(diags, validateWorkflowStepNoAdapterAttrs(sp)...)

	effectiveOnCrash, d := resolveStepOnCrash(g, sp)
	diags = append(diags, d...)

	timeout, d := decodeStepTimeout(sp)
	diags = append(diags, d...)

	if sp.MaxVisits < 0 {
		diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("step %q: max_visits must be >= 0", sp.Name)})
	}

	forEachExpr, countExpr, isIterating, d := compileWorkflowIterExpr(sp)
	diags = append(diags, d...)

	inputMap, inputExprs, d := decodeStepInput(g, sp, schemas, opts, "")
	diags = append(diags, d...)

	if !isIterating && opts.LoadDepth == 0 {
		diags = append(diags, validateEachRefs(sp.Name, inputExprs)...)
	}

	node := newWorkflowStepNode(sp, spec, effectiveOnCrash, timeout, inputMap, inputExprs, forEachExpr, countExpr)

	diags = append(diags, validateWorkflowStepOutcomes(sp, node, isIterating)...)

	d, node.Body, node.BodyEntry = compileWorkflowBody(sp, spec, schemas, opts)
	diags = append(diags, d...)

	// Decode and validate the optional `input = { ... }` attribute for body
	// variable binding. FoldExpr validates the expression at compile time.
	bodyInputExpr, d := decodeBodyInputAttr(sp, g, opts)
	diags = append(diags, d...)

	diags = append(diags, validateBodyInputBindings(node, bodyInputExpr, sp.Name)...)

	diags = append(diags, compileWorkflowOutputs(sp, node, opts)...)

	g.Steps[sp.Name] = node
	g.stepOrder = append(g.stepOrder, sp.Name)
	return diags
}

// validateWorkflowStepOutcomes compiles and validates outcome blocks for a
// workflow step. Iterating steps use validateIteratingOutcomes; non-iterating
// steps require at least one outcome.
func validateWorkflowStepOutcomes(sp *StepSpec, node *StepNode, isIterating bool) hcl.Diagnostics {
	diags := compileOutcomeBlock(sp, node)
	if isIterating {
		return append(diags, validateIteratingOutcomes(sp, node)...)
	}
	if len(node.Outcomes) == 0 {
		return append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("step %q: at least one outcome is required", sp.Name),
		})
	}
	return diags
}

// validateBodyInputBindings checks that every required body variable (no
// default) has an input binding and records the binding expression on the node.
// A missing binding is a compile-time error.
func validateBodyInputBindings(node *StepNode, bodyInputExpr hcl.Expression, stepName string) hcl.Diagnostics {
	if node.Body == nil {
		return nil
	}
	var missingVars []string
	for varName, varNode := range node.Body.Variables {
		if varNode.IsRequired() && bodyInputExpr == nil {
			missingVars = append(missingVars, varName)
		}
	}
	if len(missingVars) > 0 {
		sort.Strings(missingVars)
		return hcl.Diagnostics{{
			Severity: hcl.DiagError,
			Summary: fmt.Sprintf(
				"step %q: body variable(s) %s have no default; add input = { %s = ... } to the parent step",
				stepName, strings.Join(missingVars, ", "), missingVars[0],
			),
		}}
	}
	node.BodyInputExpr = bodyInputExpr
	return nil
}

// compileWorkflowBody dispatches to the inline or file-backed body compiler.
// Returns the compiled FSMGraph, the body entry state name, and any diagnostics.
func compileWorkflowBody(sp *StepSpec, spec *Spec, schemas map[string]AdapterInfo, opts CompileOpts) (hcl.Diagnostics, *FSMGraph, string) {
	const maxLoadDepth = 4
	var diags hcl.Diagnostics

	if opts.LoadDepth >= maxLoadDepth {
		return append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("step %q: maximum workflow nesting depth (%d) exceeded", sp.Name, maxLoadDepth),
		}), nil, ""
	}

	if sp.Workflow != nil && sp.WorkflowFile != "" {
		return append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("step %q: workflow { } block and workflow_file are mutually exclusive; remove one", sp.Name),
		}), nil, ""
	}

	if sp.Workflow == nil {
		return compileWorkflowBodyFromFile(sp, schemas, opts)
	}
	return compileWorkflowBodyInline(sp, spec, schemas, opts)
}

// compileWorkflowBodyFromFile handles the workflow_file = "..." loading path.
func compileWorkflowBodyFromFile(sp *StepSpec, schemas map[string]AdapterInfo, opts CompileOpts) (hcl.Diagnostics, *FSMGraph, string) {
	var diags hcl.Diagnostics
	if sp.WorkflowFile == "" {
		return append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("step %q: type=\"workflow\" requires a workflow { ... } block or workflow_file attribute", sp.Name),
		}), nil, ""
	}
	if opts.SubWorkflowResolver == nil {
		return append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("step %q: workflow_file requires SubWorkflowResolver in CompileOpts", sp.Name),
		}), nil, ""
	}
	for _, f := range opts.LoadedFiles {
		if f == sp.WorkflowFile {
			return append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("step %q: workflow_file %q creates a load cycle", sp.Name, sp.WorkflowFile),
			}), nil, ""
		}
	}
	loaded, err := opts.SubWorkflowResolver(sp.WorkflowFile, opts.WorkflowDir)
	if err != nil {
		return append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("step %q: failed to load workflow_file %q: %s", sp.Name, sp.WorkflowFile, err),
		}), nil, ""
	}
	childOpts := CompileOpts{
		WorkflowDir:         opts.WorkflowDir,
		LoadDepth:           opts.LoadDepth + 1,
		LoadedFiles:         append(append([]string{}, opts.LoadedFiles...), sp.WorkflowFile),
		SubWorkflowResolver: opts.SubWorkflowResolver,
	}
	body, d := CompileWithOpts(loaded, schemas, childOpts)
	diags = append(diags, d...)
	if diags.HasErrors() {
		return diags, nil, ""
	}
	return diags, body, loaded.InitialState
}

// compileWorkflowBodyInline handles the inline workflow { ... } block path.
// It decodes the body's Remain into SpecContent (shared content schema) and
// builds a synthetic *Spec for recursive compilation.
func compileWorkflowBodyInline(sp *StepSpec, spec *Spec, schemas map[string]AdapterInfo, opts CompileOpts) (hcl.Diagnostics, *FSMGraph, string) {
	var diags hcl.Diagnostics
	wb := sp.Workflow

	// Decode content blocks from the body's Remain into the shared SpecContent.
	var content SpecContent
	if d := gohcl.DecodeBody(wb.Remain, nil, &content); d.HasErrors() {
		return append(diags, d...), nil, ""
	}

	if len(content.Steps) == 0 {
		return append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("step %q: workflow body must contain at least one step", sp.Name),
		}), nil, ""
	}

	entry := resolveBodyEntry(wb, content.Steps)
	bodySpec := buildBodySpec(sp.Name, wb, spec, &content, entry)

	childOpts := CompileOpts{
		WorkflowDir:         opts.WorkflowDir,
		LoadDepth:           opts.LoadDepth + 1,
		LoadedFiles:         append([]string{}, opts.LoadedFiles...),
		SubWorkflowResolver: opts.SubWorkflowResolver,
	}
	body, d := CompileWithOpts(bodySpec, schemas, childOpts)
	diags = append(diags, d...)

	if err := validateBodyHasContinuePath(sp.Name, body); err != nil {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  err.Error(),
		})
	}

	return diags, body, entry
}

// resolveBodyEntry determines the entry state for a workflow body: explicit
// entry > initial_state > first step name.
func resolveBodyEntry(wb *BodySpec, steps []StepSpec) string {
	if wb.Entry != "" {
		return wb.Entry
	}
	if wb.InitialState != "" {
		return wb.InitialState
	}
	return steps[0].Name
}

// buildBodySpec constructs the synthetic *Spec used for recursive compilation
// of an inline workflow body. It appends the synthetic _continue terminal state
// and propagates SourceBytes from the parent spec.
// Name and Version default to "<step>:body" and "1" when not specified in wb.
func buildBodySpec(stepName string, wb *BodySpec, spec *Spec, content *SpecContent, entry string) *Spec {
	states := make([]StateSpec, len(content.States), len(content.States)+1)
	copy(states, content.States)
	states = append(states, StateSpec{Name: "_continue", Terminal: true})

	name := wb.Name
	if name == "" {
		name = stepName + ":body"
	}
	version := wb.Version
	if version == "" {
		version = "1"
	}

	return &Spec{
		Name:         name,
		Version:      version,
		InitialState: entry,
		TargetState:  "_continue",
		Variables:    content.Variables,
		Locals:       content.Locals,
		Environments: content.Environments,
		Adapters:     content.Adapters,
		Steps:        content.Steps,
		States:       states,
		Waits:        content.Waits,
		Approvals:    content.Approvals,
		Branches:     content.Branches,
		Policy:       content.Policy,
		Permissions:  content.Permissions,
		SourceBytes:  spec.SourceBytes, // propagate for BranchArm.ConditionSrc
	}
}

// decodeBodyInputAttr extracts and validates the optional `input = { ... }` map
// expression from the parent step's Remain body. Returns nil, nil when the
// attribute is absent. When present, the expression is validated via FoldExpr:
// unsupported variable namespaces produce a compile error, and a statically
// foldable result that is not a cty.Object is rejected. The expression is stored
// on StepNode.BodyInputExpr and evaluated at iteration entry to seed the child
// scope's var.* bindings.
func decodeBodyInputAttr(sp *StepSpec, g *FSMGraph, opts CompileOpts) (hcl.Expression, hcl.Diagnostics) {
	schema := &hcl.BodySchema{
		Attributes: []hcl.AttributeSchema{{Name: "input", Required: false}},
	}
	content, _, diags := sp.Remain.PartialContent(schema)
	if diags.HasErrors() || content == nil {
		return nil, diags
	}
	attr, ok := content.Attributes["input"]
	if !ok {
		return nil, nil
	}

	// Validate the expression via FoldExpr. Unsupported namespaces (not each,
	// steps, or shared_variable) produce a diagnostic error. A statically
	// foldable result must be a cty.Object.
	result, foldable, foldDiags := FoldExpr(attr.Expr, graphVars(g), graphLocals(g), opts.WorkflowDir)
	if foldDiags.HasErrors() {
		return nil, foldDiags
	}
	if foldable && result != cty.NilVal && !result.Type().IsObjectType() {
		return nil, hcl.Diagnostics{{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("step %q: body input = ... must evaluate to an object value; got %s", sp.Name, result.Type().FriendlyName()),
		}}
	}
	return attr.Expr, nil
}

// validateBodyHasContinuePath returns an error when none of the body steps
// has a transition_to = "_continue" outcome. This ensures that the iteration
// loop can terminate normally; a body with no path to _continue is a compile
// error (B-09).
func validateBodyHasContinuePath(stepName string, body *FSMGraph) error {
	if body == nil {
		return nil
	}
	for _, stepNode := range body.Steps {
		for _, target := range stepNode.Outcomes {
			if target == "_continue" {
				return nil
			}
		}
	}
	return fmt.Errorf("step %q: workflow body has no path to \"_continue\"; at least one outcome must transition_to = \"_continue\"", stepName)
}

// validateWorkflowStepNoAdapterAttrs validates that workflow steps don't have
// allow_tools or lifecycle attributes, as these require an adapter reference
// which workflow steps don't have.
func validateWorkflowStepNoAdapterAttrs(sp *StepSpec) hcl.Diagnostics {
	var diags hcl.Diagnostics
	if len(sp.AllowTools) > 0 {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("step %q: allow_tools requires an adapter reference", sp.Name),
		})
	}
	if sp.Lifecycle != "" {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("step %q: lifecycle requires an adapter reference", sp.Name),
		})
	}
	return diags
}
