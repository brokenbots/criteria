package workflow

// compile_variables.go — variable block compilation, type parsing, and
// cty value coercion for default-value validation.

import (
	"fmt"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/ext/typeexpr"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/convert"
)

// compileVariables compiles all variable blocks from spec into g.Variables.
func compileVariables(g *FSMGraph, spec *Spec) hcl.Diagnostics {
	var diags hcl.Diagnostics
	for _, vs := range spec.Variables {
		name := vs.Name
		if _, dup := g.Variables[name]; dup {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("duplicate variable %q", name)})
			continue
		}
		typ, defs, typeDiags := resolveVariableType(vs)
		if typeDiags.HasErrors() {
			diags = append(diags, typeDiags...)
			continue
		}
		defaultVal, defaultDiags := resolveVariableDefault(vs, typ, defs)
		if defaultDiags.HasErrors() {
			diags = append(diags, defaultDiags...)
			continue
		}
		g.Variables[name] = &VariableNode{
			Name:         name,
			Type:         typ,
			TypeDefaults: defs,
			Default:      defaultVal,
			Description:  vs.Description,
		}
	}
	return diags
}

// resolveVariableType returns the cty.Type (and any optional defaults) for a
// variable spec, defaulting to cty.String when the type expression is absent.
func resolveVariableType(vs VariableSpec) (cty.Type, *typeexpr.Defaults, hcl.Diagnostics) {
	if isAbsentExpr(vs.Type) {
		return cty.String, nil, nil
	}
	return resolveTypeConstraint(vs.Type)
}

// resolveVariableDefault extracts and coerces the optional default value from
// a variable spec's Remain body against the declared type.  If defs is non-nil,
// type-level optional defaults are applied to the folded value before coercion.
func resolveVariableDefault(vs VariableSpec, typ cty.Type, defs *typeexpr.Defaults) (cty.Value, hcl.Diagnostics) {
	var diags hcl.Diagnostics
	if vs.Remain == nil {
		return cty.NilVal, diags
	}
	attrs, d := vs.Remain.JustAttributes()
	diags = append(diags, d...)
	defAttr, ok := attrs["default"]
	if !ok {
		return cty.NilVal, diags
	}
	defaultVal, defDiags := defAttr.Expr.Value(nil)
	if defDiags.HasErrors() {
		diags = append(diags, defDiags...)
		return cty.NilVal, diags
	}
	defaultVal = applyDefaultsIfAny(defaultVal, defs)
	coerced, err := convertCtyValue(defaultVal, typ)
	if err != nil {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("variable %q: default value does not match declared type %s: %v", vs.Name, typ.FriendlyName(), err),
		})
		return cty.NilVal, diags
	}
	return coerced, diags
}

// convertCtyValue coerces v to typ. Exact type matches are accepted immediately.
// When the types differ, go-cty's convert package is used as a fallback so that
// HCL tuple literals (the type produced by `[a, b]` expressions) are accepted
// as default values for list-typed variables.
func convertCtyValue(v cty.Value, typ cty.Type) (cty.Value, error) {
	if v.Type().Equals(typ) {
		return v, nil
	}
	converted, err := convert.Convert(v, typ)
	if err != nil {
		return cty.NilVal, fmt.Errorf("default value is %s but variable is declared as %s", v.Type().FriendlyName(), typ.FriendlyName())
	}
	return converted, nil
}

// isListStringValue reports whether val is a list(string) or tuple-of-strings.
func isListStringValue(val cty.Value) bool {
	t := val.Type()
	if t.IsListType() {
		return t.ElementType() == cty.String
	}
	if !t.IsTupleType() {
		return false
	}
	for _, et := range t.TupleElementTypes() {
		if et != cty.String {
			return false
		}
	}
	return true
}

// isAbsentExpr reports whether an optional hcl.Expression decoded by gohcl
// was actually absent. gohcl sets absent optional expression fields to a
// zero-length-range sentinel rather than nil.
func isAbsentExpr(expr hcl.Expression) bool {
	if expr == nil {
		return true
	}
	rng := expr.Range()
	return rng.Start == rng.End
}
