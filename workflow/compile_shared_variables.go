package workflow

// compile_shared_variables.go — compile path for shared_variable "<name>" blocks.
// shared_variable blocks declare runtime-mutable, workflow-scoped values that are
// read-write throughout the run under engine-managed locking.

import (
	"fmt"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
)

// compileSharedVariables compiles all shared_variable blocks from spec into
// g.SharedVariables. Must be called after compileVariables and compileLocals
// so that name-collision checks against both namespaces are possible.
func compileSharedVariables(g *FSMGraph, spec *Spec, opts CompileOpts) hcl.Diagnostics {
	if len(spec.SharedVariables) == 0 {
		return nil
	}

	var diags hcl.Diagnostics

	for _, sv := range spec.SharedVariables {
		name := sv.Name

		if d := checkSharedVarNameCollisions(g, name); d != nil {
			diags = append(diags, d)
			continue
		}

		typ, typDiags := compileSharedVarType(name, sv.TypeStr)
		diags = append(diags, typDiags...)
		if typDiags.HasErrors() {
			continue
		}

		initialVal, valDiags, skip := compileSharedVarInitialValue(name, sv.Remain, typ, g, opts)
		diags = append(diags, valDiags...)
		if skip {
			continue
		}

		g.SharedVariables[name] = &SharedVariableNode{
			Name:         name,
			Type:         typ,
			InitialValue: initialVal,
			Description:  sv.Description,
		}
		g.SharedVariableOrder = append(g.SharedVariableOrder, name)
	}

	return diags
}

// checkSharedVarNameCollisions returns an error diagnostic if name already
// exists in the variable, local, or shared_variable namespaces.
func checkSharedVarNameCollisions(g *FSMGraph, name string) *hcl.Diagnostic {
	if _, ok := g.Variables[name]; ok {
		return &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("shared_variable %q: name conflicts with a declared variable", name)}
	}
	if _, ok := g.Locals[name]; ok {
		return &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("shared_variable %q: name conflicts with a declared local", name)}
	}
	if _, ok := g.SharedVariables[name]; ok {
		return &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("duplicate shared_variable %q", name)}
	}
	return nil
}

// compileSharedVarType parses the TypeStr attribute of a shared_variable block
// and returns the resolved cty.Type plus any diagnostics.
func compileSharedVarType(name, typeStr string) (cty.Type, hcl.Diagnostics) {
	if typeStr == "" {
		return cty.NilType, hcl.Diagnostics{&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("shared_variable %q: attribute \"type\" is required", name),
		}}
	}
	typ, err := parseVariableType(typeStr)
	if err != nil {
		return cty.NilType, hcl.Diagnostics{&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("shared_variable %q: %v", name, err),
		}}
	}
	return typ, nil
}

// compileSharedVarInitialValue parses and validates the optional "value"
// attribute from remain. Returns (value, diags, shouldSkip). shouldSkip is true
// when diags has a fatal error that means this shared_variable should be
// skipped (the caller must not register it).
func compileSharedVarInitialValue(name string, remain hcl.Body, typ cty.Type, g *FSMGraph, opts CompileOpts) (cty.Value, hcl.Diagnostics, bool) {
	initialVal := cty.NullVal(typ)
	if remain == nil {
		return initialVal, nil, false
	}

	var diags hcl.Diagnostics
	attrs, d := remain.JustAttributes()
	diags = append(diags, d...)

	for k, attr := range attrs {
		if k != "value" {
			r := attr.NameRange
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("shared_variable %q: unknown attribute %q; only \"value\" is allowed", name, k),
				Subject:  &r,
			})
		}
	}

	valAttr, ok := attrs["value"]
	if !ok {
		return initialVal, diags, false
	}

	folded, valDiags, skip := validateFoldedInitialValue(name, valAttr, typ, g, opts)
	return folded, append(diags, valDiags...), skip
}

