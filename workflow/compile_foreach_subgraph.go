package workflow

// compile_foreach_subgraph.go — compile-time iteration-subgraph computation
// and well-formedness validation for for_each nodes (W08).
//
// The iteration subgraph of a for_each node F with do = "S" is computed in
// two phases. Phase 1 (forwardReachableSteps): BFS from S over step-to-step
// outcome transitions, stopping at _continue, F.Name (legacy advance), or any
// non-step target. Phase 2 (filterByContinueReachable): restricts to the
// subset of Phase-1 steps that can reach _continue.
//
// Well-formedness has two levels:
//   - Loop level (validateOneForEach): F.Do must be in IterationSteps,
//     meaning at least one path from "do" must reach _continue. A loop whose
//     "do" step only exits to external steps (no _continue path) is invalid.
//   - Step level (validateSubgraphWellFormedness): each step in IterationSteps
//     must individually have an outcome that reaches _continue or exits to an
//     external step.
//
// computeIterationSubgraphs must be called from CompileWithOpts after all
// node maps have been populated (steps, for_each nodes, states, etc.).

import (
	"fmt"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
)

// computeIterationSubgraphs computes the iteration subgraph for every for_each
// node, validates well-formedness, and tags each step in a subgraph with its
// owning for_each name. It must be called after all nodes are compiled.
func computeIterationSubgraphs(g *FSMGraph) hcl.Diagnostics {
	var diags hcl.Diagnostics

	// First pass: compute IterationSteps for each for_each node.
	for _, fe := range g.ForEachs {
		fe.IterationSteps = buildIterationSubgraph(g, fe)
	}

	// Second pass: validate well-formedness and detect overlap.
	// Process for_each nodes in stable order for deterministic diagnostics.
	names := sortedForEachNames(g)
	for _, feName := range names {
		diags = append(diags, validateOneForEach(g, g.ForEachs[feName])...)
	}

	// Third pass: tag each step with its owning for_each; reject overlaps.
	for _, feName := range names {
		diags = append(diags, tagIterationOwners(g, g.ForEachs[feName], feName)...)
	}

	return diags
}

// sortedForEachNames returns the for_each node names in sorted order.
func sortedForEachNames(g *FSMGraph) []string {
	names := make([]string, 0, len(g.ForEachs))
	for n := range g.ForEachs {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// validateOneForEach validates the subgraph for a single for_each node.
func validateOneForEach(g *FSMGraph, fe *ForEachNode) hcl.Diagnostics {
	// If the do-step is not in the subgraph, the iteration body has no path to
	// _continue; the loop can never advance. Emit errors for all
	// forward-reachable steps to help the author identify the problem.
	if _, ok := fe.IterationSteps[fe.Do]; !ok && g.Steps[fe.Do] != nil {
		return doStepNotReachableDiags(g, fe)
	}
	return validateSubgraphWellFormedness(g, fe)
}

// doStepNotReachableDiags emits "no outcome path" diagnostics for all steps
// reachable from fe.Do when none of them can reach _continue.
func doStepNotReachableDiags(g *FSMGraph, fe *ForEachNode) hcl.Diagnostics {
	tentative := forwardReachableSteps(g, fe)
	body := strings.Join(sortedKeys(tentative), ", ")
	diags := make(hcl.Diagnostics, 0, len(tentative))
	for _, stepName := range sortedKeys(tentative) {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary: fmt.Sprintf(
				"for_each %q: iteration step %q has no outcome path that reaches "+
					"_continue or transitions out of the iteration body.\n"+
					"  Iteration body: %s\n"+
					"  Suggested fix: add an outcome to %q with transition_to = \"_continue\".",
				fe.Name, stepName, body, stepName,
			),
		})
	}
	return diags
}

// tagIterationOwners sets IterationOwner on each step in fe.IterationSteps and
// emits an error if a step is already owned by a different for_each.
func tagIterationOwners(g *FSMGraph, fe *ForEachNode, feName string) hcl.Diagnostics {
	var diags hcl.Diagnostics
	for stepName := range fe.IterationSteps {
		step, ok := g.Steps[stepName]
		if !ok {
			continue
		}
		if step.IterationOwner != "" && step.IterationOwner != feName {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary: fmt.Sprintf(
					"step %q belongs to both for_each %q and for_each %q iteration bodies; "+
						"steps cannot be shared between distinct for_each subgraphs",
					stepName, step.IterationOwner, feName,
				),
			})
		} else {
			step.IterationOwner = feName
		}
	}
	return diags
}

// forwardReachableSteps performs Phase 1 of the subgraph computation: a
// forward BFS from fe.Do following step-to-step outcome transitions. It
// returns ALL steps reachable from fe.Do, including early-exit destinations
// that cannot reach _continue. Used for error reporting.
func forwardReachableSteps(g *FSMGraph, fe *ForEachNode) map[string]struct{} {
	tentative := make(map[string]struct{})
	visited := make(map[string]bool)
	maxDepth := len(g.Steps) + 1

	var walk func(stepName string, depth int)
	walk = func(stepName string, depth int) {
		if depth > maxDepth || visited[stepName] {
			return
		}
		visited[stepName] = true
		if _, ok := g.Steps[stepName]; !ok {
			return
		}
		tentative[stepName] = struct{}{}
		step := g.Steps[stepName]
		for _, target := range step.Outcomes {
			if target == "_continue" || target == fe.Name {
				continue
			}
			if _, isStep := g.Steps[target]; isStep {
				walk(target, depth+1)
			}
		}
	}
	if _, ok := g.Steps[fe.Do]; ok {
		walk(fe.Do, 0)
	}
	return tentative
}

