package workflow

// compile_steps_subworkflow.go — compile path for non-iterating subworkflow-targeted steps.

import (
	"fmt"
	"time"

	"github.com/hashicorp/hcl/v2"
)

// compileSubworkflowStep compiles a non-iterating subworkflow-targeted step and
// registers it in g. subworkflowRef is the pre-resolved subworkflow name from
// resolveStepTarget.
func compileSubworkflowStep(g *FSMGraph, sp *StepSpec, _ *Spec, subworkflowRef string, opts CompileOpts) hcl.Diagnostics {
	var diags hcl.Diagnostics

	ok, d := validateStepRegistration(g, sp)
	diags = append(diags, d...)
	if !ok {
		return diags
	}

	diags = append(diags, validateOnFailureForNonIterating(sp)...)

	envKey, d := resolveStepEnvironmentOverride(sp.Name, sp.Remain, g)
	diags = append(diags, d...)

	if len(sp.AllowTools) > 0 {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("step %q: allow_tools is not valid for subworkflow-targeted steps", sp.Name),
		})
	}

	// Compile the step-level input block if present. Attributes are captured as
	// expressions for runtime evaluation against the parent scope, then passed
	// into the callee as variable bindings (overriding any declaration-level input).
	var inputExprs map[string]hcl.Expression
	if sp.Input != nil {
		attrs, attrDiags := sp.Input.Remain.JustAttributes()
		diags = append(diags, attrDiags...)
		if len(attrs) > 0 {
			inputExprs = make(map[string]hcl.Expression, len(attrs))
			for k, attr := range attrs {
				inputExprs[k] = attr.Expr
			}
		}
	}

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
		Timeout:        time.Duration(timeout),
		InputExprs:     inputExprs,
		Outcomes:       map[string]string{},
		Environment:    envKey,
	}

	diags = append(diags, compileOutcomeBlock(sp, node)...)

	if len(node.Outcomes) == 0 {
		diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("step %q: at least one outcome is required", sp.Name)})
	}

	g.Steps[sp.Name] = node
	g.stepOrder = append(g.stepOrder, sp.Name)
	return diags
}
