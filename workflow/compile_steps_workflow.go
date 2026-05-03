package workflow

// compile_steps_workflow.go — compile path for type="workflow" steps (inline
// workflow body and workflow_file variants) plus workflow body helpers.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
)

// compileWorkflowStep compiles a type="workflow" step, including any optional
// for_each/count modifier. It populates the step's Body and BodyEntry fields
// in addition to the common step fields.
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

	diags = append(diags, compileOutcomeBlock(sp, node)...)
	if isIterating {
		diags = append(diags, validateIteratingOutcomes(sp, node)...)
	} else if len(node.Outcomes) == 0 {
		diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("step %q: at least one outcome is required", sp.Name)})
	}

	d, node.Body, node.BodyEntry = compileWorkflowBody(sp, spec, schemas, opts)
	diags = append(diags, d...)

	// Decode the optional `input = { ... }` attribute for body variable binding.
	bodyInputExpr, d := decodeBodyInputAttr(sp)
	diags = append(diags, d...)

	// Validate that every required body variable (no default) has an input
	// binding. A missing input expression when the body requires variables is
	// caught here at compile time so the user gets an actionable diagnostic
	// rather than a silent null at runtime.
	if node.Body != nil {
		var missingVars []string
		for varName, varNode := range node.Body.Variables {
			if varNode.IsRequired() && bodyInputExpr == nil {
				missingVars = append(missingVars, varName)
			}
		}
		if len(missingVars) > 0 {
			sort.Strings(missingVars)
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary: fmt.Sprintf(
					"step %q: body variable(s) %s have no default; add input = { %s = ... } to the parent step",
					sp.Name, strings.Join(missingVars, ", "), missingVars[0],
				),
			})
		}
		node.BodyInputExpr = bodyInputExpr
	}

	diags = append(diags, compileWorkflowOutputs(g, sp, node, opts)...)

	g.Steps[sp.Name] = node
	g.stepOrder = append(g.stepOrder, sp.Name)
	return diags
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
// It builds a synthetic *Spec from the BodySpec and compiles it recursively.
func compileWorkflowBodyInline(sp *StepSpec, spec *Spec, schemas map[string]AdapterInfo, opts CompileOpts) (hcl.Diagnostics, *FSMGraph, string) {
	var diags hcl.Diagnostics
	wb := sp.Workflow
	if len(wb.Steps) == 0 {
		return append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("step %q: workflow body must contain at least one step", sp.Name),
		}), nil, ""
	}

	// Determine entry point: explicit entry > initial_state > first step.
	entry := wb.Entry
	if entry == "" {
		entry = wb.InitialState
	}
	if entry == "" {
		entry = wb.Steps[0].Name
	}

	// Build states slice with the synthetic _continue terminal appended.
	// Copy to avoid mutating the parsed slice.
	states := make([]StateSpec, len(wb.States), len(wb.States)+1)
	copy(states, wb.States)
	states = append(states, StateSpec{Name: "_continue", Terminal: true})

	bodySpec := &Spec{
		Name:         sp.Name + ":body",
		Version:      "1",
		InitialState: entry,
		TargetState:  "_continue",
		Variables:    wb.Variables,
		Locals:       wb.Locals,
		Agents:       wb.Agents,
		Steps:        wb.Steps,
		States:       states,
		Waits:        wb.Waits,
		Approvals:    wb.Approvals,
		Branches:     wb.Branches,
		Policy:       wb.Policy,
		Permissions:  wb.Permissions,
		SourceBytes:  spec.SourceBytes, // propagate for BranchArm.ConditionSrc
	}

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

// decodeBodyInputAttr extracts the optional `input = { ... }` map expression
// from the parent step's Remain body. Returns nil, nil when the attribute is
// absent. The expression is stored on StepNode.BodyInputExpr and evaluated at
// iteration entry to seed the child scope's var.* bindings.
func decodeBodyInputAttr(sp *StepSpec) (hcl.Expression, hcl.Diagnostics) {
	schema := &hcl.BodySchema{
		Attributes: []hcl.AttributeSchema{{Name: "input", Required: false}},
	}
	content, _, diags := sp.Remain.PartialContent(schema)
	if diags.HasErrors() || content == nil {
		return nil, diags
	}
	if attr, ok := content.Attributes["input"]; ok {
		return attr.Expr, nil
	}
	return nil, nil
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
