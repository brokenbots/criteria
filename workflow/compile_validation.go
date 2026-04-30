package workflow

// compile_validation.go — schema-aware and permissive HCL attribute decode,
// plus field-type validation against declared AdapterInfo ConfigField schemas,
// and compile-time file() argument validation.

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/function"
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
			// silently producing an empty value.
			r := attr.Expr.StartRange()
			for _, ed := range d {
				if ed.Severity != hcl.DiagError {
					continue
				}
				diags = append(diags, &hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  ed.Summary,
					Detail:   ed.Detail,
					Subject:  &r,
				})
			}
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

// validateFileFunctionCalls evaluates expressions that have no variable
// references against a compile-time eval context. This catches constant-literal
// file() calls that reference non-existent or path-escaping paths before the
// workflow runs, producing HCL diagnostics with source ranges.
//
// Expressions that contain variable references (e.g. file(var.path)) are
// skipped — they are evaluated at runtime.
//
// workflowDir must be non-empty; callers are responsible for this precondition.
func validateFileFunctionCalls(attrs hcl.Attributes, workflowDir string) hcl.Diagnostics {
	var diags hcl.Diagnostics
	opts := DefaultFunctionOptions(workflowDir)
	ctx := &hcl.EvalContext{
		Functions: map[string]function.Function{
			"file":            fileValidateFunction(opts),
			"fileexists":      fileExistsFunction(opts),
			"trimfrontmatter": trimFrontmatterFunction(),
		},
	}
	for _, attr := range attrs {
		// Skip expressions with variable references — runtime handles them.
		if len(attr.Expr.Variables()) > 0 {
			continue
		}
		_, evalDiags := attr.Expr.Value(ctx)
		if evalDiags.HasErrors() {
			for _, d := range evalDiags {
				if d.Severity == hcl.DiagError {
					r := attr.Expr.StartRange()
					diags = append(diags, &hcl.Diagnostic{
						Severity: hcl.DiagError,
						Summary:  d.Summary,
						Detail:   d.Detail,
						Subject:  &r,
					})
				}
			}
		}
	}
	return diags
}

// fileValidateFunction is the compile-time variant of fileFunction. It
// performs path resolution, confinement checking, and a stat call (existence
// + readability) but does NOT read the file's content. This keeps compile-time
// validation fast even for large files.
func fileValidateFunction(opts FunctionOptions) function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{{Name: "path", Type: cty.String}},
		Type:   function.StaticReturnType(cty.String),
		Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
			if opts.WorkflowDir == "" {
				return cty.StringVal(""), fmt.Errorf("file(): workflow directory not configured")
			}
			raw := args[0].AsString()
			if filepath.IsAbs(raw) {
				return cty.StringVal(""), fmt.Errorf("file(): absolute paths are not supported; use a path relative to the workflow directory")
			}
			abs := filepath.Clean(filepath.Join(opts.WorkflowDir, raw))
			if err := checkConfinement("file()", raw, abs, opts.WorkflowDir, opts.AllowedPaths); err != nil {
				return cty.StringVal(""), err
			}
			resolved, err := filepath.EvalSymlinks(abs)
			if err != nil {
				return cty.StringVal(""), mapOSError(raw, err)
			}
			resolved = filepath.Clean(resolved)
			resolvedBase := evalSymlinksOrSelf(opts.WorkflowDir)
			resolvedAllowed := evalSymlinksAll(opts.AllowedPaths)
			if err := checkConfinement("file()", raw, resolved, resolvedBase, resolvedAllowed); err != nil {
				return cty.StringVal(""), err
			}
			if _, err := os.Stat(resolved); err != nil {
				return cty.StringVal(""), mapOSError(raw, err)
			}
			return cty.StringVal(""), nil
		},
	})
}

// knownAgentConfigFields maps adapter names to the set of fields that belong in
// the agent config block rather than a step input block. When an unknown
// step-input field matches this list, the diagnostic names the agent config
// block as the correct location rather than emitting the generic "unknown field"
// message.
//
// Adding a new adapter here costs one slice entry; adapters not in the map
// continue to emit the generic diagnostic.
var knownAgentConfigFields = map[string][]string{
	"copilot": {"model", "reasoning_effort", "system_prompt", "max_turns", "working_directory"},
	// future adapters extend this list
}

// validateSchemaAttrs validates raw HCL attributes against a ConfigField schema,
// attaching source ranges to diagnostics. It handles required/unknown key checks
// and type mismatch checks. Returns (decoded string map, diagnostics).
//
// adapterName is used to produce a targeted misplacement diagnostic when an
// unknown input field matches a known agent-config field for that adapter. Pass
// "" to emit the generic "unknown field" diagnostic for all unknown keys.
//
// When evalCtx is nil, expressions that fail to evaluate are stored as "" and
// the type check is deferred to runtime (step.input{} mode). When evalCtx is
// non-nil, the expression is evaluated against that context and any failure is
// reported as a hard diagnostic (agent.config{} mode — no runtime resolution).
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
			// silently producing an empty value.
			r := attr.Expr.StartRange()
			for _, ed := range d {
				if ed.Severity != hcl.DiagError {
					continue
				}
				diags = append(diags, &hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  ed.Summary,
					Detail:   ed.Detail,
					Subject:  &r,
				})
			}
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
		for _, known := range knownAgentConfigFields[adapterName] {
			if known == field {
				return &hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  fmt.Sprintf("%s: field %q is not valid in step input for adapter %q; it belongs in the agent config block:", context, field, adapterName),
					Detail:   fmt.Sprintf("  agent \"<name>\" {\n    adapter = %q\n    config {\n      %s = ...\n    }\n  }", adapterName, field),
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
