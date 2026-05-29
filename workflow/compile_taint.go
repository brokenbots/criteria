package workflow

// compile_taint.go — secret-taint propagation pass.
//
// A step is tainted when:
//   1. Any secret_input{} expression references a tainted origin.
//   2. Any regular input{} expression references a tainted origin (this also
//      emits a hard compile error per D65).
//   3. The step's target adapter has a non-empty secrets{} block.
//   4. Any predecessor step (reachable via outcome edges) is tainted.
//
// Additionally, adapter config{} expressions that reference tainted origins
// emit hard compile errors (D65).

import (
	"fmt"

	"github.com/hashicorp/hcl/v2"
)

// taintOrigin describes where a tainted value came from.
type taintOrigin struct {
	kind string // "variable", "shared_variable", "adapter_secret", "sensitive_output"
	name string // e.g. "var.api_key", "shared.token", "adapter.shell.default.secrets"
}

// newTaintError builds the hard compile-error diagnostic required by D65.
func newTaintError(origin taintOrigin, subject *hcl.Range) *hcl.Diagnostic {
	return &hcl.Diagnostic{
		Severity: hcl.DiagError,
		Summary:  fmt.Sprintf("tainted value %s cannot be used in a non-secret channel", origin.name),
		Detail:   "Tainted values may only flow through secret channels. Bind it via adapter.X.secrets { ... } or step.X.secret_inputs { ... } instead.",
		Subject:  subject,
	}
}

// TaintPass walks the compiled FSM graph and marks StepNode.Tainted on every
// step that transitively receives secret data. It returns diagnostics when
// tainted values are used in non-secret channels (input{} and adapter.config{}).
//
//nolint:gocognit,gocyclo,funlen // WS09: multi-pass taint propagation is inherently complex
func TaintPass(g *FSMGraph, schemas map[string]AdapterInfo) hcl.Diagnostics {
	if g == nil || len(g.Steps) == 0 {
		return nil
	}

	// Build reverse adjacency: for each step, which steps point to it.
	preds := buildPredecessors(g)
	var diags hcl.Diagnostics

	// Phase 1: mark steps that directly reference tainted origins.
	for _, name := range g.stepOrder {
		step := g.Steps[name]
		if step == nil {
			continue
		}
		// secret_input is a secret channel — mark tainted but do not error.
		for _, expr := range step.SecretInputExprs {
			if _, tainted := checkExprForTaint(expr, g, schemas); tainted {
				step.Tainted = true
				break
			}
		}
		// input is a non-secret channel — mark tainted and collect errors in Phase 3.
		for _, expr := range step.InputExprs {
			if _, tainted := checkExprForTaint(expr, g, schemas); tainted {
				step.Tainted = true
			}
		}
		// Adapter with secrets taints the step.
		if step.AdapterRef != "" {
			if ad, ok := g.Adapters[step.AdapterRef]; ok && len(ad.Secrets) > 0 {
				step.Tainted = true
			}
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

	// Phase 3: emit hard errors for tainted values in non-secret channels.
	for _, name := range g.stepOrder {
		step := g.Steps[name]
		if step == nil {
			continue
		}
		for _, expr := range step.InputExprs {
			if origin, tainted := checkExprForTaint(expr, g, schemas); tainted {
				diags = append(diags, newTaintError(origin, expr.Range().Ptr()))
			}
		}
	}
	for _, ad := range g.Adapters {
		for _, expr := range ad.ConfigExprs {
			if origin, tainted := checkExprForTaint(expr, g, schemas); tainted {
				diags = append(diags, newTaintError(origin, expr.Range().Ptr()))
			}
		}
	}

	return diags
}

// buildPredecessors returns a map from step name to the list of step names
// that have an outcome edge leading to it.
//
//nolint:gocognit // WS09: two-path outcome+defaultOutcome loop is clear and compact
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

// checkExprForTaint inspects every variable traversal in expr and returns
// (origin, true) if any traversal references a tainted origin.
//
//nolint:gocognit,gocyclo,funlen // WS09: traversal type-switch for var/shared/adapter/steps namespaces
func checkExprForTaint(expr hcl.Expression, g *FSMGraph, schemas map[string]AdapterInfo) (taintOrigin, bool) {
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
				return taintOrigin{kind: "variable", name: "var." + attr.Name}, true
			}
		case "shared":
			if sv, ok := g.SharedVariables[attr.Name]; ok && sv.Secret {
				return taintOrigin{kind: "shared_variable", name: "shared." + attr.Name}, true
			}
		case "adapter":
			// adapter.TYPE.NAME.secrets.KEY
			if len(traversal) < 4 {
				continue
			}
			typeAttr, ok2 := traversal[1].(hcl.TraverseAttr)
			nameAttr, ok3 := traversal[2].(hcl.TraverseAttr)
			if !ok2 || !ok3 {
				continue
			}
			adapterRef := typeAttr.Name + "." + nameAttr.Name
			if ad, ok := g.Adapters[adapterRef]; ok && len(ad.Secrets) > 0 {
				if secAttr, ok4 := traversal[3].(hcl.TraverseAttr); ok4 && secAttr.Name == "secrets" {
					return taintOrigin{kind: "adapter_secret", name: "adapter." + adapterRef + ".secrets"}, true
				}
			}
		case "steps":
			// steps.STEP.FIELD  (3 parts)
			// steps.STEP.outputs.FIELD (4 parts)
			if len(traversal) < 3 {
				continue
			}
			stepName, ok2 := traversal[1].(hcl.TraverseAttr)
			if !ok2 {
				continue
			}
			var fieldAttr hcl.TraverseAttr
			if len(traversal) == 3 {
				// steps.STEP.FIELD
				fieldAttr, ok2 = traversal[2].(hcl.TraverseAttr)
				if !ok2 {
					continue
				}
			} else if len(traversal) >= 4 {
				// steps.STEP.outputs.FIELD
				outAttr, ok3 := traversal[2].(hcl.TraverseAttr)
				if !ok3 || outAttr.Name != "outputs" {
					continue
				}
				fieldAttr, ok2 = traversal[3].(hcl.TraverseAttr)
				if !ok2 {
					continue
				}
			}
			upstreamStep := g.Steps[stepName.Name]
			if upstreamStep != nil && upstreamStep.AdapterRef != "" {
				adapterType := adapterTypeFromRef(upstreamStep.AdapterRef)
				if info, ok := adapterInfo(schemas, adapterType); ok {
					if field, ok := info.OutputSchema[fieldAttr.Name]; ok && field.Sensitive {
						return taintOrigin{kind: "sensitive_output", name: "steps." + stepName.Name + "." + fieldAttr.Name}, true
					}
				}
			}
		}
	}
	return taintOrigin{}, false
}
