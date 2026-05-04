package workflow

// compile_step_target.go — `target` attribute resolution for steps (W14).
//
// Each step must declare exactly one `target` attribute that identifies what
// the step executes. The supported target kinds are:
//
//   - target = adapter.<type>.<name>    — invoke a declared adapter session.
//   - target = subworkflow.<name>       — invoke a declared subworkflow.
//
// `target = step.<name>` is intentionally rejected in v0.3.0: step-to-step
// routing belongs in `outcome` blocks (per W15). The universal target attribute
// still serves its main purpose with the two supported kinds.

import (
	"fmt"

	"github.com/hashicorp/hcl/v2"
)

// resolveStepTarget reads the `target` attribute from body (the step's Remain),
// parses the traversal into its kind and reference, and validates existence in g.
//
// Returns:
//   - kind: StepTargetAdapter or StepTargetSubworkflow.
//   - adapterRef: resolved "<type>.<name>" when kind == StepTargetAdapter.
//   - subworkflowRef: resolved subworkflow name when kind == StepTargetSubworkflow.
//   - diags: diagnostics for missing target, wrong shape, unresolved references.
//
//nolint:funlen // W14: comprehensive traversal validation requires length
func resolveStepTarget(stepName string, body hcl.Body, g *FSMGraph) (kind StepTargetKind, adapterRef, subworkflowRef string, diags hcl.Diagnostics) {
	if body == nil {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("step %q: target is required", stepName),
		})
		return 0, "", "", diags
	}

	// Use PartialContent instead of JustAttributes so that blocks (outcome, input, etc.)
	// remaining in the body do not cause "blocks are not allowed here" errors — those
	// blocks are already decoded by the StepSpec schema before this function runs.
	content, _, contentDiags := body.PartialContent(&hcl.BodySchema{
		Attributes: []hcl.AttributeSchema{
			{Name: "target", Required: false},
		},
	})
	diags = append(diags, contentDiags...)

	attr, ok := content.Attributes["target"]
	if !ok {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("step %q: target is required; use target = adapter.<type>.<name> or target = subworkflow.<name>", stepName),
		})
		return 0, "", "", diags
	}

	// The target must be a bare traversal expression, not a string literal.
	trav, travDiags := hcl.AbsTraversalForExpr(attr.Expr)
	if travDiags.HasErrors() {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("step %q: target must be a bareword traversal, not a string literal", stepName),
			Detail:   `Use target = adapter.<type>.<name> or target = subworkflow.<name>, not a quoted string.`,
			Subject:  attr.Expr.Range().Ptr(),
		})
		return 0, "", "", diags
	}

	root := trav.RootName()
	switch root {
	case "adapter":
		ref, d := resolveAdapterTarget(stepName, trav, attr, g)
		return StepTargetAdapter, ref, "", append(diags, d...)
	case "subworkflow":
		ref, d := resolveSubworkflowTarget(stepName, trav, attr, g)
		return StepTargetSubworkflow, "", ref, append(diags, d...)
	case "step":
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("step %q: target = step.<name> is not supported in v0.3.0", stepName),
			Detail:   `Step-to-step routing belongs in outcome blocks (W15). Use target = adapter.<type>.<name> or target = subworkflow.<name>.`,
			Subject:  attr.Expr.Range().Ptr(),
		})
		return 0, "", "", diags
	default:
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("step %q: unrecognised target kind %q", stepName, root),
			Detail:   `Supported target kinds: adapter.<type>.<name>, subworkflow.<name>.`,
			Subject:  attr.Expr.Range().Ptr(),
		})
		return 0, "", "", diags
	}
}

