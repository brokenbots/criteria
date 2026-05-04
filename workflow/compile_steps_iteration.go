package workflow

// compile_steps_iteration.go — compile path for steps that carry a for_each or
// count modifier. Supports both adapter and subworkflow target kinds.

import (
	"fmt"
	"time"

	"github.com/hashicorp/hcl/v2"
)

// compileIteratingStep compiles a for_each/count iterating step and registers
// it in g. targetKind, adapterRef, and subworkflowRef come from resolveStepTarget.
//
//nolint:funlen // W11: function length unavoidable due to comprehensive iteration and adapter validation
func compileIteratingStep(g *FSMGraph, sp *StepSpec, spec *Spec, schemas map[string]AdapterInfo, opts CompileOpts,
	targetKind StepTargetKind, adapterRef, subworkflowRef string) hcl.Diagnostics {
	var diags hcl.Diagnostics

	ok, d := validateStepRegistration(g, sp)
	diags = append(diags, d...)
	if !ok {
		return diags
	}

	diags = append(diags, validateAllowToolsWithAdapter(sp, adapterRef)...)
	diags = append(diags, validateLegacyConfig(sp)...)
	diags = append(diags, validateOnFailureValue(sp)...)

	// Environment override: valid only for adapter targets; subworkflow targets
	// use the environment declared on the subworkflow block.
	var envKey string
	if targetKind != StepTargetSubworkflow {
		var d hcl.Diagnostics
		envKey, d = resolveStepEnvironmentOverride(sp.Name, sp.Remain, g)
		diags = append(diags, d...)
	} else {
		diags = append(diags, rejectEnvOverrideForSubworkflow(sp.Name, sp.Remain)...)
	}

	effectiveOnCrash, d := resolveStepOnCrashWithAdapter(g, sp, adapterRef)
	diags = append(diags, d...)

	timeout, d := decodeStepTimeout(sp)
	diags = append(diags, d...)

	if sp.MaxVisits < 0 {
		diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("step %q: max_visits must be >= 0", sp.Name)})
	}

	forEachExpr, countExpr, d := decodeRemainIter(sp)
	diags = append(diags, d...)
	if forEachExpr != nil && countExpr != nil {
		diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("step %q: for_each and count are mutually exclusive", sp.Name)})
	}
	diags = append(diags, validateIterExprFold(g, opts, forEachExpr, countExpr)...)

	adapterType := adapterTypeFromRef(adapterRef)

	var node *StepNode
	if targetKind == StepTargetSubworkflow {
		swInputExprs, d := compileSubworkflowStepInputExprs(g, sp, subworkflowRef)
		diags = append(diags, d...)
		node = newSubworkflowIterStepNode(sp, spec, subworkflowRef, effectiveOnCrash, envKey, timeout, swInputExprs)
	} else {
		inputMap, inputExprs, d := decodeStepInput(g, sp, schemas, opts, adapterType)
		diags = append(diags, d...)
		// each.* references are valid inside iterating steps; no error emitted.
		node = newAdapterStepNode(sp, spec, adapterRef, effectiveOnCrash, envKey, timeout, inputMap, inputExprs)
		diags = append(diags, maybeCopilotAliasWarnings(sp.Name, adapterType, node.AllowTools)...)
	}

	node.ForEach = forEachExpr
	node.Count = countExpr

	diags = append(diags, compileOutcomeBlock(sp, node)...)
	diags = append(diags, validateIteratingOutcomes(sp, node)...)

	g.Steps[sp.Name] = node
	g.stepOrder = append(g.stepOrder, sp.Name)
	return diags
}

