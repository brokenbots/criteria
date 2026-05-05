package workflow

// compile_steps_graph.go — FSM graph traversal and node construction helpers
// used by every step-kind compiler: back-edge detection, reachable-target
// enumeration, outcome compilation, and node constructors.

import (
	"fmt"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
)

// compileOutcomeBlock populates node.Outcomes from sp.Outcomes. It validates:
//   - no duplicate outcome names
//   - "next" is present (non-empty)
//   - "return" is not used as a step name (reserved sentinel)
//   - default_outcome, if set, refers to a declared outcome
//   - the optional "output" expression, when present, references only known
//     vars/locals (runtime-only refs like steps.* are deferred, not errors)
//     and, when foldable at compile time, evaluates to an object type.
//
// The optional "output" expression is extracted from the outcome's Remain body
// and stored in CompiledOutcome.OutputExpr.
func compileOutcomeBlock(sp *StepSpec, node *StepNode, g *FSMGraph, opts CompileOpts) hcl.Diagnostics {
	var diags hcl.Diagnostics
	seen := map[string]bool{}
	for _, o := range sp.Outcomes {
		if seen[o.Name] {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("step %q: duplicate outcome %q", sp.Name, o.Name)})
			continue
		}
		seen[o.Name] = true
		if o.Next == "" {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("step %q outcome %q: next is required", sp.Name, o.Name)})
			continue
		}
		compiled := &CompiledOutcome{Name: o.Name, Next: o.Next}
		if o.Remain != nil {
			content, _, cDiags := o.Remain.PartialContent(&hcl.BodySchema{
				Attributes: []hcl.AttributeSchema{{Name: "output", Required: false}},
			})
			diags = append(diags, cDiags...)
			if attr, ok := content.Attributes["output"]; ok {
				compiled.OutputExpr = attr.Expr
				diags = append(diags, validateOutcomeOutputExpr(sp.Name, o.Name, attr, g, opts)...)
			}
		}
		node.Outcomes[o.Name] = compiled
	}

	// Validate default_outcome refers to a declared outcome.
	if sp.DefaultOutcome != "" {
		if !seen[sp.DefaultOutcome] {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("step %q: default_outcome %q is not a declared outcome", sp.Name, sp.DefaultOutcome),
			})
		} else {
			node.DefaultOutcome = sp.DefaultOutcome
		}
	}

	return diags
}

// validateOutcomeOutputExpr validates the output = { ... } expression on an
// outcome block. It:
//  1. Checks for unknown var/local references using validateFoldableAttrs
//     (runtime-only namespaces like "steps" and "each" are silently deferred).
//  2. When the expression is foldable at compile time (no runtime refs), verifies
//     that the result is an object type so non-object literals (e.g. strings)
//     are caught at compile time.
func validateOutcomeOutputExpr(stepName, outcomeName string, attr *hcl.Attribute, g *FSMGraph, opts CompileOpts) hcl.Diagnostics {
	// Step 1: check for unresolvable free-variable references.
	refDiags := validateFoldableAttrs(hcl.Attributes{attr.Name: attr}, graphVars(g), graphLocals(g), opts.WorkflowDir)
	if refDiags.HasErrors() {
		return refDiags
	}

	// Step 2: if foldable at compile time, validate the type is an object.
	val, foldable, foldDiags := FoldExpr(attr.Expr, graphVars(g), graphLocals(g), opts.WorkflowDir)
	if foldDiags.HasErrors() {
		return foldDiags
	}
	if !foldable {
		// Expression contains runtime-only refs — defer to runtime evaluation.
		return nil
	}
	if val == cty.NilVal || !val.IsKnown() {
		return nil
	}
	if !val.Type().IsObjectType() && val.Type() != cty.DynamicPseudoType {
		r := attr.Expr.StartRange()
		return hcl.Diagnostics{&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("step %q outcome %q: output must be an object literal; got %s", stepName, outcomeName, val.Type().FriendlyName()),
			Subject:  &r,
		}}
	}
	return nil
}

