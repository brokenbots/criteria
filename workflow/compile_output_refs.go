package workflow

// compile_output_refs.go — compile-time validation of steps.X.outputs.Y references.
//
// Walks every HCL expression site that may reference step outputs and emits a
// hard compile error when the referenced field is not declared in the target
// step's adapter OutputSchema. Suggestions are Levenshtein-distance-sorted.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
)

// compileOutputRefs validates every steps.X.outputs.Y traversal in the
// compiled graph. It returns diagnostics with error severity for unknown
// output fields, and includes Levenshtein-distance-sorted suggestions.
func compileOutputRefs(g *FSMGraph) hcl.Diagnostics {
	var diags hcl.Diagnostics
	for _, ne := range collectOutputRefExprs(g) {
		diags = append(diags, checkStepsOutputRefs(ne.context, ne.expr, g)...)
	}
	return diags
}

func appendIfNotNil(exprs []namedExpr, context string, expr hcl.Expression) []namedExpr {
	if expr != nil {
		exprs = append(exprs, namedExpr{context, expr})
	}
	return exprs
}

func collectStepOutputRefExprs(g *FSMGraph) []namedExpr {
	var exprs []namedExpr
	for _, name := range g.stepOrder {
		step := g.Steps[name]
		if step == nil {
			continue
		}
		for k, expr := range step.InputExprs {
			exprs = append(exprs, namedExpr{fmt.Sprintf("step %q input %q", step.Name, k), expr})
		}
		for k, expr := range step.SecretInputExprs {
			exprs = append(exprs, namedExpr{fmt.Sprintf("step %q secret_input %q", step.Name, k), expr})
		}
		exprs = appendIfNotNil(exprs, fmt.Sprintf("step %q for_each", step.Name), step.ForEach)
		exprs = appendIfNotNil(exprs, fmt.Sprintf("step %q count", step.Name), step.Count)
		exprs = appendIfNotNil(exprs, fmt.Sprintf("step %q parallel", step.Name), step.Parallel)
		exprs = appendIfNotNil(exprs, fmt.Sprintf("step %q while", step.Name), step.While)
		for outName, co := range step.Outcomes {
			exprs = appendIfNotNil(exprs, fmt.Sprintf("step %q outcome %q output", step.Name, outName), co.OutputExpr)
		}
		if step.DefaultOutcome != nil {
			exprs = appendIfNotNil(exprs, fmt.Sprintf("step %q default outcome output", step.Name), step.DefaultOutcome.OutputExpr)
		}
	}
	return exprs
}

func collectSwitchOutputRefExprs(g *FSMGraph) []namedExpr {
	var exprs []namedExpr
	swNames := make([]string, 0, len(g.Switches))
	for swName := range g.Switches {
		swNames = append(swNames, swName)
	}
	sort.Strings(swNames)
	for _, swName := range swNames {
		sw := g.Switches[swName]
		for i, cond := range sw.Conditions {
			if cond.Match != nil {
				exprs = append(exprs, namedExpr{fmt.Sprintf("switch %q condition[%d] match", swName, i), cond.Match})
			}
			if cond.OutputExpr != nil {
				exprs = append(exprs, namedExpr{fmt.Sprintf("switch %q condition[%d] output", swName, i), cond.OutputExpr})
			}
		}
		if sw.DefaultOutput != nil {
			exprs = append(exprs, namedExpr{fmt.Sprintf("switch %q default output", swName), sw.DefaultOutput})
		}
	}
	return exprs
}

func collectWorkflowOutputRefExprs(g *FSMGraph) []namedExpr {
	var exprs []namedExpr
	for _, name := range g.OutputOrder {
		out := g.Outputs[name]
		if out.Value != nil {
			exprs = append(exprs, namedExpr{fmt.Sprintf("output %q value", name), out.Value})
		}
	}
	return exprs
}

// collectOutputRefExprs gathers every expression site that may contain
// steps.<name>.outputs.<field> traversals, in deterministic order.
func collectOutputRefExprs(g *FSMGraph) []namedExpr {
	exprs := collectStepOutputRefExprs(g)
	exprs = append(exprs, collectSwitchOutputRefExprs(g)...)
	exprs = append(exprs, collectWorkflowOutputRefExprs(g)...)
	return exprs
}

// checkStepsOutputRefs inspects expr for steps.<name>.outputs.<field> traversals
// and emits errors for fields absent from the step's OutputSchema.
func checkStepsOutputRefs(context string, expr hcl.Expression, g *FSMGraph) hcl.Diagnostics {
	var diags hcl.Diagnostics
	for _, traversal := range expr.Variables() {
		// Require: steps . <name> . outputs . <field>
		if len(traversal) < 4 {
			continue
		}
		root, rootOK := traversal[0].(hcl.TraverseRoot)
		nameAttr, nameOK := traversal[1].(hcl.TraverseAttr)
		outAttr, outOK := traversal[2].(hcl.TraverseAttr)
		fieldAttr, fieldOK := traversal[3].(hcl.TraverseAttr)
		if !rootOK || !nameOK || !outOK || !fieldOK {
			continue
		}
		if root.Name != "steps" || outAttr.Name != "outputs" {
			continue
		}

		step, isStep := g.Steps[nameAttr.Name]
		if !isStep {
			r := nameAttr.SrcRange
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("%s: references unknown step %q", context, nameAttr.Name),
				Subject:  &r,
			})
			continue
		}

		if step.TargetKind != StepTargetAdapter || len(step.OutputSchema) == 0 {
			continue // no declared contract; permissive
		}

		if _, known := step.OutputSchema[fieldAttr.Name]; !known {
			r := fieldAttr.SrcRange
			suggestions := suggestOutputFields(fieldAttr.Name, step.OutputSchema)
			detail := fmt.Sprintf("step %q does not declare an output field named %q", nameAttr.Name, fieldAttr.Name)
			if len(suggestions) > 0 {
				detail += fmt.Sprintf("; did you mean %s?", strings.Join(suggestions, ", "))
			}
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("%s: output field %q is not declared in step %q", context, fieldAttr.Name, nameAttr.Name),
				Detail:   detail,
				Subject:  &r,
			})
		}
	}
	return diags
}

// suggestOutputFields returns up to 3 candidate field names from schema sorted by
// Levenshtein distance to the misspelled name.
func suggestOutputFields(misspelled string, schema map[string]ConfigField) []string {
	type candidate struct {
		name     string
		distance int
	}
	candidates := make([]candidate, 0, len(schema))
	for name := range schema {
		candidates = append(candidates, candidate{name: name, distance: levenshteinDistance(misspelled, name)})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].distance != candidates[j].distance {
			return candidates[i].distance < candidates[j].distance
		}
		return candidates[i].name < candidates[j].name
	})
	var out []string
	for i := 0; i < len(candidates) && i < 3; i++ {
		out = append(out, fmt.Sprintf("%q", candidates[i].name))
	}
	return out
}

// levenshteinDistance computes the edit distance between a and b.
func levenshteinDistance(a, b string) int {
	if a == "" {
		return len(b)
	}
	if b == "" {
		return len(a)
	}
	prev := make([]int, len(b)+1)
	for j := 0; j <= len(b); j++ {
		prev[j] = j
	}
	curr := make([]int, len(b)+1)
	for i := 1; i <= len(a); i++ {
		curr[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			curr[j] = min3(prev[j]+1, curr[j-1]+1, prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[len(b)]
}

func min3(a, b, c int) int {
	if a < b {
		if a < c {
			return a
		}
		return c
	}
	if b < c {
		return b
	}
	return c
}
