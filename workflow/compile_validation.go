package workflow

// compile_validation.go — schema-aware and permissive HCL attribute decode,
// plus field-type validation against declared AdapterInfo ConfigField schemas.

import (
	"fmt"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
)

// decodeAttrsToStringMap converts pre-fetched hcl.Attributes into a map[string]string.
// Numbers and bools are converted to their string representations.
// Attributes that cannot be evaluated without an EvalContext (e.g. variable
// references like "${var.env}") are stored as empty strings and deferred to
// runtime evaluation via InputExprs / BuildEvalContext (W04).
func decodeAttrsToStringMap(attrs hcl.Attributes) (map[string]string, hcl.Diagnostics) {
	var diags hcl.Diagnostics
	result := make(map[string]string, len(attrs))
	for k, attr := range attrs {
		val, d := attr.Expr.Value(nil)
		if d.HasErrors() {
			// Expression needs an EvalContext (e.g. variable references).
			// Store an empty placeholder; the engine evaluates at step entry.
			result[k] = ""
			continue
		}
		diags = append(diags, d...)
		switch val.Type() {
		case cty.String:
			result[k] = val.AsString()
		case cty.Number:
			bf := val.AsBigFloat()
			result[k] = bf.Text('f', -1)
		case cty.Bool:
			if val.True() {
				result[k] = "true"
			} else {
				result[k] = "false"
			}
		default:
			result[k] = val.GoString()
		}
	}
	return result, diags
}

// decodeBodyToStringMap converts an hcl.Body of key = "value" attributes into
// a map[string]string. Numbers and bools are converted to their string
// representations. Expression references (variables, functions) that cannot be
// evaluated without a context are deferred to W04.
func decodeBodyToStringMap(body hcl.Body) (map[string]string, hcl.Diagnostics) {
	if body == nil {
		return nil, nil
	}
	attrs, diags := body.JustAttributes()
	if diags.HasErrors() {
		return nil, diags
	}
	return decodeAttrsToStringMap(attrs)
}

// validateSchemaAttrs validates raw HCL attributes against a ConfigField schema,
// attaching source ranges to diagnostics. It handles required/unknown key checks
// and type mismatch checks. Returns (decoded string map, diagnostics).
func validateSchemaAttrs(context string, attrs hcl.Attributes, schema map[string]ConfigField, missingRange hcl.Range) (map[string]string, hcl.Diagnostics) {
	var diags hcl.Diagnostics
	result := make(map[string]string, len(attrs))

	for k, attr := range attrs {
		field, known := schema[k]
		if len(schema) > 0 && !known {
			r := attr.NameRange
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("%s: unknown field %q", context, k),
				Subject:  &r,
			})
			continue
		}
		val, d := attr.Expr.Value(nil)
		if d.HasErrors() {
			// Expression needs an EvalContext (e.g. variable references).
			// Store an empty placeholder; the engine evaluates at step entry.
			// Unknown-key check already ran above; type check is deferred to runtime.
			result[k] = ""
			continue
		}
		diags = append(diags, d...)
		// Type check against declared schema type.
		if len(schema) > 0 {
			r := attr.Expr.StartRange()
			switch field.Type {
			case ConfigFieldNumber:
				if val.Type() != cty.Number {
					diags = append(diags, &hcl.Diagnostic{
						Severity: hcl.DiagError,
						Summary:  fmt.Sprintf("%s: field %q must be a number", context, k),
						Subject:  &r,
					})
					continue
				}
			case ConfigFieldBool:
				if val.Type() != cty.Bool {
					diags = append(diags, &hcl.Diagnostic{
						Severity: hcl.DiagError,
						Summary:  fmt.Sprintf("%s: field %q must be a bool", context, k),
						Subject:  &r,
					})
					continue
				}
			case ConfigFieldListString:
				if !isListStringValue(val) {
					diags = append(diags, &hcl.Diagnostic{
						Severity: hcl.DiagError,
						Summary:  fmt.Sprintf("%s: field %q must be a list of strings", context, k),
						Subject:  &r,
					})
					continue
				}
			case ConfigFieldString:
				if val.Type() != cty.String {
					diags = append(diags, &hcl.Diagnostic{
						Severity: hcl.DiagError,
						Summary:  fmt.Sprintf("%s: field %q must be a string", context, k),
						Subject:  &r,
					})
					continue
				}
			}
		}
		// Coerce to string for the output map.
		switch val.Type() {
		case cty.String:
			result[k] = val.AsString()
		case cty.Number:
			bf := val.AsBigFloat()
			result[k] = bf.Text('f', -1)
		case cty.Bool:
			if val.True() {
				result[k] = "true"
			} else {
				result[k] = "false"
			}
		default:
			result[k] = val.GoString()
		}
	}

	// Check required fields. Use attrs for presence check so that expression-
	// valued attributes (deferred to runtime) are not reported as missing.
	for k, field := range schema {
		if field.Required {
			if _, present := attrs[k]; !present {
				var subject *hcl.Range
				if missingRange.Filename != "" {
					r := missingRange
					subject = &r
				}
				diags = append(diags, &hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  fmt.Sprintf("%s: required field %q is missing", context, k),
					Subject:  subject,
				})
			}
		}
	}

	return result, diags
}