// newSubworkflowIterStepNode constructs a StepNode for an iterating subworkflow step.
func newSubworkflowIterStepNode(sp *StepSpec, _ *Spec, subworkflowRef, effectiveOnCrash, envKey string, timeout time.Duration, inputExprs map[string]hcl.Expression) *StepNode {
	return &StepNode{
		Name:           sp.Name,
		TargetKind:     StepTargetSubworkflow,
		SubworkflowRef: subworkflowRef,
		OnCrash:        effectiveOnCrash,
		OnFailure:      sp.OnFailure,
		MaxVisits:      sp.MaxVisits,
		Timeout:        timeout,
		InputExprs:     inputExprs,
		Outcomes:       map[string]string{},
		Environment:    envKey,
	}
}

// decodeRemainIter reads the for_each and count expressions from sp.Remain
// without side-effects on any prior or future PartialContent calls.
func decodeRemainIter(sp *StepSpec) (forEachExpr, countExpr hcl.Expression, diags hcl.Diagnostics) {
	if sp.Remain == nil {
		return nil, nil, nil
	}
	content, _, d := sp.Remain.PartialContent(&hcl.BodySchema{
		Attributes: []hcl.AttributeSchema{
			{Name: "for_each", Required: false},
			{Name: "count", Required: false},
		},
	})
	diags = append(diags, d...)
	if content != nil {
		if attr, ok := content.Attributes["for_each"]; ok {
			forEachExpr = attr.Expr
		}
		if attr, ok := content.Attributes["count"]; ok {
			countExpr = attr.Expr
		}
	}
	return forEachExpr, countExpr, diags
}

// validateOnFailureValue checks that sp.OnFailure is a recognised value.
// It does not check whether on_failure is allowed on this step kind.
func validateOnFailureValue(sp *StepSpec) hcl.Diagnostics {
	if sp.OnFailure == "" {
		return nil
	}
	switch sp.OnFailure {
	case "continue", "abort", "ignore":
		return nil
	default:
		return hcl.Diagnostics{&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("step %q: invalid on_failure %q; must be \"continue\", \"abort\", or \"ignore\"", sp.Name, sp.OnFailure),
		}}
	}
}

// validateEachRefs emits a diagnostic for each input expression that
// references each.* when the step is not iterating.
func validateEachRefs(stepName string, inputExprs map[string]hcl.Expression) hcl.Diagnostics {
	var diags hcl.Diagnostics
	for k, expr := range inputExprs {
		if refsEach(expr) {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("step %q input.%s: each._idx, each.key, each.value, each._prev, each._total, each._first, and each._last are only available inside iterating steps (for_each or count)", stepName, k),
			})
		}
	}
	return diags
}

// validateIteratingOutcomes checks that iterating steps declare the required
// all_succeeded outcome and warns when any_failed is absent.
func validateIteratingOutcomes(sp *StepSpec, node *StepNode) hcl.Diagnostics {
	var diags hcl.Diagnostics
	if _, ok := node.Outcomes["all_succeeded"]; !ok {
		diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("step %q: iterating steps must declare outcome \"all_succeeded\"", sp.Name)})
	}
	if _, ok := node.Outcomes["any_failed"]; !ok {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagWarning,
			Summary:  fmt.Sprintf("step %q: outcome \"any_failed\" not declared; failed iterations will fall through to \"all_succeeded\"", sp.Name),
		})
	}
	return diags
}

// validateIterExprFold runs the compile-time fold pass on for_each and count
// expressions. Runtime-only references (steps.*, shared_variable.*) are
// silently deferred; any other fold errors are returned.
func validateIterExprFold(g *FSMGraph, opts CompileOpts, forEachExpr, countExpr hcl.Expression) hcl.Diagnostics {
	var diags hcl.Diagnostics
	if forEachExpr != nil {
		_, _, d := FoldExpr(forEachExpr, graphVars(g), graphLocals(g), opts.WorkflowDir)
		diags = append(diags, errorDiagsWithFallbackSubject(d, forEachExpr)...)
	}
	if countExpr != nil {
		_, _, d := FoldExpr(countExpr, graphVars(g), graphLocals(g), opts.WorkflowDir)
		diags = append(diags, errorDiagsWithFallbackSubject(d, countExpr)...)
	}
	return diags
}
