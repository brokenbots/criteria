package workflow

// compile_call_edges.go — CRI-157: adapter tool-call awareness in the M3
// compiler graph pass.
//
// buildAdapterCallEdges records every step-level `tools` grant as:
//   - a per-step AdapterToolRef list on the step's StepNode (runtime policy
//     consumers, CRI-159/CRI-160), and
//   - a generic AdapterCallEdge in FSMGraph.AdapterCallEdges (caller adapter,
//     callee adapter, tool name, provenance step name).
//
// The edge set is metadata only. It must not feed nodeTargets,
// checkReachability, or FSM routing (ADR-0004 §1: tool calls create no FSM
// nodes and no outcome plumbing), so reachable targets and transitions are
// identical with and without this pass.
//
// warnAdapterCallCycles detects cycles over the call-edge set and emits a
// compile WARNING: cycles are allowed and will be capped at runtime by
// policy.max_tool_depth (ADR-0004 §6). This mechanism is deliberately separate
// from the FSM back-edge warning (warnBackEdges) — the two walk different edge
// sets and carry different wording.

import (
	"fmt"
	"strings"

	"github.com/hashicorp/hcl/v2"
)

// buildAdapterCallEdges populates StepNode.Tools and g.AdapterCallEdges from
// the parse-captured tool-ref traversals (StepSpec.Tools). Entries that fail
// the adapter.<type>.<name>.tools[.<tool>] shape are skipped: they already
// carry a compile error from validateStepToolRefs (CRI-156 mode 1), and a
// graph with errors is discarded, so no edge is built for them.
//
// Subworkflow-targeted steps record Tools entries (CallerIsSelf always false,
// there is no caller adapter) but produce no AdapterCallEdge: an edge needs a
// caller adapter ref. Subworkflow-as-tool edge producers append their own
// edges later, reusing the same structure.
//
// Duplicate grants within one step — already flagged by the CRI-156 mode 6
// duplicate warning — are recorded once, in declaration order.
func buildAdapterCallEdges(g *FSMGraph) {
	if g == nil || g.spec == nil || len(g.stepOrder) == 0 {
		return
	}
	specs := stepSpecIndex(g.spec)
	for _, stepName := range g.stepOrder {
		node, sp := g.Steps[stepName], specs[stepName]
		if node == nil || sp == nil || len(sp.Tools) == 0 {
			continue
		}
		refs, edges := stepToolRefs(node, sp)
		if len(refs) > 0 {
			node.Tools = refs
		}
		if len(edges) > 0 {
			g.AdapterCallEdges = append(g.AdapterCallEdges, edges...)
		}
	}
}

// stepSpecIndex indexes StepSpecs by name for compile-order lookup.
func stepSpecIndex(spec *Spec) map[string]*StepSpec {
	byName := make(map[string]*StepSpec, len(spec.Steps))
	for i := range spec.Steps {
		byName[spec.Steps[i].Name] = &spec.Steps[i]
	}
	return byName
}

// stepToolRefs turns the step's parse-captured tool-ref traversals into its
// AdapterToolRef list plus any AdapterCallEdges it contributes (empty when the
// step does not target an adapter).
func stepToolRefs(node *StepNode, sp *StepSpec) ([]AdapterToolRef, []AdapterCallEdge) {
	refs := make([]AdapterToolRef, 0, len(sp.Tools))
	seen := make(map[[2]string]bool, len(sp.Tools))
	for _, tr := range sp.Tools {
		if toolRefShapeProblem(tr) != "" {
			continue
		}
		callee := tr[1].(hcl.TraverseAttr).Name + "." + tr[2].(hcl.TraverseAttr).Name
		var tool string
		if len(tr) == 5 {
			tool = tr[4].(hcl.TraverseAttr).Name
		}
		if seen[[2]string{callee, tool}] {
			continue
		}
		seen[[2]string{callee, tool}] = true
		refs = append(refs, AdapterToolRef{
			CallerIsSelf: node.AdapterRef != "" && callee == node.AdapterRef,
			CalleeRef:    callee,
			Tool:         tool,
		})
	}
	return refs, stepCallEdges(node, refs)
}

// stepCallEdges maps the recorded tool refs onto call edges; steps that do
// not target an adapter produce none.
func stepCallEdges(node *StepNode, refs []AdapterToolRef) []AdapterCallEdge {
	if node.TargetKind != StepTargetAdapter || node.AdapterRef == "" {
		return nil
	}
	edges := make([]AdapterCallEdge, 0, len(refs))
	for _, ref := range refs {
		edges = append(edges, AdapterCallEdge{
			CallerAdapterRef: node.AdapterRef,
			CalleeAdapterRef: ref.CalleeRef,
			Tool:             ref.Tool,
			StepName:         node.Name,
		})
	}
	return edges
}

// warnAdapterCallCycles walks the adapter call graph implied by
// g.AdapterCallEdges and emits one warning per distinct cycle (see
// adapterCycleWarnings for the policy). Self-grants are excluded by
// adapterCallAdjacency per ADR-0004 §10.
func warnAdapterCallCycles(g *FSMGraph) hcl.Diagnostics {
	if g == nil || len(g.AdapterCallEdges) == 0 {
		return nil
	}
	adj := adapterCallAdjacency(g)
	roots := adapterCycleRoots(g, adj)
	cycles := findCallCycles(adj, roots)
	return adapterCycleWarnings(cycles, g.Policy.MaxToolDepth)
}

