package workflow

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
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
				Detail:   fmt.Sprintf("no transition path exists from initial_state %q to step %q", g.InitialState, name),
			})
		}
	}
	return diags
}

// diagnoseUnreachableNodes emits DiagError for every unreachable wait, approval,
// and switch (active control-flow nodes whose presence is always intentional) and
// DiagWarning for every non-synthetic state (which may be a placeholder terminal).
func diagnoseUnreachableNodes(g *FSMGraph, reachable map[string]bool) hcl.Diagnostics {
	var diags hcl.Diagnostics
	for _, name := range sortedMapKeys(g.Waits) {
		if !reachable[name] {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("wait %q is unreachable from initial_state", name),
				Detail:   fmt.Sprintf("no transition path exists from initial_state %q to wait %q", g.InitialState, name),
			})
		}
	}
	for _, name := range sortedMapKeys(g.Approvals) {
		if !reachable[name] {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("approval %q is unreachable from initial_state", name),
				Detail:   fmt.Sprintf("no transition path exists from initial_state %q to approval %q", g.InitialState, name),
			})
		}
	}
	for _, name := range sortedMapKeys(g.Switches) {
		if !reachable[name] {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("switch %q is unreachable from initial_state", name),
				Detail:   fmt.Sprintf("no transition path exists from initial_state %q to switch %q", g.InitialState, name),
			})
		}
	}
	for _, name := range sortedMapKeys(g.States) {
		if strings.HasPrefix(name, "_") || name == g.TargetState {
			// Synthetic states (e.g. _continue) are internal loop targets;
			// target_state is checked as a hard error by checkTargetStateReachability.
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

// sortedMapKeys returns the keys of m in ascending lexical order.
func sortedMapKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// nodeHasReturnEdge reports whether the named node has a direct outgoing
// transition to ReturnSentinel ("return").
func nodeHasReturnEdge(name string, g *FSMGraph) bool {
	if step, ok := g.Steps[name]; ok {
		for _, co := range step.Outcomes {
			if co.Next == ReturnSentinel {
				return true
			}
		}
		if step.DefaultOutcome != nil && step.DefaultOutcome.Next == ReturnSentinel {
			return true
		}
	}
	if sw, ok := g.Switches[name]; ok {
		for _, cond := range sw.Conditions {
			if cond.Next == ReturnSentinel {
				return true
			}
		}
		if sw.DefaultNext == ReturnSentinel {
			return true
		}
	}
	return false
}

// checkLivelocks performs backward reachability (co-accessibility) from all
// terminal exit points (terminal states and ReturnSentinel transitions) over
// the reverse adjacency graph. Any forward-reachable node that cannot reach at
// least one terminal exit is flagged with a deterministic cycle or dead-end
// counterexample trace.
func checkLivelocks(g *FSMGraph, reachable map[string]bool) hcl.Diagnostics {
	revAdj := make(map[string][]string)
	for u := range reachable {
		for _, v := range nodeTargets(u, g) {
			revAdj[v] = append(revAdj[v], u)
		}
	}
	for v := range revAdj {
		sort.Strings(revAdj[v])
	}

	canReachTerminal := make(map[string]bool)
	var queue []string
	for _, name := range sortedMapKeys(g.States) {
		if g.States[name].Terminal || g.States[name].Requires != "" {
			canReachTerminal[name] = true
			queue = append(queue, name)
		}
	}
	for _, u := range orderedReachableNodes(g, reachable) {
		if nodeHasReturnEdge(u, g) && !canReachTerminal[u] {
			canReachTerminal[u] = true
			queue = append(queue, u)
		}
	}

	for len(queue) > 0 {
		curr := queue[0]
		queue = queue[1:]
		for _, pred := range revAdj[curr] {
			if !canReachTerminal[pred] {
				canReachTerminal[pred] = true
				queue = append(queue, pred)
			}
		}
	}

	var diags hcl.Diagnostics
	for _, u := range orderedReachableNodes(g, reachable) {
		if canReachTerminal[u] {
			continue
		}
		kind, _ := g.Lookup(u)
		trace, isCycle := reconstructLivelockTrace(u, g)
		traceStr := strings.Join(trace, " -> ")
		if isCycle {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("livelock detected: %s %q enters cycle [%s] with no exit path to any terminal state", kind, u, traceStr),
			})
		} else {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("livelock detected: %s %q reaches dead-end [%s] with no exit path to any terminal state", kind, u, traceStr),
			})
		}
	}
	return diags
}

