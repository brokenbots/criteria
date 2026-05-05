package workflow

// compile_switches.go — compilation of switch blocks into FSMGraph SwitchNodes.

import (
	"fmt"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
)

// compileSwitches compiles all switch blocks from spec into g.Switches.
// Must be called after compileApprovals so that full clash checks work.
func compileSwitches(g *FSMGraph, spec *Spec, opts CompileOpts) hcl.Diagnostics {
	var diags hcl.Diagnostics
	for _, ss := range spec.Switches {
		name := ss.Name
		if d := checkSwitchNameClash(name, g); d != nil {
			diags = append(diags, d)
			continue
		}

		node := &SwitchNode{Name: name}

		if ss.Default != nil {
			defaultNext, defaultOut, defDiags := compileSwitchDefaultBlock(ss.Default, name, g, opts)
			diags = append(diags, defDiags...)
			node.DefaultNext = defaultNext
			node.DefaultOutput = defaultOut
		} else if !isSwitchProvedExhaustive(ss.Conditions, g, opts) {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagWarning,
				Summary:  fmt.Sprintf("switch %q: default block is required when conditions are not provably exhaustive", name),
				Detail:   "A switch without a default block will fail at runtime if no condition matches. Add a default block or ensure one condition has match = true.",
			})
		}

		for i, cs := range ss.Conditions {
			cond, condDiags := compileSwitchConditionBlock(cs, i, name, spec.SourceBytes, g, opts)
			diags = append(diags, condDiags...)
			if cond != nil {
				node.Conditions = append(node.Conditions, *cond)
			}
		}

		g.Switches[name] = node
	}
	return diags
}

// checkSwitchNameClash returns a diagnostic if the given switch name clashes
// with any existing node in the graph.
func checkSwitchNameClash(name string, g *FSMGraph) *hcl.Diagnostic {
	switch {
	case g.Steps[name] != nil:
		return &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("switch %q clashes with step of the same name", name)}
	case g.States[name] != nil:
		return &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("switch %q clashes with state of the same name", name)}
	case g.Waits[name] != nil:
		return &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("switch %q clashes with wait of the same name", name)}
	case g.Approvals[name] != nil:
		return &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("switch %q clashes with approval of the same name", name)}
	case g.Switches[name] != nil:
		return &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("duplicate switch %q", name)}
	}
	return nil
}

// compileSwitchDefaultBlock compiles the default block of a switch, returning
// the resolved next target, optional output expression, and any diagnostics.
func compileSwitchDefaultBlock(def *SwitchDefaultSpec, switchName string, g *FSMGraph, opts CompileOpts) (string, hcl.Expression, hcl.Diagnostics) {
	var diags hcl.Diagnostics

	attrs, attrDiags := def.Remain.JustAttributes()
	diags = append(diags, attrDiags...)

	for attrName := range attrs {
		if attrName != "next" && attrName != "output" {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("switch %q default: unknown attribute %q (allowed: next, output)", switchName, attrName),
				Subject:  &attrs[attrName].NameRange,
			})
		}
	}

	nextAttr, hasNext := attrs["next"]
	if !hasNext {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("switch %q default: next is required", switchName),
		})
		return "", nil, diags
	}

	defaultNext, nextDiags := resolveNextAttr(nextAttr.Expr, switchName, "default")
	diags = append(diags, nextDiags...)

	var outExpr hcl.Expression
	if outAttr, ok := attrs["output"]; ok {
		outExpr = outAttr.Expr
		diags = append(diags, validateSwitchOutputExpr(switchName, "default", outAttr, g, opts)...)
	}

	return defaultNext, outExpr, diags
}

// compileSwitchConditionBlock compiles a single condition block within a switch.
// Returns a SwitchCondition (or nil on critical error) and diagnostics.
func compileSwitchConditionBlock(cs ConditionSpec, idx int, switchName string, sourceBytes []byte, g *FSMGraph, opts CompileOpts) (*SwitchCondition, hcl.Diagnostics) {
	var diags hcl.Diagnostics
	location := fmt.Sprintf("condition[%d]", idx)

	attrs, attrDiags := cs.Remain.JustAttributes()
	diags = append(diags, attrDiags...)

	for attrName := range attrs {
		if attrName != "match" && attrName != "next" && attrName != "output" {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("switch %q %s: unknown attribute %q (allowed: match, next, output)", switchName, location, attrName),
				Subject:  &attrs[attrName].NameRange,
			})
		}
	}

	matchAttr, hasMatch := attrs["match"]
	if !hasMatch {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("switch %q %s: match is required", switchName, location),
		})
		return nil, diags
	}

	nextCondAttr, hasCondNext := attrs["next"]
	if !hasCondNext {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("switch %q %s: next is required", switchName, location),
		})
		return nil, diags
	}

	matchExpr := matchAttr.Expr
	diags = append(diags, validateSwitchExprRefs(matchExpr, g, switchName, idx)...)
	if _, foldable, fd := FoldExpr(matchExpr, graphVars(g), graphLocals(g), opts.WorkflowDir); foldable {
		diags = append(diags, errorDiagsWithFallbackSubject(fd, matchExpr)...)
	}

	condNext, condNextDiags := resolveNextAttr(nextCondAttr.Expr, switchName, location)
	diags = append(diags, condNextDiags...)

	cond := &SwitchCondition{
		Match:    matchExpr,
		MatchSrc: extractExprSource(matchExpr, sourceBytes),
		Next:     condNext,
	}

	if outAttr, ok := attrs["output"]; ok {
		cond.OutputExpr = outAttr.Expr
		diags = append(diags, validateSwitchOutputExpr(switchName, location, outAttr, g, opts)...)
	}

	return cond, diags
}

