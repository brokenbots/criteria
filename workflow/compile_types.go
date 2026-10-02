package workflow

// compile_types.go — compile path for workflow-level type "name" blocks (KB-45).
//
// A type block declares a named payload type in its own workflow-scoped
// namespace, referenced as `type.<name>` from outcome "schema" attributes.
// The block's required "schema" attribute carries any WS01 type constraint
// (object({...}), list(T), optional(T, default), ...) and is parsed exactly
// once with typeexpr.TypeConstraintWithDefaults via resolveTypeConstraint,
// so optional() defaults participate in payload defaulting identically for
// named and inline forms.
//
// Slice-1: type blocks cannot reference other type blocks — the namespace is
// leaf-level. A traversal rooted at "type" inside a schema constraint is a
// compile error. (References from outcomes are the KB-45 wired consumer;
// KB-48 widens the same resolution to data blocks, variables, and outputs.)

import (
	"fmt"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/ext/typeexpr"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
)

// compileTypes compiles all type blocks from spec into g.Types in declaration
// order. Runs before every consumer pass (variables, locals, data, outputs):
// type resolution never reads variables/locals/data (slice-1), so the type
// namespace must only precede its consumers; ordering only affects diagnostic
// layout.
func compileTypes(g *FSMGraph, spec *Spec) hcl.Diagnostics {
	if len(spec.Types) == 0 {
		return nil
	}
	if g.Types == nil {
		g.Types = make(map[string]*TypeDecl, len(spec.Types))
	}
	var diags hcl.Diagnostics
	for i := range spec.Types {
		diags = append(diags, compileTypeBlock(g, &spec.Types[i])...)
	}
	return diags
}

// compileTypeBlock compiles a single type "name" block and registers it on g.
func compileTypeBlock(g *FSMGraph, ts *TypeSpec) hcl.Diagnostics {
	name := ts.Name

	if _, ok := g.Types[name]; ok {
		return hcl.Diagnostics{&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("type %q: duplicate type declaration", name),
			Detail:   "type names must be unique within a workflow",
		}}
	}
	if d := checkTypeName(name); d != nil {
		return hcl.Diagnostics{d}
	}
	if d := checkNoTypeRefsInConstraint(name, ts.Schema); d != nil {
		return hcl.Diagnostics{d}
	}

	typ, defs, typDiags := resolveTypeConstraint(ts.Schema)
	if typDiags.HasErrors() {
		for _, dg := range typDiags {
			dg.Summary = fmt.Sprintf("type %q: %s", name, dg.Summary)
		}
		return typDiags
	}
	if typ == cty.NilType || isAbsentExpr(ts.Schema) {
		return hcl.Diagnostics{&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("type %q: schema attribute is required", name),
			Subject:  ts.Schema.Range().Ptr(),
		}}
	}

	// Strict extra-attribute rejection: TypeSpec's Remain body only exists so
	// unknown attributes surface with a clear message instead of a generic
	// unsupported-attribute parse error. Type blocks declare schema only.
	if d := rejectExtraTypeBlockAttrs(name, ts); d != nil {
		return hcl.Diagnostics{d}
	}

	g.Types[name] = &TypeDecl{Name: name, Type: typ, Defaults: defs}
	g.TypeOrder = append(g.TypeOrder, name)
	return nil
}

// checkTypeName rejects names that cannot appear as the attribute of a
// `type.<name>` traversal (the only consumer syntax).
func checkTypeName(name string) *hcl.Diagnostic {
	if name == "" {
		return &hcl.Diagnostic{Severity: hcl.DiagError, Summary: "type name must not be empty"}
	}
	if !hclsyntax.ValidIdentifier(name) {
		return &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("invalid type name %q", name),
			Detail:   "type names must be valid HCL identifiers: they are referenced as type.<name> attribute traversals.",
		}
	}
	return nil
}

// checkNoTypeRefsInConstraint is the slice-1 rule: a type block's schema
// constraint must not reference other type blocks. Type blocks are a
// leaf-level namespace; nesting would create unresolvable ordering (and
// recursive/ambiguous expansion), so it is rejected at parse time.
func checkNoTypeRefsInConstraint(typeName string, expr hcl.Expression) *hcl.Diagnostic {
	if expr == nil {
		return nil
	}
	for _, tr := range expr.Variables() {
		if len(tr) == 0 {
			continue
		}
		root, ok := tr[0].(hcl.TraverseRoot)
		if ok && root.Name == "type" {
			return &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("type %q: type blocks cannot reference other type blocks", typeName),
				Detail:   "type constraints may only compose built-in types (string, number, bool, object({...}), list(T), optional(T, default), ...). Write the constraint out at each use site instead of referencing type.<name>.",
				Subject:  tr.SourceRange().Ptr(),
			}
		}
	}
	return nil
}

