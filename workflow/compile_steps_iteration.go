package workflow

// compile_steps_iteration.go — compile path for steps that carry a for_each,
// count, or parallel modifier. Supports both adapter and subworkflow target kinds.

import (
	"fmt"
	"runtime"
	"time"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
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

	forEachExpr, countExpr, parallelExpr, parallelMax, d := decodeRemainIter(sp, g)
	diags = append(diags, d...)
	if forEachExpr != nil && countExpr != nil {
		diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("step %q: for_each and count are mutually exclusive", sp.Name)})
	}
	if parallelExpr != nil && forEachExpr != nil {
		diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("step %q: parallel and for_each are mutually exclusive", sp.Name)})
	}
	if parallelExpr != nil && countExpr != nil {
		diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("step %q: parallel and count are mutually exclusive", sp.Name)})
	}
	diags = append(diags, validateIterExprFold(g, opts, forEachExpr, countExpr, parallelExpr)...)
	if parallelExpr != nil {
		diags = append(diags, validateParallelIsList(g, opts, sp.Name, parallelExpr)...)
	}

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
		// parallel_safe capability gate: when the step uses parallel = [...] the
		// adapter must declare "parallel_safe". When the adapter is absent from the
		// schemas map (binary not found during schema collection), we skip the check
		// here and rely on the runtime gate in evaluateParallel instead.
		if parallelExpr != nil {
			if info, ok := adapterInfo(schemas, adapterType); ok {
				if !adapterHasCapability(info, "parallel_safe") {
					diags = append(diags, &hcl.Diagnostic{
						Severity: hcl.DiagError,
						Summary: fmt.Sprintf(
							"step %q: adapter type %q does not declare the \"parallel_safe\" capability; "+
								"parallel execution requires the adapter to be safe for concurrent Execute calls. "+
								"Use for_each for sequential iteration or declare parallel_safe in the adapter's Info().",
							sp.Name, adapterType),
					})
				}
			}
		}
	}

	node.ForEach = forEachExpr
	node.Count = countExpr
	node.Parallel = parallelExpr
	node.ParallelMax = parallelMax

	diags = append(diags, compileOutcomeBlock(sp, node, g, opts, schemas[adapterRef].OutputSchema)...)
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
		Outcomes:       map[string]*CompiledOutcome{},
		Environment:    envKey,
	}
}

