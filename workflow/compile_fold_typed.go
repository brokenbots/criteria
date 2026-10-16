package workflow

// compile_fold_typed.go — KB-266: compile-time type checking under value
// deferral. When FoldExpr defers an expression because it references a
// runtime-only namespace (data.*, steps.*, step.*, each.*, while.*, path.*,
// subworkflow.*), the deferral must not waive TYPE checking. The helpers
// here evaluate the deferred expression a second time with every runtime
// root bound to cty.UnknownVal(declared type) — variable/data types from the
// graph, step output field types from the adapter schema (KB-264 default
// output type), and iteration binding types from the step's driving
// for_each/count/parallel expression. HCL and cty semantics then type-check
// every operation whose operand types are available; operations on truly
// dynamic operands stay runtime-deferred.
//
// No value evaluation happens here: the fold result at these sites is
// discarded and the expression remains deferred to the engine for rendering.

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
)

// validateFoldableAttrsTyped validates all attributes in attrs like
// validateFoldableAttrs and additionally type-checks deferred expressions
// (KB-266): when FoldExpr defers on a runtime-only namespace reference, the
// expression is re-evaluated with runtime roots bound to
// cty.UnknownVal(declared type). Type errors surface as compile diagnostics
// at the binding site; type-correct expressions still defer unchanged.
//
// g carries the declared types; sp is the step whose input /
// secret_input block is being validated (used to resolve iteration binding
// types and self refs).
func validateFoldableAttrsTyped(
	g *FSMGraph,
	sp *StepSpec,
	attrs hcl.Attributes,
	opts CompileOpts,
) hcl.Diagnostics {
	var diags hcl.Diagnostics
	if len(attrs) == 0 {
		return diags
	}
	vars, locals := graphVars(g), graphLocals(g)
	bindings := stepInputRuntimeBindings(g, sp, opts)
	for _, attr := range attrs {
		_, foldable, d := FoldExpr(attr.Expr, vars, locals, opts.WorkflowDir)
		if !foldable {
			// Runtime-deferred — evaluate under static type bindings for
			// compile-time type checking. The fold result is discarded:
			// unknowns never materialize into renders.
			_, _, td := FoldExprWithRuntimeBindings(attr.Expr, vars, locals, opts.WorkflowDir, bindings)
			diags = append(diags, errorDiagsWithFallbackSubject(td, attr.Expr)...)
			continue
		}
		diags = append(diags, errorDiagsWithFallbackSubject(d, attr.Expr)...)
	}
	return diags
}

// stepInputRuntimeBindings builds static unknown-of-declared-type bindings
// for the runtime-only namespaces a step's input expressions may reference,
// mirroring the runtime eval context shapes (BuildEvalContextWithOpts,
// WithStepOutputs, WithEachBinding, WithWhileBinding, SeedDataSnapshot).
//
// Members with no declared type bind cty.UnknownVal(cty.DynamicPseudoType);
// operations on them stay runtime-deferred rather than erroring.
func stepInputRuntimeBindings(g *FSMGraph, sp *StepSpec, opts CompileOpts) map[string]cty.Value {
	bindings := map[string]cty.Value{
		// Not exposed to step input evaluation at runtime; opaque bindings
		// keep refs to them deferred without introducing "undefined
		// variable" errors.
		"subworkflow": cty.DynamicVal,
	}
	bindings["path"] = cty.ObjectVal(map[string]cty.Value{
		// Unknown-of-string keeps path composition type-checked (string ops stay
		// valid, arity/typing enforced) without evaluating filesystem functions
		// at compile time: file() and templatefile() short-circuit on unknown
		// args and keep deferring to runtime. Runtime binds the real dirs
		// (eval.go), and file()/templatefile() reject absolute paths there, so
		// no currently-valid workflow loses its runtime render.
		"workflow": cty.UnknownVal(cty.String),
		"root":     cty.UnknownVal(cty.String),
		"cwd":      cty.UnknownVal(cty.String),
	})
	bindings["data"] = dataNamespaceBinding(g)
	bindings["steps"] = stepsNamespaceBinding(g, stepNameOf(sp))

	driver, driverExpr := iterationDriverOf(sp)
	bindings["each"] = eachNamespaceBinding(g, driver, driverExpr, opts)
	bindings["while"] = whileNamespaceBinding(driver)
	return bindings
}

func stepNameOf(sp *StepSpec) string {
	if sp == nil {
		return ""
	}
	return sp.Name
}