// rejectExtraTypeBlockAttrs surfaces unknown attributes inside a type block
// as a clear compile error.
func rejectExtraTypeBlockAttrs(typeName string, ts *TypeSpec) *hcl.Diagnostic {
	if ts.Remain == nil {
		return nil
	}
	attrs, diags := ts.Remain.JustAttributes()
	if diags.HasErrors() {
		// Block content is not permitted inside a type block; the generic HCL
		// unsupported-block diagnostic is already precise here.
		_ = attrs
		return nil
	}
	for name := range attrs {
		return &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("type %q: unsupported attribute %q", typeName, name),
			Detail:   "type blocks accept a single required schema attribute.",
			Subject:  attrs[name].Expr.Range().Ptr(),
		}
	}
	return nil
}

// namedTypeRef reports whether expr IS exactly the two-segment traversal
// `type.<name>` and returns the name. The type assertion (not a Variables()
// scan) matters: a compound constraint whose only variable is a type ref —
// list(type.x), map(type.x), object({ a = type.x }), optional(type.x) — must
// NOT resolve to the bare named type. Such expressions fall through to
// resolveTypeConstraint and fail as type constraints, matching the documented
// resolution rule (type refs are leaf positions only; slice-1).
func namedTypeRef(expr hcl.Expression) (string, bool) {
	tr, ok := expr.(*hclsyntax.ScopeTraversalExpr)
	if !ok || len(tr.Traversal) != 2 {
		return "", false
	}
	root, ok := tr.Traversal[0].(hcl.TraverseRoot)
	if !ok || root.Name != "type" {
		return "", false
	}
	second, ok := tr.Traversal[1].(hcl.TraverseAttr)
	if !ok {
		return "", false
	}
	return second.Name, true
}

// resolveNamedTypeConstraint resolves a type-constraint expression authored
// at a consumer site (KB-48). A `type.<name>` traversal resolves against the
// workflow's type namespace (g.Types); the declaration-order namespace carries
// the exact cty.Type and optional() defaults the type block compiled to, so a
// consumer authored inline and its type-block twin produce identical graphs.
// Any other expression is validated against the leaf-only composition rule
// (checkTypeRefNotComposable) and then parsed as an inline typeexpr
// constraint via resolveTypeConstraint.
//
// loc identifies the consumer in diagnostics (e.g. `data "http" "msg"`),
// noun names the constraint position (e.g. "Data type constraints") for the
// unknown-reference hint. Unlike resolveTypeConstraint, an absent expression
// returns (cty.NilType, nil, nil): consumers keep their own absent semantics
// (variables default to string, data requires the attribute, outputs and
// outcome schemas leave the type unconstrained).
func resolveNamedTypeConstraint(loc, noun string, expr hcl.Expression, g *FSMGraph) (cty.Type, *typeexpr.Defaults, hcl.Diagnostics) {
	if expr == nil || isAbsentExpr(expr) {
		return cty.NilType, nil, nil
	}

	if decl, ok := namedTypeRef(expr); ok {
		td := g.Types[decl]
		if td == nil {
			r := expr.StartRange()
			return cty.NilType, nil, hcl.Diagnostics{&hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("%s: unknown workflow type %q", loc, decl),
				Detail:   fmt.Sprintf("%s may reference a top-level type block declared in this workflow, or carry an inline type constraint.", noun),
				Subject:  &r,
			}}
		}
		return td.Type, td.Defaults, nil
	}

	if d := checkTypeRefNotComposable(loc, expr); d != nil {
		return cty.NilType, nil, hcl.Diagnostics{d}
	}

	typ, defs, diags := resolveTypeConstraint(expr)
	if diags.HasErrors() {
		return cty.NilType, nil, diags
	}
	return typ, defs, nil
}

// checkTypeRefNotComposable rejects a constraint that embeds a type.<name>
// reference inside a wider inline constraint (list(type.x), map(type.x),
// object({ a = type.x }), optional(type.x), ...). Type refs are leaf
// positions only: resolving the composite would silently drop the wrapper
// (or mis-resolve the whole constraint), so the composition is a compile
// error about the constraint itself — the same boundary the docs state for
// the slice-1 type namespace.
func checkTypeRefNotComposable(loc string, expr hcl.Expression) *hcl.Diagnostic {
	for _, tr := range expr.Variables() {
		if len(tr) == 0 {
			continue
		}
		root, ok := tr[0].(hcl.TraverseRoot)
		if !ok || root.Name != "type" {
			continue
		}
		r := tr.SourceRange()
		return &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("%s: type references cannot be composed into other type constraints", loc),
			Detail:   "A type.<name> reference is valid only as the whole type constraint. Compose built-in types inline instead (list(...), map(...), object({...}), optional(...)), or write this constraint as the bare traversal type.<name>; a type block may not appear nested inside another constraint.",
			Subject:  &r,
		}
	}
	return nil
}
