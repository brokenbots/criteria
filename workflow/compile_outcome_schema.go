package workflow

// compile_outcome_schema.go — outcome-level payload contracts (KB-45). An
// outcome may declare `schema` (a `type.<name>` traversal or an inline typeexpr
// constraint), `require_comment`, and `fallback = true`. The schema resolves to
// a cty.Type plus its optional() defaults; the same cty.Type is then projected
// to deterministic JSON Schema bytes (CTypeToJSONSchema) so the named and
// inline authoring forms are indistinguishable on the wire.
//
// The schema must be a subset of what the outcome can actually produce:
//   - when the outcome declares an `output = { ... }` projection, that
//     projection IS the finalized payload (engine: applyOutcome), so schema
//     fields are validated against the projected object (folded when possible,
//     static keys otherwise);
//   - otherwise the payload is the adapter's raw finalized output, so schema
//     fields are validated against the adapter handshake output_schema
//     (same pattern as validateOutputExprStepOutputRefs). A schema the adapter
//     can never satisfy is a compile error.
//
// Aggregate iteration outcomes (all_succeeded / any_failed on iterating steps)
// never see raw adapter outputs, so their schema additionally requires an
// explicit output project.

import (
	"fmt"
	"sort"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/ext/typeexpr"
	"github.com/zclconf/go-cty/cty"
)

// compileOutcomeSchemaAttr resolves the outcome's `schema` attribute. The
// attribute accepts either a `type.<name>` traversal into the workflow's type
// namespace or an inline typeexpr constraint (object({...}), list(T),
// optional(T, default)) — both forms resolve to the same cty.Type and
// typeexpr.Defaults pair (resolveNamedTypeConstraint). Nothing else is
// accepted. Returns cty.NilType (with nil defaults) when the outcome declares
// no schema.
func compileOutcomeSchemaAttr(stepName, outcomeName string, expr hcl.Expression, g *FSMGraph) (cty.Type, *typeexpr.Defaults, hcl.Diagnostics) {
	if expr == nil || isAbsentExpr(expr) {
		return cty.NilType, nil, nil
	}

	loc := fmt.Sprintf("step %q outcome %q", stepName, outcomeName)
	typ, defs, diags := resolveNamedTypeConstraint(loc, "Outcome schemas", expr, g)
	if diags.HasErrors() {
		return cty.NilType, nil, diags
	}
	// The host-side payload validator only accepts a root object (or an
	// unconstrained root); anything else (list, map, number, ...) can never be
	// satisfied as a finalized payload. The gate applies to named refs and
	// inline constraints alike: refactoring a schema from inline to a named
	// type block must not change compile behavior.
	if typ != cty.DynamicPseudoType && !typ.IsObjectType() {
		r := expr.StartRange()
		return cty.NilType, nil, hcl.Diagnostics{&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("%s: schema must be an object(...) type constraint; got %s", loc, typ.FriendlyName()),
			Subject:  &r,
		}}
	}
	return typ, defs, nil
}

// validateOutcomeSchemaPayloadContract checks an outcome's compiled schema
// against the payload the outcome can actually produce. See the file comment
// for the lane logic. schemaExpr supplies the diagnostic range; it may be nil
// when no range is available.
func validateOutcomeSchemaPayloadContract(stepName, outcomeName string, schemaExpr hcl.Expression, schemaT cty.Type, compiled *CompiledOutcome, isAggregateIter bool, g *FSMGraph, opts CompileOpts, adapterOutputSchema map[string]ConfigField) hcl.Diagnostics {
	if isAggregateIter && compiled.OutputExpr == nil {
		return hcl.Diagnostics{&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("step %q outcome %q: schema on an aggregate outcome requires an output = { ... } projection block", stepName, outcomeName),
			Detail:   `Aggregate outcomes (e.g. "all_succeeded", "any_failed") fire after all iterations complete; there are no raw adapter outputs to validate against. Add an output = { ... } projection block so the payload the schema describes is well-defined.`,
			Subject:  rangeOrNil(schemaExpr),
		}}
	}

	// The projection, when declared, is what the engine stores as the step
	// output — the payload the contract describes — regardless of whether the
	// outcome is per-iteration or aggregate.
	if compiled.OutputExpr != nil {
		return validateSchemaAgainstProjection(stepName, outcomeName, schemaExpr, schemaT, compiled.OutputExpr, g, opts)
	}

	if len(adapterOutputSchema) == 0 {
		// No declared adapter output contract (or a step kind without one, such
		// as sub-workflow steps): nothing to check the schema against at compile
		// time. Host-side validation still enforces the contract at runtime.
		return nil
	}

	var diags hcl.Diagnostics
	attrTypes := schemaT.AttributeTypes()
	for _, fieldName := range sortedObjectAttrs(schemaT) {
		declared, known := adapterOutputSchema[fieldName]
		if !known {
			diags = append(diags, schemaFieldError(stepName, outcomeName, fieldName,
				"is not declared in the adapter's output schema (schema fields must be a subset of the adapter handshake)",
				rangeOrNil(schemaExpr)))
			continue
		}
		if declared.CtyType == cty.NilType || declared.CtyType == cty.DynamicPseudoType {
			// Adapter declares the field permissively; any payload shape it may
			// produce can satisfy the field, so no type check applies.
			continue
		}
		if !typeCompatSubset(attrTypes[fieldName], declared.CtyType) {
			diags = append(diags, schemaFieldError(stepName, outcomeName, fieldName,
				fmt.Sprintf("is not compatible with the adapter's declared output type %s", declared.CtyType.FriendlyName()),
				rangeOrNil(schemaExpr)))
		}
	}
	return diags
}

