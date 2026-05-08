package workflow

// compile_variables.go — variable block compilation, type parsing, and
// cty value coercion for default-value validation.

import (
	"fmt"
	"strings"

	"github.com/hashicorp/hcl/v2"
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
		typ, err := parseVariableType(vs.TypeStr)
		if err != nil {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("variable %q: %v", name, err)})
			continue
		}
		defaultVal := cty.NilVal
		if vs.Remain != nil {
			attrs, d := vs.Remain.JustAttributes()
			diags = append(diags, d...)
			if defAttr, ok := attrs["default"]; ok {
				var defDiags hcl.Diagnostics
				defaultVal, defDiags = defAttr.Expr.Value(nil)
				if defDiags.HasErrors() {
					diags = append(diags, defDiags...)
					defaultVal = cty.NilVal
				} else {
					// Coerce to declared type.
					defaultVal, err = convertCtyValue(defaultVal, typ)
					if err != nil {
						diags = append(diags, &hcl.Diagnostic{
							Severity: hcl.DiagError,
							Summary:  fmt.Sprintf("variable %q: default value does not match declared type %q: %v", name, vs.TypeStr, err),
						})
						defaultVal = cty.NilVal
					}
				}
			}
		}
		g.Variables[name] = &VariableNode{
			Name:        name,
			Type:        typ,
			Default:     defaultVal,
			Description: vs.Description,
		}
	}
	return diags
}

// parseVariableType converts a type string from a variable declaration into
// a cty.Type. Only the subset documented in W04 is supported.
func parseVariableType(typeStr string) (cty.Type, error) {
	switch strings.TrimSpace(typeStr) {
	case "", "string":
		return cty.String, nil
	case "number":
		return cty.Number, nil
	case "bool":
		return cty.Bool, nil
	case "list(string)":
		return cty.List(cty.String), nil
	case "list(number)":
		return cty.List(cty.Number), nil
	case "list(bool)":
		return cty.List(cty.Bool), nil
	case "map(string)":
		return cty.Map(cty.String), nil
	default:
		return cty.NilType, fmt.Errorf("unsupported type %q; supported: string, number, bool, list(string), list(number), list(bool), map(string)", typeStr)
	}
}

// TypeToString converts a cty.Type back to its declared type string representation.
// This is the inverse of parseVariableType: only types accepted by parseVariableType
// are supported. Returns an error for unsupported types instead of a fallback string,
// ensuring round-trip guarantees for type serialization.
func TypeToString(t cty.Type) (string, error) {
	switch {
	case t.Equals(cty.String):
		return "string", nil
	case t.Equals(cty.Number):
		return "number", nil
	case t.Equals(cty.Bool):
		return "bool", nil
	case t.IsListType():
		switch t.ElementType() {
		case cty.String:
			return "list(string)", nil
		case cty.Number:
			return "list(number)", nil
		case cty.Bool:
			return "list(bool)", nil
		}
	case t.IsMapType():
		if t.ElementType().Equals(cty.String) {
			return "map(string)", nil
		}
	}
	// Unsupported type: return error instead of falling back to FriendlyName().
	// This ensures TypeToString is a strict inverse of parseVariableType.
	return "", fmt.Errorf("unsupported type %s; supported: string, number, bool, list(string), list(number), list(bool), map(string)", t.FriendlyName())
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
