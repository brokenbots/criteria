package workflow

// compile_steps_subworkflow.go — compile path for non-iterating subworkflow-targeted steps.

import (
	"fmt"

	"github.com/hashicorp/hcl/v2"
)

// compileSubworkflowStep compiles a non-iterating subworkflow-targeted step and
// registers it in g. subworkflowRef is the pre-resolved subworkflow name from
// resolveStepTarget.
//
//nolint:funlen // W14: sequential compile+validate phases; splitting adds indirection without clarity gain
func compileSubworkflowStep(g *FSMGraph, sp *StepSpec, _ *Spec, subworkflowRef string, opts CompileOpts) hcl.Diagnostics {
	var diags hcl.Diagnostics

	ok, d := validateStepRegistration(g, sp)
	diags = append(diags, d...)
	if !ok {
		return diags
	}

	diags = append(diags, validateOnFailureForNonIterating(sp)...)

	if len(sp.AllowTools) > 0 {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("step %q: allow_tools is not valid for subworkflow-targeted steps", sp.Name),
		})
	}

	// Environment override is not applicable for subworkflow-targeted steps;
	// the environment for the callee is set on the subworkflow declaration.
	diags = append(diags, rejectEnvOverrideForSubworkflow(sp.Name, sp.Remain)...)

	// Compile the step-level input block if present. Attributes are captured as
	// expressions for runtime evaluation against the parent scope, then passed
	// into the callee as variable bindings (overriding any declaration-level input).
	// Keys are validated against the callee's declared variables so typos are
	// caught at compile time rather than silently dropped at runtime.
	inputExprs, d := compileSubworkflowStepInputExprs(g, sp, subworkflowRef)
	diags = append(diags, d...)

	diags = append(diags, validateLegacyConfig(sp)...)

	timeout, d := decodeStepTimeout(sp)
	diags = append(diags, d...)

	if sp.MaxVisits < 0 {
		diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("step %q: max_visits must be >= 0", sp.Name)})
	}

	effectiveOnCrash := sp.OnCrash
	if effectiveOnCrash != "" && !isValidOnCrash(effectiveOnCrash) {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("step %q: invalid on_crash %q", sp.Name, effectiveOnCrash),
		})
		effectiveOnCrash = ""
	}

	_ = opts // reserved for future use (e.g. depth limiting)

	node := &StepNode{
		Name:           sp.Name,
		TargetKind:     StepTargetSubworkflow,
		SubworkflowRef: subworkflowRef,
		OnCrash:        effectiveOnCrash,
		MaxVisits:      sp.MaxVisits,
		Timeout:        timeout,
		InputExprs:     inputExprs,
		Outcomes:       map[string]*CompiledOutcome{},
		Environment:    "",
	}

	diags = append(diags, compileOutcomeBlock(sp, node, g, opts)...)

	if len(node.Outcomes) == 0 {
		diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("step %q: at least one outcome is required", sp.Name)})
	}

	g.Steps[sp.Name] = node
	g.stepOrder = append(g.stepOrder, sp.Name)
	return diags
}

// compileSubworkflowStepInputExprs captures and validates the step-level input { }
// block for subworkflow-targeted steps. Each attribute key is validated against the
// callee's declared variables (undeclared keys are rejected as a compile error).
// Returns nil, nil when no input block is present.
func compileSubworkflowStepInputExprs(g *FSMGraph, sp *StepSpec, subworkflowRef string) (map[string]hcl.Expression, hcl.Diagnostics) {
	if sp.Input == nil {
		return nil, nil
	}
	attrs, diags := sp.Input.Remain.JustAttributes()
	if len(attrs) == 0 {
		return nil, diags
	}
	exprs := make(map[string]hcl.Expression, len(attrs))
	for k, attr := range attrs {
		exprs[k] = attr.Expr
	}
	if swNode, ok := g.Subworkflows[subworkflowRef]; ok {
		for k, expr := range exprs {
			diags = append(diags, validateInputItem(subworkflowRef, k, expr, swNode.DeclaredVars)...)
		}
	}
	return exprs, diags
}
