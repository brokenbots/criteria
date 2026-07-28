package workflow

// compile_data.go — compile path for data "<kind>" "<name>" blocks.
// data blocks declare workflow-scoped values. Only kind = "internal" is
// supported today; future kinds (e.g. "http") will be read-only.

import (
	"fmt"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/ext/typeexpr"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/convert"
)

// compileData compiles all data blocks from spec into g.Data. Must be called
// after compileVariables and compileLocals so that name-collision checks
// against both namespaces are possible.
func compileData(g *FSMGraph, spec *Spec, opts CompileOpts) hcl.Diagnostics {
	if len(spec.Data) == 0 {
		return nil
	}

	var diags hcl.Diagnostics
	for i := range spec.Data {
		diags = append(diags, compileDataBlock(g, &spec.Data[i], opts)...)
	}
	return diags
}

// compileDataBlock compiles a single data block and registers it on g.
func compileDataBlock(g *FSMGraph, ds *DataSpec, opts CompileOpts) hcl.Diagnostics {
	kind := ds.Kind
	name := ds.Name

	if kind != "internal" {
		return hcl.Diagnostics{&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("unsupported data kind %q; only \"internal\" is currently supported", kind),
		}}
	}

	if d := checkDataNameCollisions(g, kind, name); d != nil {
		return hcl.Diagnostics{d}
	}

	typ, defs, typDiags := compileDataType(kind, name, ds.Type)
	if typDiags.HasErrors() {
		return typDiags
	}
	diags := typDiags

	initialVal, valDiags, skip := compileDataInitialValue(kind, name, ds.Remain, typ, defs, g, opts)
	diags = append(diags, valDiags...)
	if skip {
		return diags
	}

	secret, secretDiags := compileDataSecret(kind, name, ds.Remain)
	diags = append(diags, secretDiags...)

	if g.Data[kind] == nil {
		g.Data[kind] = make(map[string]*DataNode)
	}
	g.Data[kind][name] = &DataNode{
		Kind:         kind,
		Name:         name,
		Type:         typ,
		TypeDefaults: defs,
		InitialValue: initialVal,
		Description:  ds.Description,
		Secret:       secret,
	}
	g.DataOrder = append(g.DataOrder, DataRef{Kind: kind, Name: name})
	return diags
}

// checkDataNameCollisions returns an error diagnostic if name already
// exists in the variable, local, or data namespaces.
func checkDataNameCollisions(g *FSMGraph, kind, name string) *hcl.Diagnostic {
	if _, ok := g.Variables[name]; ok {
		return &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("data %q %q: name conflicts with a declared variable", kind, name)}
	}
	if _, ok := g.Locals[name]; ok {
		return &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("data %q %q: name conflicts with a declared local", kind, name)}
	}
	if g.Data[kind] != nil {
		if _, ok := g.Data[kind][name]; ok {
			return &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("duplicate data %q %q", kind, name)}
		}
	}
	// Also check across all kinds for the same name to avoid confusion.
	for otherKind, m := range g.Data {
		if otherKind != kind {
			if _, ok := m[name]; ok {
				return &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("data %q %q: name conflicts with data %q %q", kind, name, otherKind, name)}
			}
		}
	}
	return nil
}

// compileDataSecret reads the optional "secret" boolean attribute from a data
// block body. A secret data block is treated as a taint source by TaintPass:
// its value may only flow through secret channels (secret_input, adapter
// secrets) and is rejected in non-secret inputs (D65). Defaults to false.
func compileDataSecret(kind, name string, remain hcl.Body) (bool, hcl.Diagnostics) {
	if remain == nil {
		return false, nil
	}
	var diags hcl.Diagnostics
	attrs, _ := remain.JustAttributes()
	secretAttr, ok := attrs["secret"]
	if !ok {
		return false, nil
	}
	val, valDiags := secretAttr.Expr.Value(nil)
	if valDiags.HasErrors() {
		diags = append(diags, valDiags...)
		return false, diags
	}
	if val.Type() != cty.Bool || val.IsNull() || !val.IsKnown() {
		r := secretAttr.NameRange
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("data %q %q: \"secret\" must be a constant boolean", kind, name),
			Subject:  &r,
		})
		return false, diags
	}
	return val.True(), diags
}