// validateFoldedInitialValue folds and type-checks the value expression for a
// shared_variable declaration. Returns (folded value, diags, shouldSkip).
func validateFoldedInitialValue(name string, valAttr *hcl.Attribute, typ cty.Type, g *FSMGraph, opts CompileOpts) (cty.Value, hcl.Diagnostics, bool) {
	var diags hcl.Diagnostics
	folded, foldable, foldDiags := FoldExpr(valAttr.Expr, graphVars(g), graphLocals(g), opts.WorkflowDir)
	diags = append(diags, foldDiags...)
	if foldDiags.HasErrors() {
		return cty.NullVal(typ), diags, true
	}
	if !foldable {
		r := valAttr.Expr.StartRange()
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("shared_variable %q: initial value must be a compile-time constant expression; references to each, steps, or shared are not allowed", name),
			Subject:  &r,
		})
		return cty.NullVal(typ), diags, true
	}
	if folded == cty.NilVal || !folded.IsKnown() {
		r := valAttr.Expr.StartRange()
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("shared_variable %q: initial value could not be fully resolved at compile time", name),
			Subject:  &r,
		})
		return cty.NullVal(typ), diags, true
	}
	if !folded.Type().Equals(typ) {
		r := valAttr.Expr.StartRange()
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("shared_variable %q: initial value type %s does not match declared type %s", name, folded.Type().FriendlyName(), typ.FriendlyName()),
			Subject:  &r,
		})
		return cty.NullVal(typ), diags, true
	}
	return folded, diags, false
}

// compileSharedWritesAttr decodes and validates a shared_writes = { var_name = "output_key" }
// attribute from an outcome block. Every key must reference a declared shared_variable;
// unknown keys are compile errors. When knownOutputKeys is non-nil, every value (output key)
// must appear in the set; unknown values are compile errors. Returns the decoded map.
func compileSharedWritesAttr(stepName, outcomeName string, attr *hcl.Attribute, g *FSMGraph, knownOutputKeys map[string]bool) (map[string]string, hcl.Diagnostics) {
	var diags hcl.Diagnostics
	val, evalDiags := attr.Expr.Value(nil)
	if evalDiags.HasErrors() {
		return nil, append(diags, evalDiags...)
	}
	r := attr.Expr.StartRange()
	if val.IsNull() || !val.IsKnown() {
		return nil, hcl.Diagnostics{&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("step %q outcome %q: shared_writes must be a map literal", stepName, outcomeName),
			Subject:  &r,
		}}
	}
	if !val.Type().IsObjectType() && !val.Type().IsMapType() {
		return nil, hcl.Diagnostics{&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("step %q outcome %q: shared_writes must be an object/map; got %s", stepName, outcomeName, val.Type().FriendlyName()),
			Subject:  &r,
		}}
	}

	writes := make(map[string]string)
	for it := val.ElementIterator(); it.Next(); {
		k, v := it.Element()
		varName := k.AsString()
		outputKey, d := validateSharedWriteEntry(stepName, outcomeName, varName, v, r, g, knownOutputKeys)
		if d != nil {
			diags = append(diags, d)
			continue
		}
		writes[varName] = outputKey
	}

	if len(writes) == 0 && !diags.HasErrors() {
		return nil, nil
	}
	return writes, diags
}

// validateSharedWriteEntry validates one entry in a shared_writes map.
// Returns (outputKey, nil) on success, or ("", diagnostic) on failure.
func validateSharedWriteEntry(stepName, outcomeName, varName string, v cty.Value, exprRange hcl.Range, g *FSMGraph, knownOutputKeys map[string]bool) (string, *hcl.Diagnostic) {
	if _, declared := g.SharedVariables[varName]; !declared {
		return "", &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("step %q outcome %q: shared_writes key %q does not reference a declared shared_variable", stepName, outcomeName, varName),
			Subject:  &exprRange,
		}
	}
	if !v.IsKnown() || v.IsNull() || v.Type() != cty.String {
		return "", &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("step %q outcome %q: shared_writes value for %q must be a string (the output key name)", stepName, outcomeName, varName),
			Subject:  &exprRange,
		}
	}
	outputKey := v.AsString()
	if knownOutputKeys != nil && !knownOutputKeys[outputKey] {
		return "", &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("step %q outcome %q: shared_writes %q maps to output key %q which is not declared in the output projection", stepName, outcomeName, varName, outputKey),
			Subject:  &exprRange,
		}
	}
	return outputKey, nil
}
