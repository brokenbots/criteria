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

// compileIteratingStep compiles a for_each/count/while iterating step and registers
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

	ie, d := decodeRemainIter(sp, g)
	diags = append(diags, d...)
	diags = append(diags, validateIterMutualExclusion(sp.Name, ie.ForEach, ie.Count, ie.Parallel, ie.While)...)
	diags = append(diags, validateIterExprFold(g, opts, ie.ForEach, ie.Count, ie.Parallel)...)
	if ie.While != nil {
		diags = append(diags, validateWhileExprType(g, opts, sp.Name, ie.While)...)
	}
	if ie.Parallel != nil {
		diags = append(diags, validateParallelIsList(g, opts, sp.Name, ie.Parallel)...)
	}

	adapterType := adapterTypeFromRef(adapterRef)

	var node *StepNode
	if targetKind == StepTargetSubworkflow {
		swInputExprs, d := compileSubworkflowStepInputExprs(g, sp, subworkflowRef)
		diags = append(diags, d...)
		if ie.While == nil {
			diags = append(diags, validateWhileRefs(sp.Name, swInputExprs)...)
		}
		node = newSubworkflowIterStepNode(sp, spec, subworkflowRef, effectiveOnCrash, envKey, timeout, swInputExprs)
	} else {
		inputMap, inputExprs, d := decodeStepInput(g, sp, schemas, opts, adapterType)
		diags = append(diags, d...)
		if ie.While == nil {
			diags = append(diags, validateWhileRefs(sp.Name, inputExprs)...)
		}
		// each.* references are valid inside iterating steps; no error emitted.
		node = newAdapterStepNode(sp, spec, adapterRef, effectiveOnCrash, envKey, timeout, inputMap, inputExprs)
		diags = append(diags, maybeCopilotAliasWarnings(sp.Name, adapterType, node.AllowTools)...)
		// parallel_safe capability gate: when the step uses parallel = [...] the
		// adapter must declare "parallel_safe". When the adapter is absent from the
		// schemas map (binary not found during schema collection), we skip the check
		// here and rely on the runtime gate in evaluateParallel instead.
		if ie.Parallel != nil {
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

	node.ForEach = ie.ForEach
	node.Count = ie.Count
	node.Parallel = ie.Parallel
	node.ParallelMax = ie.ParallelMax
	node.While = ie.While

	diags = append(diags, compileOutcomeBlock(sp, node, g, opts, schemas[adapterRef].OutputSchema)...)
	diags = append(diags, validateIteratingOutcomes(sp, node)...)
	diags = append(diags, warnParallelPerIterSharedWrites(sp.Name, ie.Parallel, node)...)

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

// validateIterMutualExclusion checks that at most one of for_each, count,
// parallel, and while is set on a step.
func validateIterMutualExclusion(stepName string, forEachExpr, countExpr, parallelExpr, whileExpr hcl.Expression) hcl.Diagnostics {
	var diags hcl.Diagnostics
	pairs := [][2]string{
		{"for_each", "count"},
		{"parallel", "for_each"},
		{"parallel", "count"},
		{"while", "for_each"},
		{"while", "count"},
		{"while", "parallel"},
	}
	exprs := map[string]hcl.Expression{
		"for_each": forEachExpr,
		"count":    countExpr,
		"parallel": parallelExpr,
		"while":    whileExpr,
	}
	for _, pair := range pairs {
		if exprs[pair[0]] != nil && exprs[pair[1]] != nil {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("step %q: %s and %s are mutually exclusive", stepName, pair[0], pair[1]),
			})
		}
	}
	return diags
}

// iterExprs holds the parsed iteration modifier expressions for a step.
type iterExprs struct {
	ForEach     hcl.Expression
	Count       hcl.Expression
	Parallel    hcl.Expression
	ParallelMax int
	While       hcl.Expression
}

