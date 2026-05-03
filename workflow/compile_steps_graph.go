package workflow

// compile_steps_graph.go — FSM graph traversal and node construction helpers
// used by every step-kind compiler: back-edge detection, reachable-target
// enumeration, outcome compilation, and node constructors.

import (
	"fmt"
	"time"

	"github.com/hashicorp/hcl/v2"
)

// resolveAdapterName returns the effective adapter name for a step: either
// sp.Adapter (direct) or the adapter backing sp.Agent (indirect).
func resolveAdapterName(g *FSMGraph, sp *StepSpec) string {
	if sp.Adapter != "" {
		return sp.Adapter
	}
	if sp.Agent != "" {
		if agent, ok := g.Agents[sp.Agent]; ok {
			return agent.Adapter
		}
	}
	return ""
}

// resolveStepOnCrash returns the effective on_crash for a step, falling back
// to the backing agent's on_crash if the step doesn't specify one.
func resolveStepOnCrash(g *FSMGraph, sp *StepSpec) (string, hcl.Diagnostics) {
	var diags hcl.Diagnostics
	if sp.OnCrash != "" && !isValidOnCrash(sp.OnCrash) {
		diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("step %q: invalid on_crash %q", sp.Name, sp.OnCrash)})
	}
	effective := sp.OnCrash
	if effective == "" {
		if sp.Agent != "" {
			if agent, ok := g.Agents[sp.Agent]; ok {
				effective = agent.OnCrash
			} else {
				effective = onCrashFail
			}
		} else {
			effective = onCrashFail
		}
	}
	return effective, diags
}

// compileOutcomeBlock populates node.Outcomes from sp.Outcomes, checking for
// duplicates and missing transition_to values.
func compileOutcomeBlock(sp *StepSpec, node *StepNode) hcl.Diagnostics {
	var diags hcl.Diagnostics
	seen := map[string]bool{}
	for _, o := range sp.Outcomes {
		if seen[o.Name] {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("step %q: duplicate outcome %q", sp.Name, o.Name)})
			continue
		}
		seen[o.Name] = true
		if o.TransitionTo == "" {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("step %q outcome %q: transition_to required", sp.Name, o.Name)})
			continue
		}
		node.Outcomes[o.Name] = o.TransitionTo
	}
	return diags
}

// newWorkflowStepNode constructs a StepNode for a type="workflow" step with
// all common fields plus optional ForEach/Count.
func newWorkflowStepNode(sp *StepSpec, spec *Spec, effectiveOnCrash string, timeout time.Duration,
	inputMap map[string]string, inputExprs map[string]hcl.Expression,
	forEachExpr, countExpr hcl.Expression) *StepNode {
	return &StepNode{
		Name:       sp.Name,
		OnCrash:    effectiveOnCrash,
		Type:       sp.Type,
		OnFailure:  sp.OnFailure,
		MaxVisits:  sp.MaxVisits,
		Input:      inputMap,
		InputExprs: inputExprs,
		Timeout:    timeout,
		Outcomes:   map[string]string{},
		AllowTools: allowToolsForStep(sp, spec),
		ForEach:    forEachExpr,
		Count:      countExpr,
	}
}

// compileWorkflowOutputs extracts output{} block expressions from sp.Workflow
// and populates node.Outputs. It is safe to call when sp.Workflow is nil or
// has no outputs — it returns nil in that case.
func compileWorkflowOutputs(g *FSMGraph, sp *StepSpec, node *StepNode, opts CompileOpts) hcl.Diagnostics {
	if sp.Workflow == nil || len(sp.Workflow.Outputs) == 0 {
		return nil
	}
	var diags hcl.Diagnostics
	seen := map[string]bool{}
	node.Outputs = make(map[string]hcl.Expression, len(sp.Workflow.Outputs))
	for _, out := range sp.Workflow.Outputs {
		if out == nil {
			continue
		}
		if seen[out.Name] {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("step %q: duplicate output name %q", sp.Name, out.Name),
			})
			continue
		}
		seen[out.Name] = true
		content, _, d := out.Remain.PartialContent(&hcl.BodySchema{
			Attributes: []hcl.AttributeSchema{{Name: "value", Required: true}},
		})
		diags = append(diags, d...)
		if content != nil {
			if attr, ok := content.Attributes["value"]; ok {
				node.Outputs[out.Name] = attr.Expr
				// Validate the value expression at compile time.
				_, foldable, fd := FoldExpr(attr.Expr, graphVars(g), graphLocals(g), opts.WorkflowDir)
				if foldable {
					diags = append(diags, errorDiagsWithFallbackSubject(fd, attr.Expr)...)
				}
			}
		}
	}
	return diags
}

// warnBackEdges emits a compile-time warning for every step that:
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
// The "_continue" pseudo-target is excluded because it is never a real node.
func nodeTargets(name string, g *FSMGraph) []string {
	if step, ok := g.Steps[name]; ok {
		var targets []string
		for _, t := range step.Outcomes {
			if t != "_continue" {
				targets = append(targets, t)
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