// compileDataType parses the Type expression of a data block and returns the
// resolved cty.Type, optional defaults, plus any diagnostics.
func compileDataType(kind, name string, typeExpr hcl.Expression) (cty.Type, *typeexpr.Defaults, hcl.Diagnostics) {
	if isAbsentExpr(typeExpr) {
		return cty.NilType, nil, hcl.Diagnostics{&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("data %q %q: attribute \"type\" is required", kind, name),
		}}
	}
	typ, defs, typeDiags := ResolveTypeConstraint(typeExpr)
	if typeDiags.HasErrors() {
		return cty.NilType, nil, typeDiags
	}
	return typ, defs, nil
}

// compileDataInitialValue parses and validates the optional "value"
// attribute from remain. Returns (value, diags, shouldSkip). shouldSkip is true
// when diags has a fatal error that means this data block should be
// skipped (the caller must not register it).
func compileDataInitialValue(kind, name string, remain hcl.Body, typ cty.Type, defs *typeexpr.Defaults, g *FSMGraph, opts CompileOpts) (cty.Value, hcl.Diagnostics, bool) {
	initialVal := cty.NullVal(typ)
	if remain == nil {
		return initialVal, nil, false
	}

	var diags hcl.Diagnostics
	attrs, d := remain.JustAttributes()
	diags = append(diags, d...)

	for k, attr := range attrs {
		if k != "value" && k != "description" && k != "secret" {
			r := attr.NameRange
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("data %q %q: unknown attribute %q; only \"value\", \"description\", and \"secret\" are allowed", kind, name, k),
				Subject:  &r,
			})
		}
	}

	valAttr, ok := attrs["value"]
	if !ok {
		return initialVal, diags, false
	}

	folded, valDiags, skip := validateFoldedDataInitialValue(kind, name, valAttr, typ, defs, g, opts)
	return folded, append(diags, valDiags...), skip
}

// validateFoldedDataInitialValue folds and type-checks the value expression for a
// data declaration. Returns (folded value, diags, shouldSkip).
func validateFoldedDataInitialValue(kind, name string, valAttr *hcl.Attribute, typ cty.Type, defs *typeexpr.Defaults, g *FSMGraph, opts CompileOpts) (cty.Value, hcl.Diagnostics, bool) {
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
			Summary:  fmt.Sprintf("data %q %q: initial value must be a compile-time constant expression; references to each, steps, or data are not allowed", kind, name),
			Subject:  &r,
		})
		return cty.NullVal(typ), diags, true
	}
	if folded == cty.NilVal || !folded.IsKnown() {
		r := valAttr.Expr.StartRange()
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("data %q %q: initial value could not be fully resolved at compile time", kind, name),
			Subject:  &r,
		})
		return cty.NullVal(typ), diags, true
	}
	folded = applyDefaultsIfAny(folded, defs)
	coerced, err := convert.Convert(folded, typ)
	if err != nil {
		r := valAttr.Expr.StartRange()
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("data %q %q: initial value type %s does not match declared type %s", kind, name, folded.Type().FriendlyName(), typ.FriendlyName()),
			Subject:  &r,
		})
		return cty.NullVal(typ), diags, true
	}
	return coerced, diags, false
}

// compileWrites compiles the write blocks inside an outcome. Each target must
// be a four-segment traversal data.<kind>.<name>.value. Returns compiled writes
// and any diagnostics.
func compileWrites(stepName, outcomeName string, writes []WriteSpec, g *FSMGraph, knownOutputKeys map[string]bool) ([]CompiledWrite, hcl.Diagnostics) {
	var diags hcl.Diagnostics
	var result []CompiledWrite
	for i, w := range writes {
		cw, d := compileWrite(stepName, outcomeName, i, w, g, knownOutputKeys)
		diags = append(diags, d...)
		if cw != nil {
			result = append(result, *cw)
		}
	}
	return result, diags
}