// validateStepNameNotReturn errors when a step is named "return" since that
// string is the reserved outcome routing sentinel.
func validateStepNameNotReturn(sp *StepSpec) hcl.Diagnostics {
	if sp.Name == ReturnSentinel {
		return hcl.Diagnostics{&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  `step "return": "return" is a reserved name; steps cannot be named "return"`,
			Detail:   `The name "return" is reserved as a sentinel for outcome routing (next = "return"). Choose a different step name.`,
		}}
	}
	return nil
}

//   - has a back-edge (a path in the outcome graph that leads back to itself), AND
//   - has no max_visits set (MaxVisits == 0), AND
//   - the workflow's max_total_steps exceeds the MaxVisitsWarnThreshold (and
//     the threshold is non-zero).
//
// The check runs after compileSteps so all outcome targets are populated.
// A DFS from each step's outcomes is used to detect back-edges; visited nodes
// are tracked to keep the walk O(N) per step.
func warnBackEdges(g *FSMGraph) hcl.Diagnostics {
	threshold := g.Policy.MaxVisitsWarnThreshold
	if threshold == 0 {
		// Warning disabled via max_visits_warn_threshold = 0.
		return nil
	}
	if g.Policy.MaxTotalSteps <= threshold {
		// max_total_steps is within the threshold; no warning needed.
		return nil
	}

	var diags hcl.Diagnostics
	for _, name := range g.stepOrder {
		step := g.Steps[name]
		if step.MaxVisits != 0 {
			continue // already bounded; no warning
		}
		if stepHasBackEdge(name, g) {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagWarning,
				Summary: fmt.Sprintf(
					"step %q: appears in a loop with max_total_steps=%d and no max_visits; consider setting max_visits to bound back-edge iteration",
					name, g.Policy.MaxTotalSteps,
				),
			})
		}
	}
	return diags
}

// nodeTargets returns the names of all FSM nodes that the named node can
// transition to. Recognises steps, branches, waits, and approvals; returns
// nil for unknown nodes (state nodes have no outgoing edges and are dead-ends).
// The "_continue" and "return" pseudo-targets are excluded because they are
// never real nodes.
func nodeTargets(name string, g *FSMGraph) []string {
	if step, ok := g.Steps[name]; ok {
		var targets []string
		for _, co := range step.Outcomes {
			if co.Next != "_continue" && co.Next != ReturnSentinel {
				targets = append(targets, co.Next)
			}
		}
		return targets
	}
	if branch, ok := g.Branches[name]; ok {
		targets := make([]string, 0, len(branch.Arms)+1)
		for _, arm := range branch.Arms {
			targets = append(targets, arm.Target)
		}
		if branch.DefaultTarget != "" {
			targets = append(targets, branch.DefaultTarget)
		}
		return targets
	}
	if wait, ok := g.Waits[name]; ok {
		targets := make([]string, 0, len(wait.Outcomes))
		for _, t := range wait.Outcomes {
			targets = append(targets, t)
		}
		return targets
	}
	if approval, ok := g.Approvals[name]; ok {
		targets := make([]string, 0, len(approval.Outcomes))
		for _, t := range approval.Outcomes {
			targets = append(targets, t)
		}
		return targets
	}
	return nil
}

// stepHasBackEdge reports whether the named step can reach itself via outcome
// transitions (i.e. it is part of a cycle in the FSM graph). The walk follows
// edges through all node kinds — steps, branches, waits, and approvals — so
// that loops mediated by non-step nodes are also detected. StateNodes have no
// outgoing edges and are treated as dead-ends.
func stepHasBackEdge(startName string, g *FSMGraph) bool {
	if _, ok := g.Steps[startName]; !ok {
		return false
	}
	visited := map[string]bool{}
	var walk func(name string) bool
	walk = func(name string) bool {
		if name == startName {
			return true
		}
		if visited[name] {
			return false
		}
		visited[name] = true
		for _, target := range nodeTargets(name, g) {
			if walk(target) {
				return true
			}
		}
		return false
	}
	for _, target := range nodeTargets(startName, g) {
		if walk(target) {
			return true
		}
	}
	return false
}
