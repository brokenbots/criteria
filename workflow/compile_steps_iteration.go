package workflow

// compile_steps_iteration.go — compile path for adapter- and agent-backed steps
// that carry a for_each or count modifier.

import (
	"fmt"

	"github.com/hashicorp/hcl/v2"
)

// compileIteratingStep compiles a for_each/count iterating step and registers
// it in g. The for_each/count expressions are decoded from sp.Remain.
func compileIteratingStep(g *FSMGraph, sp *StepSpec, spec *Spec, schemas map[string]AdapterInfo, opts CompileOpts) hcl.Diagnostics {
	var diags hcl.Diagnostics

	ok, d := validateStepRegistration(g, sp)
	diags = append(diags, d...)
	if !ok {
		return diags
	}

	diags = append(diags, validateStepKindSelectionDiags(sp)...)
	diags = append(diags, validateAdapterAndAgent(g, sp)...)
	diags = append(diags, validateLegacyConfig(sp)...)
	diags = append(diags, validateOnFailureValue(sp)...)

	effectiveOnCrash, d := resolveStepOnCrash(g, sp)
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

	adapterName := resolveAdapterName(g, sp)
	inputMap, inputExprs, d := decodeStepInput(sp, schemas, opts, adapterName)
	diags = append(diags, d...)

	// each.* references are valid inside iterating steps; no error emitted.

	node := newBaseStepNode(sp, spec, effectiveOnCrash, timeout, inputMap, inputExprs)
	node.ForEach = forEachExpr
	node.Count = countExpr

	diags = append(diags, maybeCopilotAliasWarnings(sp.Name, adapterName, node.AllowTools)...)
	diags = append(diags, compileOutcomeBlock(sp, node)...)
	diags = append(diags, validateIteratingOutcomes(sp, node)...)

	g.Steps[sp.Name] = node
	g.stepOrder = append(g.stepOrder, sp.Name)
	return diags
}
