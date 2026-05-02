package workflow

// compile_steps.go — top-level step dispatcher: routes each StepSpec to the
// appropriate per-kind compiler.
//
// Per-kind implementations live in:
//   - compile_steps_adapter.go    — adapter/agent steps (non-iterating)
//   - compile_steps_iteration.go  — for_each/count iterating steps
//   - compile_steps_workflow.go   — type="workflow" steps (inline + file body)
//   - compile_steps_graph.go      — shared graph helpers (warnBackEdges etc.)

import (
	"fmt"

	"github.com/hashicorp/hcl/v2"
)

// compileSteps compiles all step blocks from spec into g.Steps and g.stepOrder.
// Must be called after compileAgents so that agent references can be resolved.
// Routes each step to the appropriate per-kind compiler based on step type and
// iteration modifier.
func compileSteps(g *FSMGraph, spec *Spec, schemas map[string]AdapterInfo, opts CompileOpts) hcl.Diagnostics {
	var diags hcl.Diagnostics
	for i := range spec.Steps {
		sp := &spec.Steps[i]
		switch {
		case sp.Type == "workflow":
			diags = append(diags, compileWorkflowStep(g, sp, spec, schemas, opts)...)
		case isIteratingStep(sp):
			diags = append(diags, compileIteratingStep(g, sp, spec, schemas, opts)...)
		default:
			diags = append(diags, compileAdapterStep(g, sp, spec, schemas, opts)...)
		}
	}
	return diags
}

// isIteratingStep reports whether sp has a for_each or count attribute in its
// Remain body. Uses JustAttributes which does not mark attributes as consumed,
// so the per-kind compiler's decodeRemainIter call still finds them.
func isIteratingStep(sp *StepSpec) bool {
	if sp.Remain == nil {
		return false
	}
	// JustAttributes is a non-destructive read: it does not update hiddenAttrs,
	// leaving the attributes available for the subsequent PartialContent call
	// inside decodeRemainIter. Blocks in Remain (unusual/erroneous HCL) are
	// intentionally not reported here; we only check for iteration attributes.
	attrs, _ := sp.Remain.JustAttributes()
	_, hasForEach := attrs["for_each"]
	_, hasCount := attrs["count"]
	return hasForEach || hasCount
}

// validateStepRegistration checks for duplicate steps and state name clashes.
// Returns false when the step should be skipped entirely.
func validateStepRegistration(g *FSMGraph, sp *StepSpec) (ok bool, diags hcl.Diagnostics) {
	if _, dup := g.Steps[sp.Name]; dup {
		diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("duplicate step %q", sp.Name)})
		return false, diags
	}
	if _, clash := g.States[sp.Name]; clash {
		diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("step %q clashes with state of the same name", sp.Name)})
		return false, diags
	}
	return true, nil
}

// validateStepKindSelectionDiags validates that exactly one of adapter, agent,
// or type="workflow" is set on sp, and that the type value is recognised.
func validateStepKindSelectionDiags(sp *StepSpec) hcl.Diagnostics {
	var diags hcl.Diagnostics
	hasAdapter := sp.Adapter != ""
	hasAgent := sp.Agent != ""
	hasWorkflowType := sp.Type == "workflow"

	numKinds := 0
	if hasAdapter {
		numKinds++
	}
	if hasAgent {
		numKinds++
	}
	if hasWorkflowType {
		numKinds++
	}
	if sp.Type != "" && sp.Type != "workflow" {
		diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("step %q: invalid type %q; only \"workflow\" is recognised", sp.Name, sp.Type)})
	} else if numKinds != 1 {
		diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("step %q: exactly one of adapter, agent, or type=\"workflow\" must be set", sp.Name)})
	}
	return diags
}