// buildIterationSubgraph computes the set of steps that form the iteration
// body of fe. It delegates to forwardReachableSteps (Phase 1) then filters to
// only steps that can reach _continue (Phase 2).
func buildIterationSubgraph(g *FSMGraph, fe *ForEachNode) map[string]struct{} {
	tentative := forwardReachableSteps(g, fe)
	return filterByContinueReachable(g, fe, tentative)
}

// isIterAdvanceTarget returns true when target ends the iteration walk:
// either the canonical _continue token or the legacy for_each node name.
func isIterAdvanceTarget(target, feName string) bool {
	return target == "_continue" || target == feName
}

// filterByContinueReachable returns the subset of tentative steps that can
// reach _continue or fe.Name via transitions through other tentative steps.
// Steps that are reachable from fe.Do but have no path to _continue are
// early-exit destinations and are excluded from the subgraph.
func filterByContinueReachable(g *FSMGraph, fe *ForEachNode, tentative map[string]struct{}) map[string]struct{} {
	// Seed: steps with a direct _continue (or legacy fe.Name) outcome.
	canReach := make(map[string]bool, len(tentative))
	for stepName := range tentative {
		step := g.Steps[stepName]
		for _, target := range step.Outcomes {
			if isIterAdvanceTarget(target, fe.Name) {
				canReach[stepName] = true
				break
			}
		}
	}
	// Propagate: if a step reaches a can-reach step it also can reach _continue.
	propagateReachability(g, tentative, canReach)

	result := make(map[string]struct{}, len(canReach))
	for stepName := range tentative {
		if canReach[stepName] {
			result[stepName] = struct{}{}
		}
	}
	return result
}

// propagateReachability propagates the canReach flag through step transitions
// until a fixed point: if step A transitions to step B and canReach[B] is set,
// then canReach[A] is set too.
func propagateReachability(g *FSMGraph, members map[string]struct{}, canReach map[string]bool) {
	for changed := true; changed; {
		changed = false
		for stepName := range members {
			if canReach[stepName] {
				continue
			}
			step := g.Steps[stepName]
			for _, target := range step.Outcomes {
				if canReach[target] {
					canReach[stepName] = true
					changed = true
					break
				}
			}
		}
	}
}

// validateSubgraphWellFormedness validates that every step in fe's iteration
// subgraph has at least one outcome path that reaches _continue (advance) or
// transitions to an external step (early-exit). Steps that can only transition
// to terminal states are a structural error: the iteration can never advance.
func validateSubgraphWellFormedness(g *FSMGraph, fe *ForEachNode) hcl.Diagnostics {
	canExit := seedCanExit(g, fe)
	propagateReachability(g, fe.IterationSteps, canExit)
	return emitWellFormednessErrors(fe, canExit)
}

// seedCanExit marks steps that have a direct valid exit edge: _continue,
// the legacy fe.Name advance, or a transition to an external step (early-exit).
func seedCanExit(g *FSMGraph, fe *ForEachNode) map[string]bool {
	canExit := make(map[string]bool, len(fe.IterationSteps))
	for stepName := range fe.IterationSteps {
		step, ok := g.Steps[stepName]
		if !ok {
			continue
		}
		for _, target := range step.Outcomes {
			if isIterAdvanceTarget(target, fe.Name) {
				canExit[stepName] = true
				break
			}
			// An external step (not a state, not in this subgraph) is a valid early-exit.
			if _, inSubgraph := fe.IterationSteps[target]; !inSubgraph {
				if _, isStep := g.Steps[target]; isStep {
					canExit[stepName] = true
					break
				}
			}
		}
	}
	return canExit
}

// emitWellFormednessErrors emits a diagnostic for each step in the subgraph
// that cannot reach any valid exit edge.
func emitWellFormednessErrors(fe *ForEachNode, canExit map[string]bool) hcl.Diagnostics {
	var diags hcl.Diagnostics
	sortedBody := sortedKeys(fe.IterationSteps)
	bodyStr := strings.Join(sortedBody, ", ")
	for _, stepName := range sortedBody {
		if canExit[stepName] {
			continue
		}
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary: fmt.Sprintf(
				"for_each %q: iteration step %q has no outcome path that reaches "+
					"_continue or transitions out of the iteration body.\n"+
					"  Iteration body: %s\n"+
					"  Suggested fix: add an outcome to %q with transition_to = \"_continue\".",
				fe.Name, stepName, bodyStr, stepName,
			),
		})
	}
	return diags
}

// validateEachReferenceScope checks that no step outside any for_each
// iteration subgraph contains an expression that references each.* attributes.
// Such references would be erroneous since each.* is only bound during
// for_each iteration (W08).
func validateEachReferenceScope(g *FSMGraph) hcl.Diagnostics {
	var diags hcl.Diagnostics
	for _, step := range g.Steps {
		if step.IterationOwner != "" {
			// This step is inside a subgraph — each.* references are valid.
			continue
		}
		for _, expr := range step.InputExprs {
			for _, traversal := range expr.Variables() {
				if len(traversal) == 0 {
					continue
				}
				root, ok := traversal[0].(hcl.TraverseRoot)
				if !ok {
					continue
				}
				if root.Name == "each" {
					r := expr.StartRange()
					diags = append(diags, &hcl.Diagnostic{
						Severity: hcl.DiagError,
						Summary: fmt.Sprintf(
							"step %q references each.* but is not part of any for_each iteration body; "+
								"each.value and each.index are only bound during for_each iteration",
							step.Name,
						),
						Subject: &r,
					})
					// One diagnostic per expression is enough.
					break
				}
			}
		}
	}
	return diags
}

// sortedKeys returns the keys of a map[string]struct{} in sorted order.
func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