// iterationDriverOf reports which iteration mechanism drives sp and returns
// its driving expression. The controls live in sp.Remain and are already
// consumed by decodeRemainIter at this point, so they are read
// non-destructively via JustAttributes.
func iterationDriverOf(sp *StepSpec) (string, hcl.Expression) {
	if sp == nil || sp.Remain == nil {
		return "", nil
	}
	attrs, _ := sp.Remain.JustAttributes()
	if _, ok := attrs["for_each"]; ok {
		return "for_each", attrs["for_each"].Expr
	}
	if _, ok := attrs["count"]; ok {
		return "count", attrs["count"].Expr
	}
	if _, ok := attrs["parallel"]; ok {
		return "parallel", attrs["parallel"].Expr
	}
	if _, ok := attrs["while"]; ok {
		return "while", attrs["while"].Expr
	}
	return "", nil
}

// dataNamespaceBinding mirrors DataStore.Snapshot: data.<kind>.<name>.value
// is a value of the declared type and data.<kind>.<name>.type is the type
// name string. Data blocks always declare a type (enforced at parse time).
func dataNamespaceBinding(g *FSMGraph) cty.Value {
	if len(g.Data) == 0 {
		return cty.EmptyObjectVal
	}
	kinds := make(map[string]cty.Type, len(g.Data))
	for kind, byName := range g.Data {
		names := make(map[string]cty.Type, len(byName))
		for name, node := range byName {
			valTy := cty.DynamicPseudoType
			if node != nil && node.Type != cty.NilType {
				valTy = node.Type
			}
			names[name] = cty.Object(map[string]cty.Type{
				"value": valTy,
				"type":  cty.String,
			})
		}
		kinds[kind] = cty.Object(names)
	}
	return cty.UnknownVal(cty.Object(kinds))
}

// stepsNamespaceBinding mirrors the runtime steps object: one entry per step
// name. Non-iterating adapter steps with a declared output schema (KB-264
// default output type) bind an object of unknown-of-declared-type fields;
// iterating steps (whose outputs expose an indexable accumulator), subworkflow
// targets, forward references, and steps with no declared contract bind
// cty.DynamicVal so their surfaces stay runtime-deferred.
func stepsNamespaceBinding(g *FSMGraph, selfName string) cty.Value {
	entries := map[string]cty.Value{}

	// Previously compiled steps carry their resolved shapes.
	for _, name := range g.stepOrder {
		node, ok := g.Steps[name]
		if !ok || node == nil {
			entries[name] = cty.DynamicVal
			continue
		}
		entries[name] = stepOutputBinding(node)
	}

	// Forward references: every declared step label not yet compiled binds
	// opaque so forward steps.<name> refs keep deferring (field-name validity
	// is enforced by checkCrossStepFieldRefs after the graph is complete).
	if g.spec != nil {
		for i := range g.spec.Steps {
			ss := g.spec.Steps[i]
			if _, exists := entries[ss.Name]; !exists {
				entries[ss.Name] = cty.DynamicVal
			}
		}
	}
	if selfName != "" {
		if _, exists := entries[selfName]; !exists {
			entries[selfName] = cty.DynamicVal
		}
	}
	if len(entries) == 0 {
		return cty.EmptyObjectVal
	}
	entryTypes := make(map[string]cty.Type, len(entries))
	for name, val := range entries {
		entryTypes[name] = val.Type()
	}
	return cty.UnknownVal(cty.Object(entryTypes))
}

// stepOutputBinding derives the static shape of one step's runtime outputs
// entry from its declared output schema.
func stepOutputBinding(node *StepNode) cty.Value {
	if node.ForEach != nil || node.Count != nil || node.Parallel != nil || node.While != nil {
		return cty.DynamicVal
	}
	if node.TargetKind != StepTargetAdapter {
		return cty.DynamicVal
	}
	return outputSchemaBinding(node.OutputSchema)
}

// outputSchemaBinding builds the typed object for a set of declared output
// fields. Fields with cty.NilType (KB-264 declared-permissive) stay dynamic.
// The legacy 4-segment steps.<name>.outputs.<field> form binds a dynamic
// "outputs" attribute: this keeps those traversals compile-clean here so the
// dedicated field-name validators (checkStepsOutputRefs, with
// Levenshtein-sorted suggestions) remain the authority without competing
// duplicate diagnostics; the canonical flat steps.<name>.<field> form carries
// the declared types.
func outputSchemaBinding(schema map[string]ConfigField) cty.Value {
	if len(schema) == 0 {
		return cty.DynamicVal
	}
	fields := make(map[string]cty.Type, len(schema))
	for name, field := range schema {
		if field.CtyType == cty.NilType {
			fields[name] = cty.DynamicPseudoType
		} else {
			fields[name] = field.CtyType
		}
	}
	fields["outputs"] = cty.DynamicPseudoType
	return cty.UnknownVal(cty.Object(fields))
}