// decodeRemainIter reads the for_each, count, parallel, and parallel_max
// expressions from sp.Remain without side-effects on any prior or future
// PartialContent calls. parallelMax of 0 means "use default (GOMAXPROCS)".
func decodeRemainIter(sp *StepSpec, g *FSMGraph) (forEachExpr, countExpr, parallelExpr hcl.Expression, parallelMax int, diags hcl.Diagnostics) {
	if sp.Remain == nil {
		return nil, nil, nil, 0, nil
	}
	content, _, d := sp.Remain.PartialContent(&hcl.BodySchema{
		Attributes: []hcl.AttributeSchema{
			{Name: "for_each", Required: false},
			{Name: "count", Required: false},
			{Name: "parallel", Required: false},
			{Name: "parallel_max", Required: false},
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
		if attr, ok := content.Attributes["parallel"]; ok {
			parallelExpr = attr.Expr
		}
		if attr, ok := content.Attributes["parallel_max"]; ok {
			var val int
			d2 := decodeIntAttr(attr, g, &val)
			diags = append(diags, d2...)
			if !d2.HasErrors() {
				if val < 1 {
					diags = append(diags, &hcl.Diagnostic{
						Severity: hcl.DiagError,
						Subject:  attr.Expr.StartRange().Ptr(),
						Summary:  fmt.Sprintf("step %q: parallel_max must be >= 1; got %d", sp.Name, val),
					})
				} else {
					parallelMax = val
				}
			}
		}
	}
	// Apply default for parallelMax when parallel is set but parallel_max is absent.
	if parallelExpr != nil && parallelMax == 0 {
		parallelMax = runtime.GOMAXPROCS(0)
	}
	return forEachExpr, countExpr, parallelExpr, parallelMax, diags
}

// validateOnFailureValue checks that sp.OnFailure is a recognised value.
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
				Summary:  fmt.Sprintf("step %q input.%s: each._idx, each.key, each.value, each._prev, each._total, each._first, and each._last are only available inside iterating steps (for_each, count, or parallel)", stepName, k),
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

// validateIterExprFold runs the compile-time fold pass on for_each, count, and
// parallel expressions. Runtime-only references (steps.*, shared_variable.*) are
// silently deferred; any other fold errors are returned.
func validateIterExprFold(g *FSMGraph, opts CompileOpts, forEachExpr, countExpr, parallelExpr hcl.Expression) hcl.Diagnostics {
	var diags hcl.Diagnostics
	if forEachExpr != nil {
		_, _, d := FoldExpr(forEachExpr, graphVars(g), graphLocals(g), opts.WorkflowDir)
		diags = append(diags, errorDiagsWithFallbackSubject(d, forEachExpr)...)
	}
	if countExpr != nil {
		_, _, d := FoldExpr(countExpr, graphVars(g), graphLocals(g), opts.WorkflowDir)
		diags = append(diags, errorDiagsWithFallbackSubject(d, countExpr)...)
	}
	if parallelExpr != nil {
		_, _, d := FoldExpr(parallelExpr, graphVars(g), graphLocals(g), opts.WorkflowDir)
		diags = append(diags, errorDiagsWithFallbackSubject(d, parallelExpr)...)
	}
	return diags
}

// validateParallelIsList checks at compile time that the parallel expression
// evaluates to a list or tuple (not a map or object). parallel = {} syntax is
// not supported; use parallel = [...] for all parallel fan-out. If the expression
// cannot be folded at compile time, the check is deferred to runtime.
func validateParallelIsList(g *FSMGraph, opts CompileOpts, stepName string, expr hcl.Expression) hcl.Diagnostics {
	val, folded, diags := FoldExpr(expr, graphVars(g), graphLocals(g), opts.WorkflowDir)
	if diags.HasErrors() || !folded {
		return nil // deferred to runtime; fold errors already reported by validateIterExprFold
	}
	if !val.IsKnown() || val.IsNull() {
		return nil
	}
	t := val.Type()
	if t.IsObjectType() || t.IsMapType() {
		return hcl.Diagnostics{&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Subject:  expr.StartRange().Ptr(),
			Summary:  fmt.Sprintf("step %q: parallel must be a list [...]; map and object syntax are not supported", stepName),
		}}
	}
	return nil
}

// both literal values and var.* references whose default is a known number.
// Returns an error diagnostic if the expression is runtime-only, unknown,
// non-numeric, or fractional.
func decodeIntAttr(attr *hcl.Attribute, g *FSMGraph, dst *int) hcl.Diagnostics {
	foldedVal, folded, diags := FoldExpr(attr.Expr, graphVars(g), graphLocals(g), "")
	if diags.HasErrors() {
		return diags
	}
	if !folded {
		return hcl.Diagnostics{&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Subject:  attr.Expr.StartRange().Ptr(),
			Summary:  fmt.Sprintf("%q: must be a compile-time value (literal or var.* with a known default); runtime-only expressions are not allowed", attr.Name),
		}}
	}
	val := foldedVal
	if val.IsNull() || !val.IsKnown() {
		return hcl.Diagnostics{&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Subject:  attr.Expr.StartRange().Ptr(),
			Summary:  fmt.Sprintf("%q: value must be a compile-time integer literal or var.* with a known default", attr.Name),
		}}
	}
	if !val.Type().Equals(cty.Number) {
		return hcl.Diagnostics{&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Subject:  attr.Expr.StartRange().Ptr(),
			Summary:  fmt.Sprintf("%q: value must be a number; got %s", attr.Name, val.Type().FriendlyName()),
		}}
	}
	bf := val.AsBigFloat()
	if !bf.IsInt() {
		return hcl.Diagnostics{&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Subject:  attr.Expr.StartRange().Ptr(),
			Summary:  fmt.Sprintf("%q: value must be a whole number; got a fractional value", attr.Name),
		}}
	}
	n, _ := bf.Int64()
	*dst = int(n)
	return nil
}