// adapterCallAdjacency builds the deduplicated caller->callee adjacency from
// the call-edge set. Self-grants (callee == caller, ADR-0004 §10) are
// excluded: a self-call is rejected at runtime with a typed call_error and
// same-session reentry is out of scope for v1, so a self-grant is not a
// tool-call cycle. Self-grants stay in the edge set and on the step's Tools
// list (CallerIsSelf) for runtime consumers.
func adapterCallAdjacency(g *FSMGraph) map[string][]string {
	adj := make(map[string][]string)
	seenPair := make(map[[2]string]bool, len(g.AdapterCallEdges))
	for _, e := range g.AdapterCallEdges {
		if e.CallerAdapterRef == e.CalleeAdapterRef {
			continue
		}
		pair := [2]string{e.CallerAdapterRef, e.CalleeAdapterRef}
		if seenPair[pair] {
			continue
		}
		seenPair[pair] = true
		adj[e.CallerAdapterRef] = append(adj[e.CallerAdapterRef], e.CalleeAdapterRef)
	}
	return adj
}

// adapterCycleRoots orders DFS roots: adapters in declaration order first,
// then any remaining callers in first-seen edge order.
func adapterCycleRoots(g *FSMGraph, adj map[string][]string) []string {
	roots := make([]string, 0, len(g.AdapterOrder))
	roots = append(roots, g.AdapterOrder...)
	for _, e := range g.AdapterCallEdges {
		if _, ok := adj[e.CallerAdapterRef]; ok && !containsString(roots, e.CallerAdapterRef) {
			roots = append(roots, e.CallerAdapterRef)
		}
	}
	return roots
}

// findCallCycles runs a coloured DFS over the adjacency and returns the
// distinct node sequences of every cycle closed by a re-entry into an
// on-stack adapter, reported as the DFS stack from the re-entered adapter
// onward. The same cycle reached from different roots is found more than
// once; callers deduplicate by canonicalCycleKey.
func findCallCycles(adj map[string][]string, roots []string) [][]string {
	const (
		white = iota // unvisited
		gray         // on the current DFS stack
		black        // fully explored
	)
	color := make(map[string]int)
	var stack []string
	var cycles [][]string

	var dfs func(from string)
	dfs = func(from string) {
		color[from] = gray
		stack = append(stack, from)
		for _, to := range adj[from] {
			switch color[to] {
			case gray:
				cycles = append(cycles, stackCycle(stack, to))
			case white:
				dfs(to)
			}
		}
		stack = stack[:len(stack)-1]
		color[from] = black
	}
	for _, root := range roots {
		if color[root] == white {
			dfs(root)
		}
	}
	return cycles
}

// stackCycle extracts the cycle suffix of the DFS stack that begins at node.
func stackCycle(stack []string, entry string) []string {
	for i, n := range stack {
		if n == entry {
			cycle := append([]string{}, stack[i:]...)
			return cycle
		}
	}
	return nil
}

// adapterCycleWarnings emits one compile WARNING per distinct cycle. Cycles
// are allowed at compile time (ADR-0004 §6); enforcement is runtime-only via
// policy.max_tool_depth. The wording is deliberately distinct from the FSM
// back-edge warning (warnBackEdges) — the two walk different edge sets.
func adapterCycleWarnings(cycles [][]string, maxToolDepth int) hcl.Diagnostics {
	reported := make(map[string]bool, len(cycles))
	var diags hcl.Diagnostics
	for _, cycle := range cycles {
		key := canonicalCycleKey(cycle)
		if reported[key] {
			continue
		}
		reported[key] = true
		path := make([]string, 0, len(cycle)+1)
		path = append(path, cycle...)
		path = append(path, cycle[0])
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagWarning,
			Summary:  fmt.Sprintf("adapter tool-call cycle detected: %s", strings.Join(path, " -> ")),
			Detail: fmt.Sprintf(
				"tool edges form a call cycle through %d adapter(s). Cycles are allowed and are not rejected at "+
					"compile time; execution is bounded at runtime by policy.max_tool_depth (currently %d), and "+
					"exceeding the depth fails the tool call, not the run. This warning concerns adapter tool-call "+
					"edges and is separate from FSM back-edge diagnostics.",
				len(cycle), maxToolDepth),
		})
	}
	return diags
}

// canonicalCycleKey builds a rotation-invariant dedupe key for a cycle: the
// node sequence rotated to start at its lexicographically smallest adapter.
func canonicalCycleKey(cycle []string) string {
	start := 0
	for i, name := range cycle {
		if name < cycle[start] {
			start = i
		}
	}
	key := make([]string, 0, len(cycle))
	for i := 0; i < len(cycle); i++ {
		key = append(key, cycle[(start+i)%len(cycle)])
	}
	return strings.Join(key, "->")
}

// containsString reports whether list contains name.
func containsString(list []string, name string) bool {
	for _, n := range list {
		if n == name {
			return true
		}
	}
	return false
}
