package workflow

// compile_steps_adapter_ref.go — traversal-based step adapter reference resolution.
// This helper validates and resolves step adapter references from HCL traversal
// expressions (not string literals), e.g. adapter = adapter.shell.default.
// It is reused by [14-universal-step-target.md](14-universal-step-target.md) for
// the universal target attribute.

import (
	"fmt"

	"github.com/hashicorp/hcl/v2"
)

// ResolveStepAdapterRef extracts and validates a step's adapter reference from
// the step's Remain hcl.Body. The adapter attribute, if present, must be an HCL
// traversal expression (not a string literal) of the form adapter.<type>.<name>.
//
// Returns:
//   - adapterRef: the combined "<type>.<name>" string ready for FSMGraph key lookup.
//   - present: true if an adapter attribute was found; false if absent.
//   - diags: diagnostics for validation errors (string literals, wrong shape, etc.).
//
// If an adapter attribute is found but invalid, present=true and diags contains errors.
// If no adapter attribute is found, present=false and diags is empty.
func ResolveStepAdapterRef(body hcl.Body) (adapterRef string, present bool, diags hcl.Diagnostics) {
	if body == nil {
		return "", false, nil
	}

	attrs, attrDiags := body.JustAttributes()
	diags = append(diags, attrDiags...)

	attr, ok := attrs["adapter"]
	if !ok {
		return "", false, nil
	}

	// Attribute found; now validate it is a traversal (not a string literal).
	trav, traversalDiags := hcl.AbsTraversalForExpr(attr.Expr)
	diags = append(diags, traversalDiags...)

	if len(traversalDiags) > 0 {
		// Expression is not a valid traversal (e.g., a string literal or function call).
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "adapter reference must be a bareword traversal",
			Detail:   `adapter reference must take the form adapter.<type>.<name>, e.g., adapter = adapter.shell.default (not a string literal like "shell.default")`,
			Subject:  attr.Expr.Range().Ptr(),
		})
		return "", true, diags
	}

	typeName, nameStr, shapeDiags := validateAdapterTraversalShape(trav, attr)
	diags = append(diags, shapeDiags...)
	if shapeDiags.HasErrors() {
		return "", true, diags
	}

	adapterRef = fmt.Sprintf("%s.%s", typeName, nameStr)
	return adapterRef, true, nil
}

// validateAdapterTraversalShape validates that trav has exactly 3 segments,
// is rooted at "adapter", and that segments 1 and 2 are bareword identifiers.
// Returns the type and name strings on success.
func validateAdapterTraversalShape(trav hcl.Traversal, attr *hcl.Attribute) (typeName, name string, diags hcl.Diagnostics) {
	if len(trav) != 3 {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "adapter reference has wrong shape",
			Detail:   fmt.Sprintf("adapter reference must have exactly 3 segments (adapter.<type>.<name>); got %d", len(trav)),
			Subject:  attr.Expr.Range().Ptr(),
		})
		return "", "", diags
	}

	if trav.RootName() != "adapter" {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "adapter reference must start with 'adapter'",
			Detail:   `adapter reference must take the form adapter.<type>.<name>`,
			Subject:  attr.Expr.Range().Ptr(),
		})
		return "", "", diags
	}

	// Validate both remaining segments are attribute traversals (not index, etc).
	typeAttr, typeOK := trav[1].(hcl.TraverseAttr)
	nameAttr, nameOK := trav[2].(hcl.TraverseAttr)
	if !typeOK || !nameOK {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "adapter reference segments must be bareword identifiers",
			Detail:   `adapter reference must take the form adapter.<type>.<name>; segments after "adapter" must be identifiers, not indexing or function calls`,
			Subject:  attr.Expr.Range().Ptr(),
		})
		return "", "", diags
	}

	return typeAttr.Name, nameAttr.Name, nil
}