// orderedReachableNodes returns all forward-reachable nodes in a deterministic
// order: steps in declaration order, followed by switches, waits, approvals,
// and states in lexical order.
func orderedReachableNodes(g *FSMGraph, reachable map[string]bool) []string {
	var out []string
	for _, name := range g.stepOrder {
		if reachable[name] {
			out = append(out, name)
		}
	}
	for _, name := range sortedMapKeys(g.Switches) {
		if reachable[name] {
			out = append(out, name)
		}
	}
	for _, name := range sortedMapKeys(g.Waits) {
		if reachable[name] {
			out = append(out, name)
		}
	}
	for _, name := range sortedMapKeys(g.Approvals) {
		if reachable[name] {
			out = append(out, name)
		}
	}
	for _, name := range sortedMapKeys(g.States) {
		if reachable[name] {
			out = append(out, name)
		}
	}
	return out
}

// reconstructLivelockTrace reconstructs a deterministic cycle or dead-end trace
// starting from a trapped node u (a node that cannot reach any terminal state).
func reconstructLivelockTrace(u string, g *FSMGraph) ([]string, bool) {
	if cycle := shortestCycleFrom(u, g); len(cycle) > 0 {
		return cycle, true
	}

	visited := map[string]int{u: 0}
	path := []string{u}
	curr := u
	for {
		targets := nodeTargets(curr, g)
		if len(targets) == 0 {
			return path, false
		}
		next := targets[0]
		if idx, seen := visited[next]; seen {
			prefix := append([]string(nil), path[:idx]...)
			if cycle := shortestCycleFrom(next, g); len(cycle) > 0 {
				return append(prefix, cycle...), true
			}
			return append(path, next), true
		}
		visited[next] = len(path)
		path = append(path, next)
		curr = next
	}
}

// shortestCycleFrom finds the shortest directed cycle starting and ending at
// start using BFS. Returns nil if start cannot reach itself.
func shortestCycleFrom(start string, g *FSMGraph) []string {
	parent := make(map[string]string)
	visited := make(map[string]bool)
	var queue []string

	for _, next := range nodeTargets(start, g) {
		if next == start {
			return []string{start, start}
		}
		if !visited[next] {
			visited[next] = true
			parent[next] = start
			queue = append(queue, next)
		}
	}

	for len(queue) > 0 {
		curr := queue[0]
		queue = queue[1:]
		for _, next := range nodeTargets(curr, g) {
			if next == start {
				var cycle []string
				for p := curr; p != start; p = parent[p] {
					cycle = append(cycle, p)
				}
				cycle = append(cycle, start)
				for i, j := 0, len(cycle)-1; i < j; i, j = i+1, j-1 {
					cycle[i], cycle[j] = cycle[j], cycle[i]
				}
				cycle = append(cycle, start)
				return cycle
			}
			if !visited[next] {
				visited[next] = true
				parent[next] = curr
				queue = append(queue, next)
			}
		}
	}
	return nil
}

// checkTargetStateReachability verifies that g.TargetState (or ReturnSentinel)
// is reachable from g.InitialState. Returns diagnostics and true when reachable.
func checkTargetStateReachability(g *FSMGraph, reachable map[string]bool) (hcl.Diagnostics, bool) {
	if g.TargetState == "" {
		return nil, false
	}
	if reachable[g.TargetState] {
		return nil, true
	}
	for u := range reachable {
		if nodeHasReturnEdge(u, g) {
			return nil, true
		}
	}

	detail := fmt.Sprintf("no transition path exists from initial_state %q to target_state %q", g.InitialState, g.TargetState)
	if altPath := shortestPathToAnyTerminal(g, g.InitialState); len(altPath) > 0 {
		detail = fmt.Sprintf("all paths from initial_state %q terminate at non-target states (e.g. [%s]); no happy path to target_state %q exists",
			g.InitialState, strings.Join(altPath, " -> "), g.TargetState)
	}

	return hcl.Diagnostics{{
		Severity: hcl.DiagError,
		Summary:  fmt.Sprintf("target_state %q is unreachable from initial_state %q: no happy path to target_state exists", g.TargetState, g.InitialState),
		Detail:   detail,
	}}, false
}

// shortestPathToAnyTerminal returns the shortest BFS node path from start to
// any declared terminal state, or nil if none is reachable.
func shortestPathToAnyTerminal(g *FSMGraph, start string) []string {
	if st, ok := g.States[start]; ok && st.Terminal {
		return []string{start}
	}
	parent := make(map[string]string)
	visited := map[string]bool{start: true}
	queue := []string{start}
	for len(queue) > 0 {
		curr := queue[0]
		queue = queue[1:]
		for _, next := range nodeTargets(curr, g) {
			if visited[next] {
				continue
			}
			visited[next] = true
			parent[next] = curr
			if st, ok := g.States[next]; ok && st.Terminal {
				return reconstructBFSPath(parent, start, next)
			}
			queue = append(queue, next)
		}
	}
	return nil
}

