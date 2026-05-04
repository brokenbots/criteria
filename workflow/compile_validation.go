package workflow

// compile_validation.go — schema-aware and permissive HCL attribute decode,
// plus field-type validation against declared AdapterInfo ConfigField schemas,
// and compile-time file() argument validation.

import (
	"fmt"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
)

// decodeAttrsToStringMap converts pre-fetched hcl.Attributes into a map[string]string.
// Numbers and bools are converted to their string representations.
//
// When evalCtx is nil, attributes that cannot be evaluated without an EvalContext
// (e.g. variable references like "${var.env}") are stored as empty strings and
// deferred to runtime evaluation via InputExprs / BuildEvalContext (W04).
//
// When evalCtx is non-nil, the expression is evaluated against that context and
// any evaluation failure is reported as a hard diagnostic. This is the
// "compile-time-resolved" mode used for blocks like agent.config { } that have
// no runtime resolution path.
func decodeAttrsToStringMap(attrs hcl.Attributes, evalCtx *hcl.EvalContext) (map[string]string, hcl.Diagnostics) {
	var diags hcl.Diagnostics
	result := make(map[string]string, len(attrs))
	for k, attr := range attrs {
		val, d := attr.Expr.Value(evalCtx)
		if d.HasErrors() {
			if evalCtx == nil {
				// Expression needs an EvalContext (e.g. variable references).
				// Store an empty placeholder; the engine evaluates at step entry.
				result[k] = ""
				continue
			}
			// Compile-time-resolved mode: surface the failure rather than
			// silently producing an empty value. Preserve each diagnostic
			// verbatim so the original Subject/Context from HCL is kept.
			diags = append(diags, errorDiagsWithFallbackSubject(d, attr.Expr)...)
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

// errorDiagsWithFallbackSubject returns the error diagnostics from in,
// preserving each diagnostic's original Subject and Context. When a diagnostic
// has no Subject, the expression's full range is used as a fallback so the
// reported error always points at a specific location in the source.
func errorDiagsWithFallbackSubject(in hcl.Diagnostics, expr hcl.Expression) hcl.Diagnostics {
	var out hcl.Diagnostics
	for _, ed := range in {
		if ed == nil || ed.Severity != hcl.DiagError {
			continue
		}
		if ed.Subject != nil {
			out = append(out, ed)
			continue
		}
		fallback := expr.Range()
		clone := *ed
		clone.Subject = &fallback
		out = append(out, &clone)
	}
	return out
}

// validateFoldableAttrs validates all attributes in attrs using FoldExpr,
// catching unknown var/local references, type errors, and bad file() calls at
// compile time. Expressions that reference runtime-only namespaces (each,
// steps, shared_variable) are silently deferred — they are not errors.
//
// vars is the flat name→value map for the "var" namespace (may be nil).
// locals is the flat name→value map for the "local" namespace (may be nil).
// workflowDir is forwarded to FoldExpr for path-relative file() validation.
func validateFoldableAttrs(attrs hcl.Attributes, vars, locals map[string]cty.Value, workflowDir string) hcl.Diagnostics {
	var diags hcl.Diagnostics
	for _, attr := range attrs {
		_, foldable, d := FoldExpr(attr.Expr, vars, locals, workflowDir)
		if !foldable {
			// Runtime-deferred expression — not an error.
			continue
		}
		diags = append(diags, errorDiagsWithFallbackSubject(d, attr.Expr)...)
	}
	return diags
}

// knownAdapterConfigFields maps adapter names to the set of fields that belong in
// the adapter config block rather than a step input block. When an unknown
// step-input field matches this list, the diagnostic names the adapter config
// block as the correct location rather than emitting the generic "unknown field"
// message.
//
// Adding a new adapter here costs one slice entry; adapters not in the map
// continue to emit the generic diagnostic.
var knownAdapterConfigFields = map[string][]string{
	"copilot": {"model", "reasoning_effort", "system_prompt", "max_turns", "working_directory"},
	// future adapters extend this list
}

// validateSchemaAttrs validates raw HCL attributes against a ConfigField schema,
// attaching source ranges to diagnostics. It handles required/unknown key checks
// and type mismatch checks. Returns (decoded string map, diagnostics).
//
// adapterName is used to produce a targeted misplacement diagnostic when an
// unknown input field matches a known adapter-config field for that adapter. Pass
// "" to emit the generic "unknown field" diagnostic for all unknown keys.
//
// When evalCtx is nil, expressions that fail to evaluate are stored as "" and
// the type check is deferred to runtime (step.input{} mode). When evalCtx is
// non-nil, the expression is evaluated against that context and any failure is
// reported as a hard diagnostic (adapter.config{} mode — no runtime resolution).
func validateSchemaAttrs(context string, attrs hcl.Attributes, schema map[string]ConfigField, missingRange hcl.Range, adapterName string, evalCtx *hcl.EvalContext) (map[string]string, hcl.Diagnostics) { //nolint:funlen,gocognit,gocyclo // W03: exhaustive schema validation with per-adapter diagnostics
	var diags hcl.Diagnostics
	result := make(map[string]string, len(attrs))

	for k, attr := range attrs {
		field, known := schema[k]
		if len(schema) > 0 && !known {
			r := attr.NameRange
			diags = append(diags, unknownFieldDiagnostic(context, k, adapterName, r))
			continue
		}
		val, d := attr.Expr.Value(evalCtx)
		if d.HasErrors() {
			if evalCtx == nil {
				// Expression needs an EvalContext (e.g. variable references).
				// Store an empty placeholder; the engine evaluates at step entry.
				// Unknown-key check already ran above; type check is deferred to runtime.
				result[k] = ""
				continue
			}
			// Compile-time-resolved mode: surface the failure rather than
			// silently producing an empty value. Preserve the original
			// diagnostic's Subject/Context where available.
			diags = append(diags, errorDiagsWithFallbackSubject(d, attr.Expr)...)
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

// unknownFieldDiagnostic returns the appropriate diagnostic for an unknown field
// in a step input block. When the field is a known agent-config field for the
// given adapter, the diagnostic names the agent config block as the fix. Otherwise
// the generic "unknown field" message is returned.
func unknownFieldDiagnostic(context, field, adapterName string, r hcl.Range) *hcl.Diagnostic {
	if adapterName != "" {
		for _, known := range knownAdapterConfigFields[adapterName] {
			if known == field {
				return &hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  fmt.Sprintf("%s: field %q is not valid in step input for adapter %q; it belongs in the adapter config block", context, field, adapterName),
					Detail:   fmt.Sprintf("  adapter %q \"<name>\" {\n    config {\n      %s = ...\n    }\n  }", adapterName, field),
					Subject:  &r,
				}
			}
		}
	}
	return &hcl.Diagnostic{
		Severity: hcl.DiagError,
		Summary:  fmt.Sprintf("%s: unknown field %q", context, field),
		Subject:  &r,
	}
}
