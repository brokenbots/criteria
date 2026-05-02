package workflow

// compile_steps_workflow.go — compile path for type="workflow" steps (inline
// workflow body and workflow_file variants) plus supporting helpers.

import (
	"fmt"
	"time"

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

	inputMap, inputExprs, d := decodeStepInput(sp, schemas, opts, "")
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

	d, node.Body, node.BodyEntry = compileWorkflowBody(sp, schemas, opts)
	diags = append(diags, d...)

	diags = append(diags, compileWorkflowOutputs(sp, node)...)

	g.Steps[sp.Name] = node
	g.stepOrder = append(g.stepOrder, sp.Name)
	return diags
}

// compileWorkflowIterExpr decodes the for_each/count expressions, checks for
// mutual exclusion, and validates the on_failure constraint for workflow steps.
func compileWorkflowIterExpr(sp *StepSpec) (forEachExpr, countExpr hcl.Expression, isIterating bool, diags hcl.Diagnostics) {
	forEachExpr, countExpr, d := decodeRemainIter(sp)
	diags = append(diags, d...)
	if forEachExpr != nil && countExpr != nil {
		diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("step %q: for_each and count are mutually exclusive", sp.Name)})
	}
	isIterating = forEachExpr != nil || countExpr != nil
	if sp.OnFailure != "" && !isIterating {
		diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("step %q: on_failure requires for_each or count", sp.Name)})
	}
	return forEachExpr, countExpr, isIterating, diags
}

// newWorkflowStepNode constructs a StepNode for a type="workflow" step with
// all common fields plus optional ForEach/Count.
func newWorkflowStepNode(sp *StepSpec, spec *Spec, effectiveOnCrash string, timeout time.Duration,
	inputMap map[string]string, inputExprs map[string]hcl.Expression,
	forEachExpr, countExpr hcl.Expression) *StepNode {
	return &StepNode{
		Name:       sp.Name,
		OnCrash:    effectiveOnCrash,
		Type:       sp.Type,
		OnFailure:  sp.OnFailure,
		MaxVisits:  sp.MaxVisits,
		Input:      inputMap,
		InputExprs: inputExprs,
		Timeout:    timeout,
		Outcomes:   map[string]string{},
		AllowTools: allowToolsForStep(sp, spec),
		ForEach:    forEachExpr,
		Count:      countExpr,
	}
}

// compileWorkflowOutputs extracts output{} block expressions from sp.Workflow
// and populates node.Outputs. It is safe to call when sp.Workflow is nil or
// has no outputs — it returns nil in that case.
func compileWorkflowOutputs(sp *StepSpec, node *StepNode) hcl.Diagnostics {
	if sp.Workflow == nil || len(sp.Workflow.Outputs) == 0 {
		return nil
	}
	var diags hcl.Diagnostics
	seen := map[string]bool{}
	node.Outputs = make(map[string]hcl.Expression, len(sp.Workflow.Outputs))
	for _, out := range sp.Workflow.Outputs {
		if out == nil {
			continue
		}
		if seen[out.Name] {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("step %q: duplicate output name %q", sp.Name, out.Name),
			})
			continue
		}
		seen[out.Name] = true
		content, _, d := out.Remain.PartialContent(&hcl.BodySchema{
			Attributes: []hcl.AttributeSchema{{Name: "value", Required: true}},
		})
		diags = append(diags, d...)
		if content != nil {
			if attr, ok := content.Attributes["value"]; ok {
				node.Outputs[out.Name] = attr.Expr
			}
		}
	}
	return diags
}

// compileWorkflowBody dispatches to the inline or file-backed body compiler.
// Returns the compiled FSMGraph, the body entry state name, and any diagnostics.
func compileWorkflowBody(sp *StepSpec, schemas map[string]AdapterInfo, opts CompileOpts) (hcl.Diagnostics, *FSMGraph, string) {
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
	return compileWorkflowBodyInline(sp, schemas, opts)
}