// resolveStepEnvironmentOverride reads the `environment` attribute from body
// (the step's Remain), parses the traversal of the form `<type>.<name>`, and
// validates it against g.Environments. Returns the resolved "<type>.<name>" key,
// or "" if no environment attribute is present. Returns an error diagnostic when
// the attribute is a quoted string rather than a bare traversal.
//
//nolint:funlen // W14: multi-step traversal validation with per-error diagnostics; splitting adds indirection
func resolveStepEnvironmentOverride(stepName string, body hcl.Body, g *FSMGraph) (envKey string, diags hcl.Diagnostics) {
	if body == nil {
		return "", nil
	}

	content, _, contentDiags := body.PartialContent(&hcl.BodySchema{
		Attributes: []hcl.AttributeSchema{
			{Name: "environment", Required: false},
		},
	})
	diags = append(diags, contentDiags...)

	attr, ok := content.Attributes["environment"]
	if !ok {
		return "", diags
	}

	trav, travDiags := hcl.AbsTraversalForExpr(attr.Expr)
	if travDiags.HasErrors() {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("step %q: environment must be a bareword reference (e.g. shell.ci), not a quoted string", stepName),
			Detail:   `Use environment = shell.ci (no quotes). Quoted strings are not accepted for step environment overrides.`,
			Subject:  attr.Expr.Range().Ptr(),
		})
		return "", diags
	}

	if len(trav) != 2 {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("step %q: environment must have exactly 2 segments (<type>.<name>); got %d", stepName, len(trav)),
			Subject:  attr.Expr.Range().Ptr(),
		})
		return "", diags
	}

	typeRoot, typeOK := trav[0].(hcl.TraverseRoot)
	nameAttr, nameOK := trav[1].(hcl.TraverseAttr)
	if !typeOK || !nameOK {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("step %q: environment segments must be bareword identifiers (<type>.<name>)", stepName),
			Subject:  attr.Expr.Range().Ptr(),
		})
		return "", diags
	}

	key := fmt.Sprintf("%s.%s", typeRoot.Name, nameAttr.Name)
	if _, ok := g.Environments[key]; !ok {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("step %q: environment override %q is not declared", stepName, key),
			Subject:  attr.Expr.Range().Ptr(),
		})
		return key, diags
	}
	return key, nil
}

// rejectEnvOverrideForSubworkflow emits a compile error if the step's Remain
// body contains an `environment` attribute. Environment is set at the
// subworkflow declaration level (subworkflow { environment = "shell.ci" }),
// not per-step; step-level environment overrides are only valid for
// adapter-targeted steps.
func rejectEnvOverrideForSubworkflow(stepName string, body hcl.Body) hcl.Diagnostics {
	if body == nil {
		return nil
	}
	content, _, _ := body.PartialContent(&hcl.BodySchema{
		Attributes: []hcl.AttributeSchema{{Name: "environment", Required: false}},
	})
	attr, ok := content.Attributes["environment"]
	if !ok {
		return nil
	}
	r := attr.Expr.Range()
	return hcl.Diagnostics{&hcl.Diagnostic{
		Severity: hcl.DiagError,
		Summary:  fmt.Sprintf("step %q: environment override is not valid for subworkflow-targeted steps", stepName),
		Detail:   `Set environment on the subworkflow declaration instead: subworkflow "<name>" { environment = "<type>.<name>" }.`,
		Subject:  &r,
	}}
}
func resolveAdapterTarget(stepName string, trav hcl.Traversal, attr *hcl.Attribute, g *FSMGraph) (adapterRef string, diags hcl.Diagnostics) {
	if len(trav) != 3 {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("step %q: adapter target must have exactly 3 segments (adapter.<type>.<name>); got %d", stepName, len(trav)),
			Subject:  attr.Expr.Range().Ptr(),
		})
		return "", diags
	}

	typeAttr, typeOK := trav[1].(hcl.TraverseAttr)
	nameAttr, nameOK := trav[2].(hcl.TraverseAttr)
	if !typeOK || !nameOK {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("step %q: adapter target segments must be bareword identifiers (adapter.<type>.<name>)", stepName),
			Subject:  attr.Expr.Range().Ptr(),
		})
		return "", diags
	}

	ref := fmt.Sprintf("%s.%s", typeAttr.Name, nameAttr.Name)
	if _, ok := g.Adapters[ref]; !ok {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("step %q: referenced adapter %q is not declared", stepName, ref),
			Subject:  attr.Expr.Range().Ptr(),
		})
		return ref, diags
	}
	return ref, nil
}

// resolveSubworkflowTarget validates and resolves a subworkflow traversal of the
// form subworkflow.<name>. Returns the subworkflow name and any diagnostics.
func resolveSubworkflowTarget(stepName string, trav hcl.Traversal, attr *hcl.Attribute, g *FSMGraph) (subworkflowRef string, diags hcl.Diagnostics) {
	if len(trav) != 2 {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("step %q: subworkflow target must have exactly 2 segments (subworkflow.<name>); got %d", stepName, len(trav)),
			Subject:  attr.Expr.Range().Ptr(),
		})
		return "", diags
	}

	nameAttr, nameOK := trav[1].(hcl.TraverseAttr)
	if !nameOK {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("step %q: subworkflow target name must be a bareword identifier (subworkflow.<name>)", stepName),
			Subject:  attr.Expr.Range().Ptr(),
		})
		return "", diags
	}

	ref := nameAttr.Name
	if _, ok := g.Subworkflows[ref]; !ok {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("step %q: referenced subworkflow %q is not declared", stepName, ref),
			Subject:  attr.Expr.Range().Ptr(),
		})
		return ref, diags
	}
	return ref, nil
}