func reconstructBFSPath(parent map[string]string, start, end string) []string {
	var path []string
	for curr := end; curr != start; curr = parent[curr] {
		path = append(path, curr)
	}
	path = append(path, start)
	for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
		path[i], path[j] = path[j], path[i]
	}
	return path
}

// weightedEdge represents a directed FSM transition with static step and visit costs.
type weightedEdge struct {
	to        string
	stepCost  int
	visitCost int
}

// outgoingWeightedEdges enumerates all non-_continue transitions from node u
// along with their minimum static step and visit costs.
func outgoingWeightedEdges(u string, g *FSMGraph) []weightedEdge {
	if step, ok := g.Steps[u]; ok {
		names := sortedMapKeys(step.Outcomes)
		var edges []weightedEdge
		for _, name := range names {
			co := step.Outcomes[name]
			if co.Next == "_continue" {
				continue
			}
			sc, vc := stepOutcomeCosts(step, co, g)
			edges = append(edges, weightedEdge{to: co.Next, stepCost: sc, visitCost: vc})
		}
		if step.DefaultOutcome != nil && step.DefaultOutcome.Next != "_continue" {
			sc, vc := stepOutcomeCosts(step, step.DefaultOutcome, g)
			edges = append(edges, weightedEdge{to: step.DefaultOutcome.Next, stepCost: sc, visitCost: vc})
		}
		return edges
	}
	if sw, ok := g.Switches[u]; ok {
		var edges []weightedEdge
		for _, cond := range sw.Conditions {
			edges = append(edges, weightedEdge{to: cond.Next, stepCost: 0, visitCost: 0})
		}
		if sw.DefaultNext != "" {
			edges = append(edges, weightedEdge{to: sw.DefaultNext, stepCost: 0, visitCost: 0})
		}
		return edges
	}
	if wait, ok := g.Waits[u]; ok {
		var edges []weightedEdge
		for _, target := range mapValues(wait.Outcomes) {
			edges = append(edges, weightedEdge{to: target, stepCost: 0, visitCost: 0})
		}
		return edges
	}
	if appr, ok := g.Approvals[u]; ok {
		var edges []weightedEdge
		for _, target := range mapValues(appr.Outcomes) {
			edges = append(edges, weightedEdge{to: target, stepCost: 0, visitCost: 0})
		}
		return edges
	}
	return nil
}

// stepOutcomeCosts returns the static minimum (stepCost, visitCost) incurred
// by executing step and taking outcome co.
func stepOutcomeCosts(step *StepNode, co *CompiledOutcome, g *FSMGraph) (int, int) {
	stepCost := 1
	visitCost := 1

	takesAllIterations := co.Name == "all_succeeded" ||
		(co.Name == "any_failed" && (step.OnFailure == "continue" || step.OnFailure == "ignore"))

	if takesAllIterations {
		if step.Count != nil && !exprRefsVar(step.Count) {
			if n, ok := foldStaticCount(step.Count, g); ok && n >= 1 {
				stepCost = n
				visitCost = n
			}
		} else if step.ForEach != nil && !exprRefsVar(step.ForEach) {
			if n, ok := foldStaticCollectionLen(step.ForEach, g); ok && n >= 1 {
				stepCost = n
				visitCost = n
			}
		} else if step.Parallel != nil && !exprRefsVar(step.Parallel) {
			if n, ok := foldStaticCollectionLen(step.Parallel, g); ok && n >= 1 {
				stepCost = 1
				visitCost = n
			}
		}
	}

	return stepCost, visitCost
}

// exprRefsVar reports whether expr references any var.* variable (which can be
// overridden at runtime via CLI -var flags).
func exprRefsVar(expr hcl.Expression) bool {
	if expr == nil {
		return false
	}
	for _, tr := range expr.Variables() {
		if len(tr) > 0 {
			if root, ok := tr[0].(hcl.TraverseRoot); ok && root.Name == "var" {
				return true
			}
		}
	}
	return false
}

func foldStaticCount(expr hcl.Expression, g *FSMGraph) (int, bool) {
	val, folded, diags := FoldExpr(expr, nil, graphLocals(g), g.WorkflowDir)
	if diags.HasErrors() || !folded || !val.IsKnown() || val.IsNull() || !val.Type().Equals(cty.Number) {
		return 0, false
	}
	bf := val.AsBigFloat()
	if !bf.IsInt() {
		return 0, false
	}
	n, _ := bf.Int64()
	return int(n), true
}