// compileWrite validates a single WriteSpec and returns a CompiledWrite.
func compileWrite(stepName, outcomeName string, idx int, w WriteSpec, g *FSMGraph, knownOutputKeys map[string]bool) (*CompiledWrite, hcl.Diagnostics) {
	var diags hcl.Diagnostics
	loc := fmt.Sprintf("write[%d]", idx)

	kind, name, d := resolveWriteTarget(stepName, outcomeName, loc, w.Target, g)
	diags = append(diags, d...)
	if kind == "" {
		return nil, diags
	}

	if knownOutputKeys != nil {
		diags = append(diags, validateWriteOutputRefs(stepName, outcomeName, loc, w.Value, knownOutputKeys)...)
	}

	return &CompiledWrite{
		DataKind:  kind,
		DataName:  name,
		ValueExpr: w.Value,
	}, diags
}

// resolveWriteTarget extracts and validates the data kind/name from a write
// target traversal. Returns empty strings on error.
func resolveWriteTarget(stepName, outcomeName, loc string, target hcl.Expression, g *FSMGraph) (kind, name string, diags hcl.Diagnostics) {
	kind, name, d := parseWriteTargetTraversal(target)
	diags = append(diags, d...)
	if kind == "" {
		return
	}
	if g.Data[kind] == nil || g.Data[kind][name] == nil {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("step %q outcome %q %s: target data %q %q is not declared", stepName, outcomeName, loc, kind, name),
			Subject:  target.StartRange().Ptr(),
		})
		return "", "", diags
	}
	return kind, name, diags
}

// parseWriteTargetTraversal validates that target is data.<kind>.<name>.value and
// returns the kind/name. Empty kind means validation failed.
func parseWriteTargetTraversal(target hcl.Expression) (kind, name string, diags hcl.Diagnostics) {
	vars := target.Variables()
	if len(vars) != 1 || len(vars[0]) != 4 {
		return "", "", hcl.Diagnostics{&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "target must be a traversal of the form data.<kind>.<name>.value",
			Subject:  target.StartRange().Ptr(),
		}}
	}
	root, ok1 := vars[0][0].(hcl.TraverseRoot)
	kindSeg, ok2 := vars[0][1].(hcl.TraverseAttr)
	nameSeg, ok3 := vars[0][2].(hcl.TraverseAttr)
	valueSeg, ok4 := vars[0][3].(hcl.TraverseAttr)
	if !ok1 || !ok2 || !ok3 || !ok4 || root.Name != "data" || valueSeg.Name != "value" {
		return "", "", hcl.Diagnostics{&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "target must be a traversal of the form data.<kind>.<name>.value",
			Subject:  target.StartRange().Ptr(),
		}}
	}
	return kindSeg.Name, nameSeg.Name, nil
}

// validateWriteOutputRefs checks that every output.* traversal in the value
// expression references a key present in knownOutputKeys.
func validateWriteOutputRefs(stepName, outcomeName, loc string, value hcl.Expression, knownOutputKeys map[string]bool) hcl.Diagnostics {
	var diags hcl.Diagnostics
	for _, traversal := range value.Variables() {
		if len(traversal) < 2 {
			continue
		}
		rootVar, ok1 := traversal[0].(hcl.TraverseRoot)
		field, ok2 := traversal[1].(hcl.TraverseAttr)
		if !ok1 || !ok2 || rootVar.Name != "output" {
			continue
		}
		if !knownOutputKeys[field.Name] {
			r := field.SrcRange
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("step %q outcome %q %s: output key %q is not declared in the output projection", stepName, outcomeName, loc, field.Name),
				Subject:  &r,
			})
		}
	}
	return diags
}
