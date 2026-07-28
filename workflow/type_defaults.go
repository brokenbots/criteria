package workflow

import (
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/ext/typeexpr"
	"github.com/zclconf/go-cty/cty"
)

// resolveTypeConstraint parses an HCL type expression that may contain optional
// attribute modifiers with default values (e.g. object({ a = optional(string, "hello") })).
// Returns cty.String and nil defaults when expr is absent.
func resolveTypeConstraint(expr hcl.Expression) (cty.Type, *typeexpr.Defaults, hcl.Diagnostics) {
	if isAbsentExpr(expr) {
		return cty.String, nil, nil
	}
	typ, defs, diags := typeexpr.TypeConstraintWithDefaults(expr)
	if diags.HasErrors() {
		return cty.NilType, nil, diags
	}
	return typ, defs, nil
}

// ApplyDefaultsIfAny applies type-level optional defaults to v when defs
// is non-nil. It is a thin exported wrapper around applyDefaultsIfAny so
// that the runtime engine can apply defaults before type conversion.
func ApplyDefaultsIfAny(v cty.Value, defs *typeexpr.Defaults) cty.Value {
	return applyDefaultsIfAny(v, defs)
}

func applyDefaultsIfAny(v cty.Value, defs *typeexpr.Defaults) cty.Value {
	if defs == nil {
		return v
	}
	return defs.Apply(v)
}
