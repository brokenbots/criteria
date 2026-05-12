package workflow

// compile_steps.go — top-level step dispatcher: routes each StepSpec to the
// appropriate per-kind compiler based on the resolved `target` attribute.
//
// Per-kind implementations live in:
//   - compile_steps_adapter.go      — adapter-targeted steps (non-iterating)
//   - compile_steps_subworkflow.go  — subworkflow-targeted steps
//   - compile_steps_iteration.go    — for_each/count/while iterating steps
//   - compile_steps_graph.go        — shared graph helpers (warnBackEdges etc.)

import (
	"fmt"

	"github.com/hashicorp/hcl/v2"
)

// compileSteps compiles all step blocks from spec into g.Steps and g.stepOrder.
// Must be called after compileAdapters and compileSubworkflows so that adapter
// and subworkflow references can be resolved.
func compileSteps(g *FSMGraph, spec *Spec, schemas map[string]AdapterInfo, opts CompileOpts) hcl.Diagnostics {
	var diags hcl.Diagnostics
	for i := range spec.Steps {
		sp := &spec.Steps[i]
		targetKind, adapterRef, subworkflowRef, d := resolveStepTarget(sp.Name, sp.Remain, g)
		diags = append(diags, d...)
		if d.HasErrors() {
			// Registration still needed so duplicate detection works on subsequent
			// steps, but skip compilation to avoid cascading errors.
			ok, rd := validateStepRegistration(g, sp)
			diags = append(diags, rd...)
			if ok {
				g.Steps[sp.Name] = &StepNode{Name: sp.Name, Outcomes: map[string]*CompiledOutcome{}}
				g.stepOrder = append(g.stepOrder, sp.Name)
			}
			continue
		}

		switch {
		case isIteratingStep(sp):
			diags = append(diags, compileIteratingStep(g, sp, spec, schemas, opts, targetKind, adapterRef, subworkflowRef)...)
		case targetKind == StepTargetSubworkflow:
			diags = append(diags, compileSubworkflowStep(g, sp, spec, subworkflowRef, opts)...)
		default:
			diags = append(diags, compileAdapterStep(g, sp, spec, schemas, opts, adapterRef)...)
		}
	}
	return diags
}

// isIteratingStep reports whether sp has a for_each, count, parallel, or while
// attribute in its Remain body. Uses JustAttributes which does not mark
// attributes as consumed, so the per-kind compiler's decodeRemainIter call
// still finds them.
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
	_, hasParallel := attrs["parallel"]
	_, hasWhile := attrs["while"]
	return hasForEach || hasCount || hasParallel || hasWhile
}

// validateStepRegistration checks for duplicate steps, state name clashes, and
// reserved names (e.g. "return"). Returns false when the step should be skipped.
func validateStepRegistration(g *FSMGraph, sp *StepSpec) (ok bool, diags hcl.Diagnostics) {
	diags = append(diags, validateStepNameNotReturn(sp)...)
	if diags.HasErrors() {
		return false, diags
	}
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
