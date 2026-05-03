package workflow

// compile_locals.go — compile path for local "<name>" blocks.
// Locals are compile-time-resolved named values whose expressions must fold
// entirely within the (var ∪ local ∪ literal ∪ funcs) closure.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
)

// compileLocals folds every local.* declaration in declaration order,
// respecting inter-local dependencies (a later local may reference an earlier
// one). Cycles among locals are a compile error.
//
// Must be called after compileVariables so g.Variables is fully populated.
func compileLocals(g *FSMGraph, spec *Spec, opts CompileOpts) hcl.Diagnostics {
	if len(spec.Locals) == 0 {
		return nil
	}

	var diags hcl.Diagnostics

	idx, d := buildLocalIndex(spec)
	diags = append(diags, d...)
	if diags.HasErrors() {
		return diags
	}

	valueExprs := extractLocalValueExprs(spec)

	inDegree, reverseDeps := buildLocalDepGraph(spec, idx, valueExprs)

	order, d := topoSortLocals(spec, inDegree, reverseDeps)
	diags = append(diags, d...)
	if diags.HasErrors() {
		return diags
	}

	diags = append(diags, compileLocalNodes(g, spec, opts, order)...)
	return diags
}

// buildLocalIndex builds a name → declaration-index map and errors on duplicates.
func buildLocalIndex(spec *Spec) (map[string]int, hcl.Diagnostics) {
	var diags hcl.Diagnostics
	idx := make(map[string]int, len(spec.Locals))
	for i, ls := range spec.Locals {
		if _, dup := idx[ls.Name]; dup {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("duplicate local %q", ls.Name),
			})
			continue
		}
		idx[ls.Name] = i
	}
	return idx, diags
}

// extractLocalValueExprs returns the "value" expression for each local (nil if absent).
// Attribute parsing errors are silently dropped here; they are re-reported
// during the compilation pass in compileLocalNodes.
func extractLocalValueExprs(spec *Spec) []hcl.Expression {
	exprs := make([]hcl.Expression, len(spec.Locals))
	for i, ls := range spec.Locals {
		attrs, _ := ls.Remain.JustAttributes()
		if valAttr, ok := attrs["value"]; ok {
			exprs[i] = valAttr.Expr
		}
	}
	return exprs
}

// buildLocalDepGraph computes the in-degree and reverse-dependency lists for
// Kahn's topological sort.
func buildLocalDepGraph(spec *Spec, idx map[string]int, exprs []hcl.Expression) (inDegree []int, reverseDeps [][]int) {
	inDegree = make([]int, len(spec.Locals))
	reverseDeps = make([][]int, len(spec.Locals))
	for i, expr := range exprs {
		if expr == nil {
			continue
		}
		seen := map[int]bool{}
		for _, traversal := range expr.Variables() {
			addLocalDep(i, traversal, idx, seen, inDegree, reverseDeps)
		}
	}
	return inDegree, reverseDeps
}

// addLocalDep records a single dependency edge if traversal is a local.* reference.
func addLocalDep(i int, traversal hcl.Traversal, idx map[string]int, seen map[int]bool, inDegree []int, reverseDeps [][]int) {
	if len(traversal) < 2 {
		return
	}
	root, ok := traversal[0].(hcl.TraverseRoot)
	if !ok || root.Name != "local" {
		return
	}
	attr, ok := traversal[1].(hcl.TraverseAttr)
	if !ok {
		return
	}
	j, ok := idx[attr.Name]
	if !ok {
		return // unknown local — FoldExpr will report the error
	}
	if i == j {
		inDegree[i]++ // self-reference: cycle of size 1
		seen[j] = true
		return
	}
	if !seen[j] {
		seen[j] = true
		inDegree[i]++
		reverseDeps[j] = append(reverseDeps[j], i)
	}
}

// topoSortLocals runs Kahn's algorithm and returns an error diagnostic if a
// cycle is detected.
func topoSortLocals(spec *Spec, inDegree []int, reverseDeps [][]int) ([]int, hcl.Diagnostics) {
	ready := make([]int, 0, len(spec.Locals))
	for i := range spec.Locals {
		if inDegree[i] == 0 {
			ready = append(ready, i)
		}
	}

	order := make([]int, 0, len(spec.Locals))
	for len(ready) > 0 {
		i := ready[0]
		ready = ready[1:]
		order = append(order, i)
		for _, dep := range reverseDeps[i] {
			inDegree[dep]--
			if inDegree[dep] == 0 {
				ready = append(ready, dep)
				sort.Ints(ready)
			}
		}
	}

	if len(order) < len(spec.Locals) {
		processed := make(map[int]bool, len(order))
		for _, i := range order {
			processed[i] = true
		}
		var cycleNames []string
		for i, ls := range spec.Locals {
			if !processed[i] {
				cycleNames = append(cycleNames, fmt.Sprintf("%q", ls.Name))
			}
		}
		sort.Strings(cycleNames)
		return nil, hcl.Diagnostics{&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("local cycle detected involving: %s", strings.Join(cycleNames, ", ")),
		}}
	}
	return order, nil
}

// compileLocalNodes evaluates each local in topological order and stores the
// result in g.Locals.
func compileLocalNodes(g *FSMGraph, spec *Spec, opts CompileOpts, order []int) hcl.Diagnostics {
	var diags hcl.Diagnostics
	for _, i := range order {
		ls := spec.Locals[i]

		attrs, d := ls.Remain.JustAttributes()
		diags = append(diags, d...)

		for k, attr := range attrs {
			if k != "value" {
				r := attr.NameRange
				diags = append(diags, &hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  fmt.Sprintf("local %q: unknown attribute %q; only \"value\" is allowed", ls.Name, k),
					Subject:  &r,
				})
			}
		}

		valAttr, ok := attrs["value"]
		if !ok {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("local %q: required attribute \"value\" is missing", ls.Name),
			})
			continue
		}

		d = compileOneLocal(g, ls, valAttr.Expr, opts)
		diags = append(diags, d...)
	}
	return diags
}

// compileOneLocal folds a single local's value expression and stores the result.
func compileOneLocal(g *FSMGraph, ls LocalSpec, expr hcl.Expression, opts CompileOpts) hcl.Diagnostics {
	val, foldable, diags := FoldExpr(expr, graphVars(g), graphLocals(g), opts.WorkflowDir)
	if !foldable {
		r := expr.StartRange()
		return append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("local %q: value must be a compile-time constant expression; references to each, steps, or shared_variable are not allowed", ls.Name),
			Subject:  &r,
		})
	}
	if diags.HasErrors() {
		return diags
	}
	g.Locals[ls.Name] = &LocalNode{
		Name:        ls.Name,
		Type:        val.Type(),
		Value:       val,
		Description: ls.Description,
	}
	return diags
}
