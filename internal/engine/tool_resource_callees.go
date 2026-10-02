package engine

import (
	"slices"

	"github.com/brokenbots/criteria/workflow"
)

// toolResourceCalleeNames returns the distinct callee adapter instance ids
// granted by any step's tools list in the body tree (KB-58), EXCLUDING the
// adapters a step runs against directly: those are bound by initScopeAdapters
// with a full per-scope session, and borrowing their verified records into a
// fresh iteration manager would hijack the child scope's per-scope
// provisioning and event stream. A granted adapter that is never a step
// target is a tool resource (e.g. an mcp server a caller probes): reachable
// only through the nested tool-call lazy bind, which a fresh per-iteration
// SessionManager cannot satisfy from its own state because no scope ever
// provisions it. Recursive so grants declared in nested subworkflow bodies
// are collected at the outer parallel seam too. Deterministic (sorted) order.
func toolResourceCalleeNames(body *workflow.FSMGraph) []string {
	if body == nil {
		return nil
	}
	targets := map[string]bool{}
	grants := map[string]bool{}
	seen := map[*workflow.FSMGraph]bool{}
	var walk func(g *workflow.FSMGraph)
	walk = func(g *workflow.FSMGraph) {
		if g == nil || seen[g] {
			return
		}
		seen[g] = true
		for _, step := range g.Steps {
			if step == nil {
				continue
			}
			if step.AdapterRef != "" {
				targets[step.AdapterRef] = true
			}
			for _, tr := range step.Tools {
				// CallerIsSelf grants are already covered by the targets
				// exclusion (the callee is the executing step's own adapter).
				if tr.CalleeRef != "" && tr.CalleeRef != step.AdapterRef {
					grants[tr.CalleeRef] = true
				}
			}
		}
		for _, sub := range g.Subworkflows {
			if sub != nil {
				walk(sub.Body)
			}
		}
	}
	walk(body)

	names := make([]string, 0, len(grants))
	for name := range grants {
		if !targets[name] {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}
