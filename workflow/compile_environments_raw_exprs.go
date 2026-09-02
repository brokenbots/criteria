package workflow

// compile_environments_raw_exprs.go — helpers to recover per-key HCL
// expressions from object-literal environment attributes so that a later
// taint pass can inspect them (CRI-88).

import (
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
)

// extractRawMapExprs returns the per-key expressions from a map/object literal
// expression. If the expression is an object cons ({ k = v, ... }) or a
// function call whose result is a map literal (e.g. merge({...})), it extracts
// the value expression for each literal-string key. Dynamic keys are skipped.
// For non-object expressions it returns a single entry mapping "" to the
// original expression so callers can still check the whole attribute for
// taint. The returned map is intended for taint validation only; callers must
// not use it for runtime evaluation.
func extractRawMapExprs(expr hcl.Expression) map[string]hcl.Expression {
	oc, ok := expr.(*hclsyntax.ObjectConsExpr)
	if !ok {
		return map[string]hcl.Expression{"": expr}
	}

	out := make(map[string]hcl.Expression, len(oc.Items))
	for _, item := range oc.Items {
		keyVal, diags := item.KeyExpr.Value(nil)
		if diags.HasErrors() || !keyVal.IsKnown() || keyVal.IsNull() || keyVal.Type() != cty.String {
			continue
		}
		out[keyVal.AsString()] = item.ValueExpr
	}
	return out
}