// decodeRemainIter reads the for_each, count, parallel, parallel_max, and while
// expressions from sp.Remain without side-effects on any prior or future
// PartialContent calls. ParallelMax of 0 means "use default (GOMAXPROCS)".
func decodeRemainIter(sp *StepSpec, g *FSMGraph) (iterExprs, hcl.Diagnostics) {
	var ie iterExprs
	if sp.Remain == nil {
		return ie, nil
	}
	content, _, diags := sp.Remain.PartialContent(&hcl.BodySchema{
		Attributes: []hcl.AttributeSchema{
			{Name: "for_each", Required: false},
			{Name: "count", Required: false},
			{Name: "parallel", Required: false},
			{Name: "parallel_max", Required: false},
			{Name: "while", Required: false},
		},
	})
	if content == nil {
		return ie, diags
	}
	if attr, ok := content.Attributes["for_each"]; ok {
		ie.ForEach = attr.Expr
	}
	if attr, ok := content.Attributes["count"]; ok {
		ie.Count = attr.Expr
	}
	if attr, ok := content.Attributes["parallel"]; ok {
		ie.Parallel = attr.Expr
	}
	if attr, ok := content.Attributes["while"]; ok {
		ie.While = attr.Expr
	}
	if attr, ok := content.Attributes["parallel_max"]; ok {
		var d hcl.Diagnostics
		ie.ParallelMax, d = decodeParallelMax(sp.Name, attr, g)
		diags = append(diags, d...)
	}
	// Apply default for ParallelMax when parallel is set but parallel_max is absent.
	if ie.Parallel != nil && ie.ParallelMax == 0 {
		ie.ParallelMax = runtime.GOMAXPROCS(0)
	}
	return ie, diags
}

// decodeParallelMax decodes and validates the parallel_max attribute.
func decodeParallelMax(stepName string, attr *hcl.Attribute, g *FSMGraph) (int, hcl.Diagnostics) {
	var val int
	diags := decodeIntAttr(attr, g, &val)
	if diags.HasErrors() {
		return 0, diags
	}
	if val < 1 {
		return 0, hcl.Diagnostics{&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Subject:  attr.Expr.StartRange().Ptr(),
			Summary:  fmt.Sprintf("step %q: parallel_max must be >= 1; got %d", stepName, val),
		}}
	}
	return val, nil
}

// validateWhileExprType performs a compile-time static type check on the while
// expression. If the expression can be folded at compile time (i.e. it references
// only known constants or compile-time-resolved variables) and the result is not
// bool, a DiagError is returned. Runtime-only references defer to the runtime check.
func validateWhileExprType(g *FSMGraph, opts CompileOpts, stepName string, expr hcl.Expression) hcl.Diagnostics {
	val, folded, diags := FoldExpr(expr, graphVars(g), graphLocals(g), opts.WorkflowDir)
	if diags.HasErrors() || !folded {
		return nil // deferred to runtime
	}
	if !val.IsKnown() || val.IsNull() {
		return nil
	}
	if !val.Type().Equals(cty.Bool) {
		return hcl.Diagnostics{&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Subject:  expr.StartRange().Ptr(),
			Summary:  fmt.Sprintf("step %q: while must be a bool expression; got %s", stepName, val.Type().FriendlyName()),
		}}
	}
	return nil
}

// validateWhileRefs emits a diagnostic for each input expression that
// references while.* when the step is not a while-driven iterating step.
func validateWhileRefs(stepName string, inputExprs map[string]hcl.Expression) hcl.Diagnostics {
	var diags hcl.Diagnostics
	for k, expr := range inputExprs {
		if refsWhile(expr) {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("step %q input.%s: while.index, while.first, and while._prev are only valid inside while-modified steps", stepName, k),
			})
		}
	}
	return diags
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

// warnParallelPerIterSharedWrites emits a DiagWarning for each per-iteration
// outcome (_continue) on a parallel step that declares shared_writes. Goroutines
// read a pre-parallel snapshot; writes are applied in index order after all
// iterations complete, so accumulation patterns are not safe. Authors should use
// aggregate outcomes with an output = { ... } projection instead.
func warnParallelPerIterSharedWrites(stepName string, parallelExpr hcl.Expression, node *StepNode) hcl.Diagnostics {
	if parallelExpr == nil {
		return nil
	}
	var diags hcl.Diagnostics
	for outcomeName, co := range node.Outcomes {
		if co.Next == "_continue" && len(co.SharedWrites) > 0 {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagWarning,
				Summary: fmt.Sprintf(
					"step %q outcome %q: shared_writes on a parallel step's per-iteration outcome "+
						"are applied in index order after all iterations complete. "+
						"All goroutines read a pre-parallel snapshot, so accumulation patterns "+
						"(e.g. reading shared.x and writing back x+1) are not safe. "+
						"Last-index-wins applies when multiple iterations write the same variable. "+
						"Consider using an aggregate outcome with output = { ... } projection.",
					stepName, outcomeName),
			})
		}
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
