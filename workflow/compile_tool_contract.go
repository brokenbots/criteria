package workflow

// compile_tool_contract.go — typed tool contracts on adapter tool blocks
// (KB-59, MCP-probe wave 2 Gap 2: typed I/O at the adapter-tools seam).
//
// A tool declaration may pin the callee's request shape (`in`) and the shape
// of the outputs surfaced to the caller (`out`) to a named type block
// (KB-45) referenced as `type.<name>`, or to an inline typeexpr constraint.
// The engine then validates nested tool-call arguments against `in`
// (internal/adapterhost/tool_call.go seam: callee_input) and successful
// callee results against `out`, and a direct step target's input{} block is
// validated against `in` at compile time (validateStepInputToolContract).
// Payload validation reuses the pinned criteria-adapter-proto evaluator
// vocabulary, so a seam rejection reads like any other payload_schema issue
// list. Adapters that declare no contracts — schema-less or schema-ful — are
// entirely unaffected.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/convert"
)

// compileToolContract decodes one tool declaration's typed contract
// attributes (`in`, `out`). Each resolves through the shared type-constraint
// path and is gated to the payload shape the seam validator accepts: an
// object(...) constraint or an unconstrained root (matching the outcome
// schema posture — the pinned evaluator validates object payloads only).
// Empty or absent attributes leave that side unconstrained; a contract with
// no declared side is not recorded.
func compileToolContract(adapterKey, toolName string, t *ToolDeclSpec, g *FSMGraph) (ToolContract, hcl.Diagnostics) {
	var diags hcl.Diagnostics
	var contract ToolContract

	loc := fmt.Sprintf("adapter %q tool %q", adapterKey, toolName)
	contract.Loc = loc

	schemaJSON, typ, d := compileToolContractSide(loc, "in", t.In, g)
	diags = append(diags, d...)

	outJSON, outTyp, d := compileToolContractSide(loc, "out", t.Out, g)
	diags = append(diags, d...)

	contract.InType = typ
	contract.InSchemaJSON = schemaJSON
	contract.OutType = outTyp
	contract.OutSchemaJSON = outJSON

	// Once a contract side is declared the body is closed vocabulary (the
	// type-block posture); without a contract, uninterpreted body content is
	// still decoded and ignored, byte-identical to CRI-155.
	if contract.HasAny() && t.Remain != nil {
		attrs, d := t.Remain.JustAttributes()
		diags = append(diags, d...)
		for _, k := range sortedAttrNames(attrs) {
			if k == "in" || k == "out" {
				continue
			}
			r := attrs[k].Expr.Range()
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("tool %q: unsupported attribute %q", toolName, k),
				Subject:  &r,
			})
		}
	}
	return contract, diags
}

// compileToolContractSide resolves one contract side (`in` or `out`) and
// returns its deterministic schema bytes plus the resolved object type.
func compileToolContractSide(loc, side string, expr hcl.Expression, g *FSMGraph) ([]byte, cty.Type, hcl.Diagnostics) {
	typ, defs, diags := resolveNamedTypeConstraint(fmt.Sprintf("%s: %s", loc, side), "Tool contracts", expr, g)
	if diags.HasErrors() {
		return nil, cty.NilType, diags
	}
	if typ == cty.NilType {
		return nil, cty.NilType, nil
	}
	// The seam validator accepts a root object (or an unconstrained root);
	// anything else (list, map, number, ...) can never be satisfied as a
	// tool-call payload. The gate applies to named refs and inline
	// constraints alike: refactoring a contract from inline to a named type
	// block must not change compile behavior.
	if typ != cty.DynamicPseudoType && !typ.IsObjectType() {
		r := expr.StartRange()
		return nil, cty.NilType, hcl.Diagnostics{&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("%s: %s must be an object(...) type constraint; got %s", loc, side, typ.FriendlyName()),
			Subject:  &r,
		}}
	}
	schemaJSON, err := CTypeToJSONSchema(typ, defs)
	if err != nil {
		r := expr.StartRange()
		return nil, cty.NilType, hcl.Diagnostics{&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("%s: %s: invalid type constraint: %v", loc, side, err),
			Subject:  &r,
		}}
	}
	return schemaJSON, typ, nil
}

// validateStepInputToolContract resolves the in-type contract that governs a
// step's input{} block (KB-59), if any. It applies only to dynamic
// (schema-less) adapters: a schema-ful adapter keeps the handshake-first
// static posture at compile time, and the seam's declared-type override is
// the seam's own precedence. Adapters without tool contracts stay
// byte-identical.
//
// The input must route to a declared tool as a compile-time literal
// (input { tool = "<name>" }); a dynamic tool reference stays unchecked at
// compile time and is validated by the callee adapter at runtime.
func validateStepInputToolContract(g *FSMGraph, sp *StepSpec, adapterRef string, schemas map[string]AdapterInfo) (ToolContract, bool, hcl.Diagnostics) {
	if sp.Input == nil || g == nil || adapterRef == "" {
		return ToolContract{}, false, nil
	}
	adNode, ok := g.Adapters[adapterRef]
	if !ok || !adNode.HasToolContracts() {
		return ToolContract{}, false, nil
	}
	// Schema-ful adapters keep the static handshake-first posture at compile
	// time.
	if info, adOK := adapterInfo(schemas, adapterTypeFromRef(adapterRef)); adOK && len(info.InputSchema) > 0 {
		return ToolContract{}, false, nil
	}
	attrs, d := sp.Input.Remain.JustAttributes()
	if d.HasErrors() {
		// Block-content errors are already reported by decodeStepInput.
		return ToolContract{}, false, nil
	}
	// The input must route to a contract-bearing tool. Two cases qualify:
	// a compile-time literal tool reference, and (single-tool adapters only)
	// no tool attribute at all — the lone contract is unambiguous then, so
	// missing-required diagnostics are still reportable. A tool attribute
	// that is not a known literal (dynamic route) stays unchecked at
	// compile time; the callee validates it at runtime.
	var contract ToolContract
	var routed bool
	if toolName := stepInputToolLiteral(attrs); toolName != "" {
		contract, routed = adNode.ToolContractFor(toolName)
	} else if _, hasToolAttr := attrs["tool"]; !hasToolAttr && len(adNode.ToolContractOrder) == 1 {
		contract, routed = adNode.ToolContractFor(adNode.ToolContractOrder[0])
	}
	if !routed || contract.InType == cty.NilType {
		return ToolContract{}, false, nil
	}
	return contract, true, nil
}

