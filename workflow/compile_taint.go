package workflow

// compile_taint.go — secret-taint propagation pass.
//
// A step is tainted when:
//   1. Any secret_input{} expression references a variable or shared_variable
//      with Secret=true.
//   2. Any regular input{} expression references a variable or shared_variable
//      with Secret=true.
//   3. Any predecessor step (reachable via outcome edges) is tainted.
//
// Taint propagates forward until a fixed point is reached.

import (
	"fmt"

	"github.com/hashicorp/hcl/v2"
)

// TaintPass walks the compiled FSM graph and marks StepNode.Tainted on every
// step that transitively receives secret data. It returns diagnostics when
// tainted variables are used in non-secret input contexts (future-proofing).
func TaintPass(g *FSMGraph, schemas map[string]AdapterInfo) hcl.Diagnostics {
	if g == nil || len(g.Steps) == 0 {
		return nil
	}

	// Build reverse adjacency: for each step, which steps point to it.
	preds := buildPredecessors(g)

	// Phase 1: mark steps that directly reference secret variables.
	for _, name := range g.stepOrder {
		step := g.Steps[name]
		if step == nil {
			continue
		}
		if referencesSecretVar(step.SecretInputExprs, g) || referencesSecretVar(step.InputExprs, g) {
			step.Tainted = true
		}
	}

	// Phase 2: propagate taint forward through the graph until fixed point.
	changed := true
	for changed {
		changed = false
		for _, name := range g.stepOrder {
			step := g.Steps[name]
			if step == nil || step.Tainted {
				continue
			}
			for _, p := range preds[name] {
				if predStep := g.Steps[p]; predStep != nil && predStep.Tainted {
					step.Tainted = true
					changed = true
					break
				}
			}
		}
	}

	return nil
}

// buildPredecessors returns a map from step name to the list of step names
// that have an outcome edge leading to it.
func buildPredecessors(g *FSMGraph) map[string][]string {
	preds := make(map[string][]string, len(g.Steps))
	for _, name := range g.stepOrder {
		step := g.Steps[name]
		if step == nil {
			continue
		}
		for _, co := range step.Outcomes {
			target := co.Next
			if target == "_continue" || target == ReturnSentinel {
				continue
			}
			if _, ok := g.Steps[target]; ok {
				preds[target] = append(preds[target], name)
			}
		}
		if step.DefaultOutcome != nil {
			target := step.DefaultOutcome.Next
			if target != "_continue" && target != ReturnSentinel {
				if _, ok := g.Steps[target]; ok {
					preds[target] = append(preds[target], name)
				}
			}
		}
	}
	return preds
}

// referencesSecretVar inspects every variable traversal in exprs and returns
// true if any traversal references a variable or shared_variable whose
// Secret flag is true.
func referencesSecretVar(exprs map[string]hcl.Expression, g *FSMGraph) bool {
	for _, expr := range exprs {
		for _, traversal := range expr.Variables() {
			if len(traversal) < 2 {
				continue
			}
			root, ok := traversal[0].(hcl.TraverseRoot)
			if !ok {
				continue
			}
			attr, ok := traversal[1].(hcl.TraverseAttr)
			if !ok {
				continue
			}
			switch root.Name {
			case "var":
				if v, ok := g.Variables[attr.Name]; ok && v.Secret {
					return true
				}
			case "shared":
				if sv, ok := g.SharedVariables[attr.Name]; ok && sv.Secret {
					return true
				}
			}
		}
	}
	return false
}

// IsTaintedDiagnostic returns a diagnostic that can be used when a tainted
// step is used in a context that requires non-secret data. Currently a
// no-op placeholder for future policy enforcement.
func IsTaintedDiagnostic(stepName string, context string) *hcl.Diagnostic {
	return &hcl.Diagnostic{
		Severity: hcl.DiagWarning,
		Summary:  fmt.Sprintf("step %q (%s): step is tainted by secret data", stepName, context),
	}
}
