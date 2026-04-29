package workflow

// compile_nodes.go — compilation of wait, approval, and branch blocks
// into their respective FSMGraph node maps.

import (
	"fmt"
	"time"

	"github.com/hashicorp/hcl/v2"
)

// compileWaits compiles all wait blocks from spec into g.Waits.
// Must be called after compileSteps so that clash checks against steps work.
func compileWaits(g *FSMGraph, spec *Spec) hcl.Diagnostics {
	var diags hcl.Diagnostics
	for _, ws := range spec.Waits {
		name := ws.Name
		if _, dup := g.Steps[name]; dup {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("wait %q clashes with step of the same name", name)})
			continue
		}
		if _, dup := g.States[name]; dup {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("wait %q clashes with state of the same name", name)})
			continue
		}
		if _, dup := g.Waits[name]; dup {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("duplicate wait %q", name)})
			continue
		}
		hasDuration := ws.Duration != ""
		hasSignal := ws.Signal != ""
		if hasDuration == hasSignal { // both or neither
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("wait %q: exactly one of duration or signal must be set", name)})
			continue
		}
		node := &WaitNode{
			Name:     name,
			Signal:   ws.Signal,
			Outcomes: map[string]string{},
		}
		if hasDuration {
			d, err := time.ParseDuration(ws.Duration)
			if err != nil {
				diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("wait %q: invalid duration %q: %v", name, ws.Duration, err)})
				continue
			}
			node.Duration = d
		}
		if len(ws.Outcomes) == 0 {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("wait %q: at least one outcome is required", name)})
		}
		for _, o := range ws.Outcomes {
			if o.TransitionTo == "" {
				diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("wait %q outcome %q: transition_to required", name, o.Name)})
				continue
			}
			node.Outcomes[o.Name] = o.TransitionTo
		}
		g.Waits[name] = node
	}
	return diags
}

// compileApprovals compiles all approval blocks from spec into g.Approvals.
// Must be called after compileWaits so that clash checks work correctly.
func compileApprovals(g *FSMGraph, spec *Spec) hcl.Diagnostics {
	var diags hcl.Diagnostics
	for _, as := range spec.Approvals {
		name := as.Name
		if _, dup := g.Steps[name]; dup {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("approval %q clashes with step of the same name", name)})
			continue
		}
		if _, dup := g.States[name]; dup {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("approval %q clashes with state of the same name", name)})
			continue
		}
		if _, dup := g.Waits[name]; dup {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("approval %q clashes with wait of the same name", name)})
			continue
		}
		if _, dup := g.Approvals[name]; dup {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("duplicate approval %q", name)})
			continue
		}
		node := &ApprovalNode{
			Name:      name,
			Approvers: as.Approvers,
			Reason:    as.Reason,
			Outcomes:  map[string]string{},
		}
		for _, o := range as.Outcomes {
			if o.TransitionTo == "" {
				diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("approval %q outcome %q: transition_to required", name, o.Name)})
				continue
			}
			node.Outcomes[o.Name] = o.TransitionTo
		}
		// Enforce required outcomes: approved and rejected must both be present.
		if _, ok := node.Outcomes["approved"]; !ok {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("approval %q: outcome \"approved\" is required", name)})
		}
		if _, ok := node.Outcomes["rejected"]; !ok {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("approval %q: outcome \"rejected\" is required", name)})
		}
		g.Approvals[name] = node
	}
	return diags
}

// compileBranches compiles all branch blocks from spec into g.Branches.
// Must be called after compileApprovals so that full clash checks work.
func compileBranches(g *FSMGraph, spec *Spec) hcl.Diagnostics {
	var diags hcl.Diagnostics
	for _, bs := range spec.Branches {
		name := bs.Name
		// Clash checks: must not duplicate any existing node kind.
		if _, dup := g.Steps[name]; dup {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("branch %q clashes with step of the same name", name)})
			continue
		}
		if _, dup := g.States[name]; dup {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("branch %q clashes with state of the same name", name)})
			continue
		}
		if _, dup := g.Waits[name]; dup {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("branch %q clashes with wait of the same name", name)})
			continue
		}
		if _, dup := g.Approvals[name]; dup {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("branch %q clashes with approval of the same name", name)})
			continue
		}
		if _, dup := g.Branches[name]; dup {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("duplicate branch %q", name)})
			continue
		}
		// Default block is required.
		if bs.Default == nil {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("branch %q: default block is required", name)})
			continue
		}
		if bs.Default.TransitionTo == "" {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("branch %q: default transition_to is required", name)})
			continue
		}
		// Compile arms.
		node := &BranchNode{
			Name:          name,
			DefaultTarget: bs.Default.TransitionTo,
		}
		for i, arm := range bs.Arms {
			if arm.TransitionTo == "" {
				diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("branch %q arm[%d]: transition_to is required", name, i)})
				continue
			}
			// Extract the `when` expression from the remain body.
			var condExpr hcl.Expression
			if arm.Remain != nil {
				attrs, d := arm.Remain.JustAttributes()
				diags = append(diags, d...)
				if whenAttr, ok := attrs["when"]; ok {
					condExpr = whenAttr.Expr
				}
			}
			if condExpr == nil {
				diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("branch %q arm[%d]: when expression is required", name, i)})
				continue
			}
			// Walk the expression traversals: validate that referenced var/<name>
			// and steps/<name> names exist in the graph. This is a best-effort
			// static check; full type evaluation is a runtime concern.
			for _, traversal := range condExpr.Variables() {
				if len(traversal) < 2 {
					continue
				}
				root, rootOK := traversal[0].(hcl.TraverseRoot)
				if !rootOK {
					continue
				}
				attr, attrOK := traversal[1].(hcl.TraverseAttr)
				if !attrOK {
					continue
				}
				switch root.Name {
				case "var":
					if _, known := g.Variables[attr.Name]; !known {
						diags = append(diags, &hcl.Diagnostic{
							Severity: hcl.DiagError,
							Summary:  fmt.Sprintf("branch %q arm[%d]: undefined variable %q", name, i, attr.Name),
						})
					}
				case "steps":
					if _, known := g.Steps[attr.Name]; !known {
						diags = append(diags, &hcl.Diagnostic{
							Severity: hcl.DiagError,
							Summary:  fmt.Sprintf("branch %q arm[%d]: unknown step %q referenced in condition", name, i, attr.Name),
						})
					}
				}
			}
			// Capture the source text of the condition expression so it can be
			// surfaced in BranchEvaluated events (W06).
			condSrc := ""
			if spec.SourceBytes != nil {
				type noder interface{ Range() hcl.Range }
				if nr, ok := condExpr.(noder); ok {
					r := nr.Range()
					if r.Start.Byte < r.End.Byte && r.End.Byte <= len(spec.SourceBytes) {
						condSrc = string(spec.SourceBytes[r.Start.Byte:r.End.Byte])
					}
				}
			}
			node.Arms = append(node.Arms, BranchArm{
				Condition:    condExpr,
				ConditionSrc: condSrc,
				Target:       arm.TransitionTo,
			})
		}
		g.Branches[name] = node
	}
	return diags
}