// isSwitchProvedExhaustive reports whether the given conditions are provably
// exhaustive — i.e. at least one condition folds to the constant true.
func isSwitchProvedExhaustive(conditions []ConditionSpec, g *FSMGraph, opts CompileOpts) bool {
	for _, cs := range conditions {
		attrs, _ := cs.Remain.JustAttributes()
		matchAttr, ok := attrs["match"]
		if !ok {
			continue
		}
		val, foldable, _ := FoldExpr(matchAttr.Expr, graphVars(g), graphLocals(g), opts.WorkflowDir)
		if foldable && val == cty.True {
			return true
		}
	}
	return false
}

// validateSwitchOutputExpr validates the output = { ... } expression on a
// switch condition or default block. If the expression is foldable at compile
// time, it must evaluate to an object; otherwise it is deferred to runtime.
func validateSwitchOutputExpr(switchName, location string, attr *hcl.Attribute, g *FSMGraph, opts CompileOpts) hcl.Diagnostics {
	refDiags := validateFoldableAttrs(hcl.Attributes{attr.Name: attr}, graphVars(g), graphLocals(g), opts.WorkflowDir)
	if refDiags.HasErrors() {
		return refDiags
	}
	val, foldable, foldDiags := FoldExpr(attr.Expr, graphVars(g), graphLocals(g), opts.WorkflowDir)
	if foldDiags.HasErrors() {
		return foldDiags
	}
	if !foldable || val == cty.NilVal || !val.IsKnown() {
		return nil
	}
	if !val.Type().IsObjectType() && val.Type() != cty.DynamicPseudoType {
		r := attr.Expr.StartRange()
		return hcl.Diagnostics{&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("switch %q %s: output must be an object literal; got %s", switchName, location, val.Type().FriendlyName()),
			Subject:  &r,
		}}
	}
	return nil
}

// resolveNextAttr resolves a `next` attribute expression to a node name string.
// Accepts traversal form (step.foo, state.done) or bare string ("done", "return").
// Traversals must be exactly two parts: <kind>.<name>. Extra segments are rejected.
// Returns the resolved name and any diagnostics.
func resolveNextAttr(expr hcl.Expression, switchName, location string) (string, hcl.Diagnostics) {
	var diags hcl.Diagnostics

	vars := expr.Variables()
	if len(vars) == 0 {
		// Literal string form: next = "done" or next = "return".
		val, evalDiags := expr.Value(nil)
		if evalDiags.HasErrors() {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("switch %q %s: could not evaluate next: %s", switchName, location, evalDiags.Error()),
				Subject:  expr.StartRange().Ptr(),
			})
			return "", diags
		}
		if val.Type() != cty.String {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("switch %q %s: next must be a string or traversal, got %s", switchName, location, val.Type().FriendlyName()),
				Subject:  expr.StartRange().Ptr(),
			})
			return "", diags
		}
		return val.AsString(), diags
	}

	// Traversal form: next = step.foo  or  next = state.done.
	// Must be exactly two segments: <kind>.<name>. Reject step.foo.bar etc.
	if len(vars) == 1 {
		traversal := vars[0]
		if len(traversal) != 2 {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("switch %q %s: next traversal must be exactly <kind>.<name> (e.g. step.foo, state.done); got %d segments", switchName, location, len(traversal)),
				Subject:  expr.StartRange().Ptr(),
			})
			return "", diags
		}
		root, rootOK := traversal[0].(hcl.TraverseRoot)
		attr, attrOK := traversal[1].(hcl.TraverseAttr)
		if rootOK && attrOK {
			switch root.Name {
			case "step", "state", "wait", "approval", "switch":
				return attr.Name, diags
			}
		}
	}

	diags = append(diags, &hcl.Diagnostic{
		Severity: hcl.DiagError,
		Summary:  fmt.Sprintf("switch %q %s: next must be a string literal (\"name\") or a node traversal (step.name, state.name)", switchName, location),
		Subject:  expr.StartRange().Ptr(),
	})
	return "", diags
}

// validateSwitchExprRefs validates that variable and step traversals referenced
// in a switch condition match expression are declared in the graph.
func validateSwitchExprRefs(expr hcl.Expression, g *FSMGraph, switchName string, condIdx int) hcl.Diagnostics {
	var diags hcl.Diagnostics
	for _, traversal := range expr.Variables() {
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
					Summary:  fmt.Sprintf("switch %q condition[%d]: undefined variable %q", switchName, condIdx, attr.Name),
				})
			}
		case "steps":
			// steps.<name> may reference a step or a switch node (switches
			// publish their output under steps.<switch_name>.*).
			if attr.Name == switchName {
				diags = append(diags, &hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  fmt.Sprintf("switch %q condition[%d]: self-reference steps.%s is always empty at match time; use a variable or a prior step instead", switchName, condIdx, switchName),
				})
				continue
			}
			_, isStep := g.Steps[attr.Name]
			_, isSwitch := g.Switches[attr.Name]
			if !isStep && !isSwitch {
				diags = append(diags, &hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  fmt.Sprintf("switch %q condition[%d]: unknown step %q referenced in match expression", switchName, condIdx, attr.Name),
				})
			}
		}
	}
	return diags
}

// extractExprSource extracts the source text of an expression from raw source bytes.
func extractExprSource(expr hcl.Expression, sourceBytes []byte) string {
	if sourceBytes == nil {
		return ""
	}
	type noder interface{ Range() hcl.Range }
	if nr, ok := expr.(noder); ok {
		r := nr.Range()
		if r.Start.Byte < r.End.Byte && r.End.Byte <= len(sourceBytes) {
			return string(sourceBytes[r.Start.Byte:r.End.Byte])
		}
	}
	return ""
}