// eachNamespaceBinding mirrors WithEachBinding: value comes from the driving
// collection when its element type is statically known (keys are strings,
// counters are numbers, _prev is only known dynamically). Non-iterating
// steps and while-driven steps do not bind "each" at runtime; bind opaque so
// refs keep deferring (each.* validity is enforced by validateEachRefs).
func eachNamespaceBinding(g *FSMGraph, driver string, driverExpr hcl.Expression, opts CompileOpts) cty.Value {
	if driver == "" || driver == "while" {
		return cty.DynamicVal
	}

	elemTy := cty.DynamicPseudoType
	switch driver {
	case "count":
		elemTy = cty.Number
	case "for_each", "parallel":
		if driverExpr != nil {
			if val, folded, d := FoldExpr(driverExpr, graphVars(g), graphLocals(g), opts.WorkflowDir); folded && !d.HasErrors() && val.IsKnown() && !val.IsNull() {
				elemTy = collectionElementType(val.Type())
			}
		}
	}

	return cty.UnknownVal(cty.Object(map[string]cty.Type{
		"value":  elemTy,
		"key":    cty.String,
		"_idx":   cty.Number,
		"_total": cty.Number,
		"_first": cty.Bool,
		"_last":  cty.Bool,
		"_prev":  cty.DynamicPseudoType,
	}))
}

// collectionElementType resolves the element type of a list/map/set value
// type. Homogeneous tuples resolve to their unified element type; anything
// else stays dynamic.
func collectionElementType(ty cty.Type) cty.Type {
	switch {
	case ty.IsListType() || ty.IsSetType() || ty.IsMapType():
		return ty.ElementType()
	case ty.IsTupleType():
		et := ty.TupleElementTypes()
		if len(et) > 0 {
			uniform := true
			for _, t := range et[1:] {
				if !t.Equals(et[0]) {
					uniform = false
					break
				}
			}
			if uniform {
				return et[0]
			}
		}
	}
	return cty.DynamicPseudoType
}

// whileNamespaceBinding mirrors WithWhileBinding: index is a number, first is
// a bool, _prev is only known dynamically. Non-while steps do not bind
// "while" at runtime; bind opaque so refs keep deferring (while.* validity is
// enforced by validateWhileRefs).
func whileNamespaceBinding(driver string) cty.Value {
	if driver != "while" {
		return cty.DynamicVal
	}
	return cty.UnknownVal(cty.Object(map[string]cty.Type{
		"index": cty.Number,
		"first": cty.Bool,
		"_prev": cty.DynamicPseudoType,
	}))
}

// unknownVarNames reports the dot-paths of unknown values nested in a value
// with their declared static types, for diagnostics naming the variable at
// the binding site ("port [number], snap.items [list of string]"). Paths are
// sorted for stable diagnostics and capped to keep messages bounded.
func unknownVarNames(v cty.Value) string {
	var paths []string
	var walk func(cur cty.Value, path string)
	walk = func(cur cty.Value, path string) {
		if len(paths) >= maxUnknownVarNames {
			return
		}
		if !cur.IsKnown() {
			if path != "" {
				paths = append(paths, fmt.Sprintf("%s [%s]", path, declaredCtyTypeName(cur.Type())))
			}
			return
		}
		if cur.IsNull() {
			return
		}
		ty := cur.Type()
		if !(ty.IsObjectType() || ty.IsMapType() || ty.IsListType() || ty.IsTupleType() || ty.IsSetType()) {
			return
		}
		if ty.IsListType() || ty.IsSetType() {
			// Homogeneous collections: the root already names the element
			// type; per-element paths add noise.
			return
		}
		if len(paths) >= maxUnknownVarNames-1 {
			paths = append(paths, "...")
			return
		}
		it := cur.ElementIterator()
		for it.Next() {
			k, elem := it.Element()
			// Object and map iterators yield String keys; tuple iterators
			// yield Number element indexes (cty guarantees this). AsString
			// panics on non-String keys, so each key is formatted by type.
			switch {
			case k.Type() == cty.String:
				walk(elem, joinPath(path, k.AsString()))
			case k.Type() == cty.Number:
				// Tuple keys are exact element indexes.
				i, _ := k.AsBigFloat().Int64()
				walk(elem, joinPath(path, strconv.FormatInt(i, 10)))
			}
		}
	}
	walk(v, "")
	sort.Strings(paths)
	return strings.Join(paths, ", ")
}

// maxUnknownVarNames caps the number of unknown-variable paths reported in a
// single type-validation diagnostic so messages stay readable.
const maxUnknownVarNames = 8

func joinPath(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "." + name
}

// declaredCtyTypeName names a declared static type for diagnostics.
func declaredCtyTypeName(ty cty.Type) string {
	if ty == cty.DynamicPseudoType {
		return "dynamic"
	}
	return ty.FriendlyName()
}
