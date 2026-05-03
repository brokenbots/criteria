package workflow

// compile_outputs.go — compile path for output "<name>" blocks.
// Outputs are top-level or inline workflow-body declarations that specify
// named values produced when a workflow reaches a terminal state.

import (
	"fmt"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
)

// compileOutputs decodes each output{ value=... } block, validates the value
// expression's free variables (must be in var/local/each/steps/shared_variable),
// parses optional type and description attributes, and stores the compiled
// output in g.Outputs.
//
// Must be called after compileLocals so g.Locals is fully populated.
// The value expression is captured as hcl.Expression for runtime evaluation
// (it may reference steps.* which is runtime-only).
func compileOutputs(g *FSMGraph, spec *Spec, opts CompileOpts) hcl.Diagnostics {
	if len(spec.Outputs) == 0 {
		return nil
	}

	var diags hcl.Diagnostics

	// Check for duplicate names.
	seen := make(map[string]bool, len(spec.Outputs))
	for _, os := range spec.Outputs {
		if seen[os.Name] {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("duplicate output %q", os.Name),
			})
			continue
		}
		seen[os.Name] = true
	}

	if diags.HasErrors() {
		return diags
	}

	// Compile each output in declaration order.
	for _, os := range spec.Outputs {
		d := compileOneOutput(g, os, opts)
		diags = append(diags, d...)
	}

	return diags
}

// compileOneOutput compiles a single output declaration.
func compileOneOutput(g *FSMGraph, os OutputSpec, opts CompileOpts) hcl.Diagnostics {
	var diags hcl.Diagnostics

	// Extract attributes from the Remain body.
	attrs, d := os.Remain.JustAttributes()
	diags = append(diags, d...)

	// Validate attribute names and extract value.
	valAttr, ok := validateOutputAttrs(os.Name, attrs, &diags)
	if !ok {
		return diags
	}

	// Parse and validate type attribute from schema (os.TypeStr).
	declaredType := cty.NilType
	if os.TypeStr != "" {
		parsedType, err := parseVariableType(os.TypeStr)
		if err != nil {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("output %q: %v", os.Name, err),
			})
			return diags
		}
		declaredType = parsedType
	}

	// Validate and fold the value expression.
	d = validateOutputValue(os.Name, valAttr, declaredType, g, opts)
	diags = append(diags, d...)
	if diags.HasErrors() {
		return diags
	}

	// Store the compiled output node.
	g.Outputs[os.Name] = &OutputNode{
		Name:         os.Name,
		Description:  os.Description,
		DeclaredType: declaredType,
		Value:        valAttr.Expr,
	}
	g.OutputOrder = append(g.OutputOrder, os.Name)

	return diags
}

// validateOutputValue validates the value expression and its type match.
func validateOutputValue(name string, valAttr *hcl.Attribute, declaredType cty.Type, g *FSMGraph, opts CompileOpts) hcl.Diagnostics {
	var result hcl.Diagnostics

	// Validate the value expression's free variables.
	d := validateFoldableAttrs(hcl.Attributes{"value": valAttr}, graphVars(g), graphLocals(g), opts.WorkflowDir)
	result = append(result, d...)
	if result.HasErrors() {
		return result
	}

	// Try to fold the value expression. If it folds, validate the type match.
	valueExpr := valAttr.Expr
	val, foldable, d := FoldExpr(valueExpr, graphVars(g), graphLocals(g), opts.WorkflowDir)
	result = append(result, d...)
	if result.HasErrors() {
		return result
	}

	// If foldable at compile time, validate declared type match.
	if foldable && declaredType != cty.NilType {
		if !val.Type().Equals(declaredType) {
			r := valueExpr.StartRange()
			result = append(result, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("output %q: value is %s but declared type is %s", name, val.Type().FriendlyName(), declaredType.FriendlyName()),
				Subject:  &r,
			})
		}
	}

	return result
}

// validateOutputAttrs checks output attribute names are known and returns the value attribute.
func validateOutputAttrs(name string, attrs hcl.Attributes, diags *hcl.Diagnostics) (*hcl.Attribute, bool) {
	allowedAttrs := map[string]bool{
		"value":       true,
		"description": true,
	}
	for k, attr := range attrs {
		if !allowedAttrs[k] {
			r := attr.NameRange
			*diags = append(*diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("output %q: unknown attribute %q; only \"value\" and \"description\" are allowed", name, k),
				Subject:  &r,
			})
		}
	}

	valAttr, ok := attrs["value"]
	if !ok {
		*diags = append(*diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("output %q: required attribute \"value\" is missing", name),
		})
		return nil, false
	}
	return valAttr, true
}
