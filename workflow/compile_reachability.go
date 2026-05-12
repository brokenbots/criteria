package workflow

import (
	"fmt"
	"strings"

	"github.com/hashicorp/hcl/v2"
)

// collectReachableNodes performs an iterative BFS from start and returns the
// set of all node names reachable through outgoing FSM edges.
func collectReachableNodes(g *FSMGraph, start string) map[string]bool {
	reachable := map[string]bool{start: true}
	queue := []string{start}
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		for _, target := range nodeTargets(name, g) {
			if !reachable[target] {
				reachable[target] = true
				queue = append(queue, target)
			}
		}
	}
	return reachable
}

// diagnoseUnreachableSteps emits DiagError for every step not in reachable.
func diagnoseUnreachableSteps(g *FSMGraph, reachable map[string]bool) hcl.Diagnostics {
	var diags hcl.Diagnostics
	for _, name := range g.stepOrder {
		if !reachable[name] {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("step %q is unreachable from initial_state", name),
			})
		}
	}
	return diags
}

// diagnoseUnreachableNodes emits DiagWarning for every wait, approval, switch,
// and non-synthetic state not in reachable.
func diagnoseUnreachableNodes(g *FSMGraph, reachable map[string]bool) hcl.Diagnostics {
	var diags hcl.Diagnostics
	for name := range g.Waits {
		if !reachable[name] {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagWarning,
				Summary:  fmt.Sprintf("wait %q is unreachable from initial_state", name),
			})
		}
	}
	for name := range g.Approvals {
		if !reachable[name] {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagWarning,
				Summary:  fmt.Sprintf("approval %q is unreachable from initial_state", name),
			})
		}
	}
	for name := range g.Switches {
		if !reachable[name] {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagWarning,
				Summary:  fmt.Sprintf("switch %q is unreachable from initial_state", name),
			})
		}
	}
	for name := range g.States {
		if strings.HasPrefix(name, "_") {
			// Synthetic states (e.g. _continue) are internal loop targets;
			// skipping them avoids spurious "unreachable" warnings.
			continue
		}
		if !reachable[name] {
			// Unreachable terminal states are a warning — they may be intentional placeholders.
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagWarning,
				Summary:  fmt.Sprintf("state %q is unreachable from initial_state", name),
			})
		}
	}
	return diags
}
