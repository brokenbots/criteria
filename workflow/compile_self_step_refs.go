package workflow

// compile_self_step_refs.go — compile-time rejection of a step reading its own
// outputs through the steps.<self>.* namespace inside its own outcome blocks.
//
// The steps.<name>.* namespace for a step is only populated after that step
// completes, and not at all on failure paths where the step produced no
// outputs. A step that references steps.<self> in its own outcome therefore
// resolves on some paths (e.g. success with outputs) and fails at runtime on
// others (e.g. a failure outcome). Catching this at compile time avoids a long
// run dying late on an error that was always knowable up front.

import (
	"fmt"

	"github.com/hashicorp/hcl/v2"
)

// checkSelfStepOutputRefs emits a hard compile error for every steps.<self>.*
// traversal found in a step's own outcome output projections and write value
// expressions.
func checkSelfStepOutputRefs(g *FSMGraph) hcl.Diagnostics {
	var diags hcl.Diagnostics
	for _, name := range g.stepOrder {
		step := g.Steps[name]
		if step == nil {
			continue
		}
		forEachOutcome(step, func(co *CompiledOutcome) {
			diags = append(diags, selfStepRefDiags(
				fmt.Sprintf("step %q outcome %q output", step.Name, co.Name), step.Name, co.OutputExpr)...)
			for _, w := range co.Writes {
				diags = append(diags, selfStepRefDiags(
					fmt.Sprintf("step %q outcome %q write %q %q", step.Name, co.Name, w.DataKind, w.DataName), step.Name, w.ValueExpr)...)
			}
		})
	}
	return diags
}

// selfStepRefDiags returns a diagnostic for every traversal in expr that reads
// steps.<selfName>.*.
func selfStepRefDiags(context, selfName string, expr hcl.Expression) hcl.Diagnostics {
	if expr == nil {
		return nil
	}
	var diags hcl.Diagnostics
	for _, tr := range expr.Variables() {
		if len(tr) < 2 {
			continue
		}
		root, ok1 := tr[0].(hcl.TraverseRoot)
		self, ok2 := tr[1].(hcl.TraverseAttr)
		if !ok1 || !ok2 || root.Name != "steps" || self.Name != selfName {
			continue
		}
		r := tr.SourceRange()
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("%s: cannot reference this step's own output via steps.%s", context, selfName),
			Detail:   fmt.Sprintf("steps.%s.* is only populated after the step completes, and not at all on failure paths where the step produced no outputs, so this reference resolves on some paths and fails on others at runtime. Use output.<field> or step.output.<field> for adapter outputs, or subworkflow.<field> for subworkflow return values.", selfName),
			Subject:  r.Ptr(),
		})
	}
	return diags
}