// stepInputToolLiteral returns the tool a step's input block routes to when
// it is a compile-time literal (input { tool = "..." }), or "" otherwise.
func stepInputToolLiteral(attrs map[string]*hcl.Attribute) string {
	attr, ok := attrs["tool"]
	if !ok {
		return ""
	}
	val, err := attr.Expr.Value(nil)
	if err != nil || val.IsNull() || !val.IsKnown() || val.Type() != cty.String {
		return ""
	}
	name := strings.TrimSpace(val.AsString())
	if name == "" {
		return ""
	}
	return name
}

// validateTypedInputAttrs validates a direct target's input attributes
// against the declared in-type (the declared type IS the schema for this
// path): unknown keys are rejected exactly as they are for a schema-ful
// adapter's input, values are checked against the type's attribute types
// when they are concrete at compile time (deferred otherwise, mirroring the
// schema path's placeholder handling), and required non-optional attributes
// must be present. Diagnostics mirror the schema path's vocabulary.
func validateTypedInputAttrs(context string, attrs map[string]*hcl.Attribute, inType cty.Type, adapterName string, missingRange hcl.Range) hcl.Diagnostics {
	var diags hcl.Diagnostics
	typAttrs := inType.AttributeTypes()

	for _, k := range sortedAttrNames(attrs) {
		if _, known := typAttrs[k]; !known {
			diags = append(diags, unknownFieldDiagnostic(context, k, adapterName, attrs[k].Expr.Range()))
		}
	}
	for _, k := range sortedAttrNames(attrs) {
		attrType, known := typAttrs[k]
		if !known {
			continue
		}
		val, err := attrs[k].Expr.Value(nil)
		if err != nil {
			// Eval errors are already reported by the input decode pass.
			continue
		}
		if !val.IsKnown() || val.IsNull() {
			// Deferred to runtime, like the schema path's placeholders.
			continue
		}
		cv, cerr := typedAttrCheck(val, attrType)
		if cerr != nil {
			r := attrs[k].Expr.Range()
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("%s: field %q: %v", context, k, cerr),
				Subject:  &r,
			})
			continue
		}
		_ = cv
	}
	// Required attributes (non-optional object attributes) must be present.
	for _, k := range sortedTypeAttrNames(typAttrs) {
		if inType.AttributeOptional(k) {
			continue
		}
		if _, present := attrs[k]; present {
			continue
		}
		diags = append(diags, requiredFieldDiagnostic(context, k, missingRange))
	}
	return diags
}

// typedAttrCheck validates one concrete value against a declared attribute
// type with JSON payload fidelity: a string-typed attribute accepts only
// string values (the wire payload is JSON, where 12345 is not a string), a
// number-typed one only numbers, and a bool only bools — while structured
// attributes (list/map/object/tuple/...) convert like the permissive decode
// does. Unconstrained (dynamic) attribute types accept anything.
func typedAttrCheck(val cty.Value, attrType cty.Type) (cty.Value, error) {
	if attrType == cty.DynamicPseudoType {
		return val, nil
	}
	if attrType == cty.String || attrType == cty.Number || attrType == cty.Bool {
		if val.Type() == attrType {
			return val, nil
		}
		return cty.NilVal, fmt.Errorf("%s required", attrType.FriendlyName())
	}
	if val.Type().Equals(attrType) {
		return val, nil
	}
	return convert.Convert(val, attrType)
}

// requiredFieldDiagnostic mirrors the schema path's required-field diagnostic.
func requiredFieldDiagnostic(context, field string, missingRange hcl.Range) *hcl.Diagnostic {
	var subject *hcl.Range
	if missingRange.Filename != "" {
		r := missingRange
		subject = &r
	}
	return &hcl.Diagnostic{
		Severity: hcl.DiagError,
		Summary:  fmt.Sprintf("%s: required field %q is missing", context, field),
		Subject:  subject,
	}
}

// sortedAttrNames returns the input attribute keys in deterministic order.
func sortedAttrNames(attrs map[string]*hcl.Attribute) []string {
	names := make([]string, 0, len(attrs))
	for k := range attrs {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// sortedTypeAttrNames returns an object type's attribute names in
// deterministic order.
func sortedTypeAttrNames(typAttrs map[string]cty.Type) []string {
	names := make([]string, 0, len(typAttrs))
	for name := range typAttrs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