func foldStaticCollectionLen(expr hcl.Expression, g *FSMGraph) (int, bool) {
	val, folded, diags := FoldExpr(expr, nil, graphLocals(g), g.WorkflowDir)
	if diags.HasErrors() || !folded || !val.IsKnown() || val.IsNull() {
		return 0, false
	}
	t := val.Type()
	if t.IsListType() || t.IsTupleType() || t.IsSetType() || t.IsMapType() || t.IsObjectType() {
		return val.LengthInt(), true
	}
	return 0, false
}

// checkBudgetAndVisitFeasibility proves static step_budget (policy.max_total_steps)
// and per-step max_visits feasibility along shortest paths from initial_state
// to target_state.
func checkBudgetAndVisitFeasibility(g *FSMGraph) hcl.Diagnostics {
	var diags hcl.Diagnostics

	minSteps, minPath, ok := shortestStepPathToTarget(g, false)
	if !ok {
		return nil
	}

	if g.Policy.MaxTotalSteps > 0 && minSteps > g.Policy.MaxTotalSteps {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary: fmt.Sprintf(
				"unsatisfiable step_budget: shortest path from %q to %q [%s] requires %d steps, but budget is %d",
				g.InitialState, g.TargetState, strings.Join(minPath, " -> "), minSteps, g.Policy.MaxTotalSteps,
			),
		})
	}

	// Check if any path from initial_state to target_state satisfies per-step max_visits.
	if _, _, validVisitPathExists := shortestStepPathToTarget(g, true); !validVisitPathExists {
		if violatingStep, reqVisits, maxVisits := findVisitBoundViolationOnPath(minPath, g); violatingStep != "" {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary: fmt.Sprintf(
					"unsatisfiable max_visits: shortest path from %q to %q [%s] requires visiting step %q %d times, but max_visits is %d",
					g.InitialState, g.TargetState, strings.Join(minPath, " -> "), violatingStep, reqVisits, maxVisits,
				),
			})
		}
	}

	return diags
}

// shortestStepPathToTarget runs Dijkstra's algorithm from g.InitialState to
// g.TargetState (or ReturnSentinel). When enforceMaxVisits is true, edges whose
// visitCost exceeds step.MaxVisits (when MaxVisits > 0) are excluded.
func shortestStepPathToTarget(g *FSMGraph, enforceMaxVisits bool) (int, []string, bool) {
	dist := map[string]int{g.InitialState: 0}
	parent := make(map[string]string)
	visited := make(map[string]bool)

	for {
		u := ""
		bestDist := math.MaxInt
		for node, d := range dist {
			if !visited[node] && (d < bestDist || (d == bestDist && node < u)) {
				u = node
				bestDist = d
			}
		}
		if u == "" {
			break
		}
		if u == g.TargetState || u == ReturnSentinel {
			path := reconstructBFSPath(parent, g.InitialState, u)
			if u == ReturnSentinel && len(path) > 0 && path[len(path)-1] == ReturnSentinel {
				path[len(path)-1] = g.TargetState
			}
			return bestDist, path, true
		}
		visited[u] = true

		step := g.Steps[u]
		for _, edge := range outgoingWeightedEdges(u, g) {
			if enforceMaxVisits && step != nil && step.MaxVisits > 0 && edge.visitCost > step.MaxVisits {
				continue
			}
			nd := bestDist + edge.stepCost
			if old, exists := dist[edge.to]; !exists || nd < old {
				dist[edge.to] = nd
				parent[edge.to] = u
			}
		}
	}

	return 0, nil, false
}

func findVisitBoundViolationOnPath(path []string, g *FSMGraph) (string, int, int) {
	for i := 0; i+1 < len(path); i++ {
		u := path[i]
		v := path[i+1]
		step, ok := g.Steps[u]
		if !ok || step.MaxVisits <= 0 {
			continue
		}
		minVisitForEdge := math.MaxInt
		for _, edge := range outgoingWeightedEdges(u, g) {
			target := edge.to
			if target == ReturnSentinel {
				target = g.TargetState
			}
			if target == v && edge.visitCost < minVisitForEdge {
				minVisitForEdge = edge.visitCost
			}
		}
		if minVisitForEdge != math.MaxInt && minVisitForEdge > step.MaxVisits {
			return step.Name, minVisitForEdge, step.MaxVisits
		}
	}
	return "", 0, 0
}