// validateSchemaAgainstProjection checks schema fields against an outcome's
// output projection. When the projection folds at compile time (no runtime
// references), both field presence and types are checked against the folded
// object type. Otherwise only statically-known literal keys can be checked.
func validateSchemaAgainstProjection(stepName, outcomeName string, schemaExpr hcl.Expression, schemaT cty.Type, projExpr hcl.Expression, g *FSMGraph, opts CompileOpts) hcl.Diagnostics {
	var diags hcl.Diagnostics

	val, foldable, foldDiags := FoldExpr(projExpr, graphVars(g), graphLocals(g), opts.WorkflowDir)
	if foldDiags.HasErrors() {
		return foldDiags
	}
	if !foldable || val == cty.NilVal || !val.IsKnown() || !val.Type().IsObjectType() {
		// Computed projections cannot be typed at compile time. Literal keys,
		// when present, still give a presence check.
		keys := staticObjectExprKeys(projExpr)
		if keys == nil {
			return nil
		}
		for _, fieldName := range sortedObjectAttrs(schemaT) {
			if !keys[fieldName] {
				diags = append(diags, schemaFieldError(stepName, outcomeName, fieldName,
					"is not produced by the outcome's output projection", rangeOrNil(schemaExpr)))
			}
		}
		return diags
	}

	projT := val.Type()
	projAttrs := projT.AttributeTypes()
	for _, fieldName := range sortedObjectAttrs(schemaT) {
		projFieldT, known := projAttrs[fieldName]
		if !known {
			diags = append(diags, schemaFieldError(stepName, outcomeName, fieldName,
				"is not produced by the outcome's output projection", rangeOrNil(schemaExpr)))
			continue
		}
		if !typeCompatSubset(schemaT.AttributeTypes()[fieldName], projFieldT) {
			diags = append(diags, schemaFieldError(stepName, outcomeName, fieldName,
				fmt.Sprintf("is not compatible with the projected type %s", projFieldT.FriendlyName()),
				rangeOrNil(schemaExpr)))
		}
	}
	return diags
}

// schemaFieldError builds the standard "schema field cannot be satisfied"
// diagnostic for one outcome schema field.
func schemaFieldError(stepName, outcomeName, fieldName, problem string, r *hcl.Range) *hcl.Diagnostic {
	return &hcl.Diagnostic{
		Severity: hcl.DiagError,
		Summary:  fmt.Sprintf("step %q outcome %q: schema field %q %s", stepName, outcomeName, fieldName, problem),
		Subject:  r,
	}
}

// typeCompatSubset reports whether a payload drawn from sup (what the producer
// can emit) can always satisfy sub (the outcome's schema): sub must be a
// structural subset of sup. Untyped (cty.NilType / dynamic) producers accept
// anything. Object compatibility is attribute-wise so optional()/required
// differences and nested shapes compare correctly; primitive leaves must match
// exactly.
func typeCompatSubset(sub, sup cty.Type) bool {
	if sub == cty.NilType || sup == cty.NilType || sup == cty.DynamicPseudoType {
		return true
	}
	if sub.IsObjectType() && sup.IsObjectType() {
		return objectAttrsSubset(sub, sup)
	}
	if sub.IsListType() && sup.IsListType() || sub.IsSetType() && sup.IsSetType() || sub.IsMapType() && sup.IsMapType() {
		return typeCompatSubset(sub.ElementType(), sup.ElementType())
	}
	return sub.Equals(sup)
}

// objectAttrsSubset compares two object types attribute-wise: every schema
// attribute must exist on the producer with a compatible type, and the
// producer must not be allowed to omit a required field.
func objectAttrsSubset(sub, sup cty.Type) bool {
	supAttrs := sup.AttributeTypes()
	for name, subFieldT := range sub.AttributeTypes() {
		supFieldT, ok := supAttrs[name]
		if !ok {
			return false
		}
		// The producer may omit an optional attribute; a schema that
		// requires it could then never be satisfied.
		if sup.AttributeOptional(name) && !sub.AttributeOptional(name) {
			return false
		}
		if !typeCompatSubset(subFieldT, supFieldT) {
			return false
		}
	}
	return true
}

// sortedObjectAttrs returns the attribute names of an object type in
// deterministic order.
func sortedObjectAttrs(typ cty.Type) []string {
	attrs := typ.AttributeTypes()
	names := make([]string, 0, len(attrs))
	for name := range attrs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// rangeOrNil returns a subject range for a diagnostic, tolerating nil
// expressions.
func rangeOrNil(expr hcl.Expression) *hcl.Range {
	if expr == nil {
		return nil
	}
	r := expr.StartRange()
	return &r
}

// validateOutcomeContractAttrs rejects schema/require_comment/fallback outcome
// attributes on node kinds that have no per-outcome payload contract (KB-45
// scope: wait and approval outcomes are engine-mandated with no payload).
// Shared by the simple-outcome compile paths so unsupported attributes fail
// loudly at compile time instead of being silently ignored.
func validateOutcomeContractAttrs(kind, nodeName string, outcomes []OutcomeSpec) hcl.Diagnostics {
	var diags hcl.Diagnostics
	for _, o := range outcomes {
		if o.Schema != nil && !isAbsentExpr(o.Schema) {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("%s %q outcome %q: schema is not supported on %s outcomes", kind, nodeName, o.Name, kind),
				Detail:   "Per-outcome payload schemas are only available on adapter and sub-workflow step outcomes (KB-45).",
			})
		}
		if o.RequireComment {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("%s %q outcome %q: require_comment is not supported on %s outcomes", kind, nodeName, o.Name, kind),
			})
		}
		if o.Fallback {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("%s %q outcome %q: fallback is not supported on %s outcomes", kind, nodeName, o.Name, kind),
			})
		}
	}
	return diags
}
