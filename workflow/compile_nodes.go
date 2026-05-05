package workflow

// compile_nodes.go — compilation of wait and approval blocks
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
			if o.Next == "" {
				diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("wait %q outcome %q: next is required", name, o.Name)})
				continue
			}
			node.Outcomes[o.Name] = o.Next
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
			if o.Next == "" {
				diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("approval %q outcome %q: next is required", name, o.Name)})
				continue
			}
			node.Outcomes[o.Name] = o.Next
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

// compileSwitches is now in compile_switches.go.
