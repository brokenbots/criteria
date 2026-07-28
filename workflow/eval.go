// Package workflow — eval.go provides the HCL evaluation context builder and
// helpers for runtime expression evaluation introduced in W04.
package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/convert"
	ctyjson "github.com/zclconf/go-cty/cty/json"
)

// BuildEvalContext constructs an HCL evaluation context from the run-scoped
// vars map (typically RunState.Vars). The context exposes:
//
//   - var.<name>          from vars["var"] object
//   - steps.<step>.<out>  from vars["steps"] object
//   - each.value          from vars["each"] object, when inside a for_each iteration (W07)
//   - each.index          from vars["each"] object, when inside a for_each iteration (W07)
//
// When vars["each"] is absent, the "each" variable is not included in the
// context. ResolveInputExprs detects each.* references in that case and emits
// "each is only valid inside for_each".
//
// Expression functions (file, fileexists, trimfrontmatter) are registered with
// default options (env-var sourced, empty workflow dir). Callers that need a
// specific workflow directory should use BuildEvalContextWithOpts.
func BuildEvalContext(vars map[string]cty.Value) *hcl.EvalContext {
	return BuildEvalContextWithOpts(vars, DefaultFunctionOptions(""))
}

// BuildEvalContextWithOpts is like BuildEvalContext but accepts explicit
// FunctionOptions so callers can provide a WorkflowDir for file() resolution.
// Use DefaultFunctionOptions(dir) to source MaxBytes and AllowedPaths from
// environment variables alongside the workflow directory.
//
// If vars["local"] is present and is a non-nil object, it is exposed as the
// "local" namespace in the context so runtime expressions can read compiled
// locals.
func BuildEvalContextWithOpts(vars map[string]cty.Value, opts *FunctionOptions) *hcl.EvalContext {
	varObj := cty.EmptyObjectVal
	stepsObj := cty.EmptyObjectVal

	if v, ok := objectFromVars(vars, "var"); ok {
		varObj = v
	}
	if s, ok := objectFromVars(vars, "steps"); ok {
		stepsObj = s
	}

	ctxVars := map[string]cty.Value{
		"var":   varObj,
		"steps": stepsObj,
	}

	// Include "each" bindings when inside a for_each iteration (W07).
	if each, ok := objectFromVars(vars, "each"); ok {
		ctxVars["each"] = each
	}

	// Include "while" bindings when inside a while iteration.
	if w, ok := objectFromVars(vars, "while"); ok {
		ctxVars["while"] = w
	}

	// Expose compiled locals as "local.*" when they have been seeded (W07).
	if local, ok := objectFromVars(vars, "local"); ok {
		ctxVars["local"] = local
	}

	// Expose data block values as "data.*" when a snapshot is present.
	if data, ok := objectFromVars(vars, "data"); ok {
		ctxVars["data"] = data
	}

	// Expose path variables for workflow-relative path construction (WS05).
	ctxVars["path"] = cty.ObjectVal(map[string]cty.Value{
		"workflow": cty.StringVal(opts.WorkflowDir),
		"root":     cty.StringVal(opts.RootDir),
		"cwd":      cty.StringVal(opts.Cwd),
	})

	return &hcl.EvalContext{
		Variables: ctxVars,
		Functions: workflowFunctions(opts),
	}
}

// objectFromVars retrieves a named value from vars and returns it only if it is
// a non-nil, known, object-typed value. Used to populate optional eval context
// namespaces (var, steps, each, local, data).
func objectFromVars(vars map[string]cty.Value, key string) (cty.Value, bool) {
	v, ok := vars[key]
	return v, ok && v != cty.NilVal && v.Type().IsObjectType()
}

// ResolveInputExprs evaluates a map of HCL expressions against the provided
// vars map and returns the resulting string map. It is equivalent to
// ResolveInputExprsWithOpts with DefaultFunctionOptions("") — file() and
// fileexists() will error with "workflow directory not configured" if invoked.
// Callers with a known workflow path should use ResolveInputExprsWithOpts.
func ResolveInputExprs(exprs map[string]hcl.Expression, vars map[string]cty.Value) (map[string]string, error) {
	return ResolveInputExprsWithOpts(exprs, vars, DefaultFunctionOptions(""))
}

// ResolveInputExprsWithOpts evaluates a map of HCL expressions against the
// provided vars map and returns the resulting string map. Each expression is
// evaluated with BuildEvalContextWithOpts(vars, opts). If any expression fails
// to evaluate, the error is returned so callers can fail fast. References to
// each.* are detected via expression variable analysis and produce the planned
// message "each is only valid inside for_each".
func ResolveInputExprsWithOpts(exprs map[string]hcl.Expression, vars map[string]cty.Value, opts *FunctionOptions) (map[string]string, error) {
	if len(exprs) == 0 {
		return nil, nil
	}
	ctx := BuildEvalContextWithOpts(vars, opts)
	result := make(map[string]string, len(exprs))
	var errs []string
	for k, expr := range exprs {
		// Check for each.* references before evaluation. Only error when the
		// "each" binding is absent from the context (outside a for_each
		// iteration). When each is present, BuildEvalContext has already
		// included it and evaluation will succeed normally (W07).
		if refsEach(expr) {
			if _, hasEach := vars["each"]; !hasEach {
				errs = append(errs, fmt.Sprintf("input.%s: each is only valid inside for_each", k))
				continue
			}
		}
		val, diags := expr.Value(ctx)
		if diags.HasErrors() {
			errs = append(errs, fmt.Sprintf("input.%s: %s", k, diags.Error()))
			continue
		}
		result[k] = CtyValueToString(val)
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("expression evaluation errors: %s", strings.Join(errs, "; "))
	}
	return result, nil
}

// ResolveInputExprsAsCty evaluates a map of HCL expressions against the provided
// vars map and returns the raw cty.Value map. Unlike ResolveInputExprsWithOpts,
// values are not coerced to strings — callers that need cty.Value (e.g. subworkflow
// step input binding) use this form.
func ResolveInputExprsAsCty(exprs map[string]hcl.Expression, vars map[string]cty.Value, opts *FunctionOptions) (map[string]cty.Value, error) {
	if len(exprs) == 0 {
		return nil, nil
	}
	ctx := BuildEvalContextWithOpts(vars, opts)
	result := make(map[string]cty.Value, len(exprs))
	var errs []string
	for k, expr := range exprs {
		if refsEach(expr) {
			if _, hasEach := vars["each"]; !hasEach {
				errs = append(errs, fmt.Sprintf("input.%s: each is only valid inside for_each", k))
				continue
			}
		}
		val, diags := expr.Value(ctx)
		if diags.HasErrors() {
			errs = append(errs, fmt.Sprintf("input.%s: %s", k, diags.Error()))
			continue
		}
		result[k] = val
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("expression evaluation errors: %s", strings.Join(errs, "; "))
	}
	return result, nil
}

// is the "each" variable. Used to produce the planned error message before
// the HCL evaluator runs, which would otherwise give a generic error.
func refsEach(expr hcl.Expression) bool {
	for _, traversal := range expr.Variables() {
		if len(traversal) > 0 {
			if root, ok := traversal[0].(hcl.TraverseRoot); ok && root.Name == "each" {
				return true
			}
		}
	}
	return false
}

// refsWhile reports whether the expression references the while.* namespace.
// Used to reject while.* references outside while-driven iterating steps.
func refsWhile(expr hcl.Expression) bool {
	for _, traversal := range expr.Variables() {
		if len(traversal) > 0 {
			if root, ok := traversal[0].(hcl.TraverseRoot); ok && root.Name == "while" {
				return true
			}
		}
	}
	return false
}

// CtyValueToString converts a cty.Value to its string representation.
// Lists are rendered as comma-separated values. Unknown/null values yield "".
func CtyValueToString(v cty.Value) string {
	if v == cty.NilVal || v.IsNull() {
		return ""
	}
	if !v.IsKnown() {
		return ""
	}
	switch v.Type() {
	case cty.String:
		return v.AsString()
	case cty.Number:
		bf := v.AsBigFloat()
		return bf.Text('f', -1)
	case cty.Bool:
		if v.True() {
			return "true"
		}
		return "false"
	default:
		if v.Type().IsListType() || v.Type().IsTupleType() {
			var parts []string
			for it := v.ElementIterator(); it.Next(); {
				_, elem := it.Element()
				parts = append(parts, CtyValueToString(elem))
			}
			return strings.Join(parts, ",")
		}
		return v.GoString()
	}
}

// SeedVarsFromGraph initialises the run-scoped vars map from a compiled
// FSMGraph's variable defaults. Returns a map with "var" and "steps" keys.
// Called at run start by the engine.
func SeedVarsFromGraph(g *FSMGraph) map[string]cty.Value {
	varAttrs := make(map[string]cty.Value, len(g.Variables))
	for name, node := range g.Variables {
		if node.Default != cty.NilVal {
			varAttrs[name] = node.Default
		} else {
			// No default declared: use cty.NullVal as placeholder.
			varAttrs[name] = cty.NullVal(node.Type)
		}
	}
	vars := map[string]cty.Value{
		"steps": cty.EmptyObjectVal,
	}
	if len(varAttrs) > 0 {
		vars["var"] = cty.ObjectVal(varAttrs)
	} else {
		vars["var"] = cty.EmptyObjectVal
	}
	return vars
}

// SeedLocalsFromGraph returns a cty object value containing all compiled locals
// from the graph. Returns cty.EmptyObjectVal when there are no locals.
// Called at run start alongside SeedVarsFromGraph.
func SeedLocalsFromGraph(g *FSMGraph) cty.Value {
	if len(g.Locals) == 0 {
		return cty.EmptyObjectVal
	}
	m := make(map[string]cty.Value, len(g.Locals))
	for name, ln := range g.Locals {
		m[name] = ln.Value
	}
	return cty.ObjectVal(m)
}

// SeedDataSnapshot wraps a snapshot from DataStore.Snapshot() into a cty
// object and stores it under vars["data"]. Returns an unmodified vars when
// snap is nil or empty. Call this before building the eval context to expose
// data.* in HCL expressions.
func SeedDataSnapshot(vars, snap map[string]cty.Value) map[string]cty.Value {
	if len(snap) == 0 {
		return vars
	}
	newVars := make(map[string]cty.Value, len(vars)+1)
	for k, v := range vars {
		newVars[k] = v
	}
	newVars["data"] = cty.ObjectVal(snap)
	return newVars
}

// ApplyVarOverrides merges CLI/file-supplied overrides into an existing vars
// map produced by SeedVarsFromGraph. Only keys that match declared variables
// are applied; unknown keys are silently ignored. Each override is converted
// to the declared variable type (and optional object defaults are applied).
func ApplyVarOverrides(g *FSMGraph, vars, overrides map[string]cty.Value) (map[string]cty.Value, error) {
	if len(overrides) == 0 {
		return vars, nil
	}
	varObj := vars["var"]
	existing := map[string]cty.Value{}
	if varObj != cty.NilVal && varObj.Type().IsObjectType() {
		for k := range varObj.Type().AttributeTypes() {
			existing[k] = varObj.GetAttr(k)
		}
	}
	for name, val := range overrides {
		node, ok := g.Variables[name]
		if !ok {
			continue
		}
		converted, err := ParseAndConvertVarOverride(val, node)
		if err != nil {
			return nil, fmt.Errorf("variable %q: %w", name, err)
		}
		existing[name] = converted
	}
	out := map[string]cty.Value{"steps": vars["steps"]}
	// Preserve compiled locals (compile-time constants; not affected by var overrides).
	if local, ok := vars["local"]; ok {
		out["local"] = local
	}
	if len(existing) > 0 {
		out["var"] = cty.ObjectVal(existing)
	} else {
		out["var"] = cty.EmptyObjectVal
	}
	return out, nil
}

// ParseAndConvertVarOverride handles the raw-string CLI case: it parses the
// override string according to the declared variable type (verbatim for string,
// strict scalar parsing for number/bool, HCL expression for everything else),
// then delegates to ConvertVarOverrideValue for optional-default application
// and declared-type conversion.
func ParseAndConvertVarOverride(val cty.Value, node *VariableNode) (cty.Value, error) {
	// Raw CLI strings are parsed according to the declared type so that string
	// variables keep the exact text supplied on the command line. The string
	// branch must be known and non-null before calling AsString; ConvertVarOverrideValue
	// is the single place that reports null/unknown errors.
	if val.IsKnown() && !val.IsNull() && val.Type() == cty.String {
		parsed, err := parseOverrideString(val.AsString(), node.Type)
		if err != nil {
			return cty.NilVal, err
		}
		val = parsed
	}

	return ConvertVarOverrideValue(val, node)
}

// ConvertVarOverrideValue coerces an already-typed override value to a
// variable's declared type, applying optional object defaults before type
// conversion so that partially-specified object overrides receive their
// declared defaults. It is used by var-file values, subworkflow bindings, and
// ParseAndConvertVarOverride after raw CLI parsing.
func ConvertVarOverrideValue(val cty.Value, node *VariableNode) (cty.Value, error) {
	if node.Type == cty.NilType {
		return val, nil
	}
	if !val.IsKnown() {
		return cty.NilVal, fmt.Errorf("override value is unknown")
	}
	if val.IsNull() {
		return cty.NilVal, fmt.Errorf("override value is null")
	}

	// Apply object optional() defaults before conversion.
	if node.TypeDefaults != nil {
		val = ApplyDefaultsIfAny(val, node.TypeDefaults)
	}

	if val.Type().Equals(node.Type) {
		return val, nil
	}
	converted, err := convert.Convert(val, node.Type)
	if err != nil {
		return cty.NilVal, fmt.Errorf("cannot convert %s to %s: %w", val.Type().FriendlyName(), node.Type.FriendlyName(), err)
	}
	return converted, nil
}

// parseOverrideString parses a raw --var value according to the declared
// variable type. String variables receive the text verbatim; number and bool
// variables use legacy scalar parsing; everything else is parsed as an HCL
// expression so that list/map/object literals work on the CLI.
func parseOverrideString(raw string, want cty.Type) (cty.Value, error) {
	if want == cty.String {
		return cty.StringVal(raw), nil
	}
	if want == cty.Number {
		converted, err := convert.Convert(cty.StringVal(raw), cty.Number)
		if err != nil {
			return cty.NilVal, fmt.Errorf("cannot parse %q as number: %w", raw, err)
		}
		return converted, nil
	}
	if want == cty.Bool {
		switch raw {
		case "true", "1":
			return cty.BoolVal(true), nil
		case "false", "0":
			return cty.BoolVal(false), nil
		default:
			return cty.NilVal, fmt.Errorf("cannot parse %q as bool: expected true/false/1/0", raw)
		}
	}

	expr, diags := hclsyntax.ParseExpression([]byte(raw), "<var>", hcl.Pos{Line: 1, Column: 1})
	if diags.HasErrors() {
		return cty.NilVal, fmt.Errorf("cannot parse override value %q: %w", raw, diags)
	}
	parsed, diags := expr.Value(nil)
	if diags.HasErrors() {
		return cty.NilVal, fmt.Errorf("cannot parse override value %q: %w", raw, diags)
	}
	if !parsed.IsKnown() {
		return cty.NilVal, fmt.Errorf("cannot parse override value %q: value is unknown", raw)
	}
	return parsed, nil
}

// WithStepOutputs returns a new vars map with the given step's outputs merged
// under vars["steps"][stepName]. Existing step entries are preserved.
func WithStepOutputs(vars map[string]cty.Value, stepName string, outputs map[string]cty.Value) map[string]cty.Value {
	if vars == nil {
		vars = map[string]cty.Value{
			"var":   cty.EmptyObjectVal,
			"steps": cty.EmptyObjectVal,
		}
	}

	// Build the step output object. Values are stored with their native cty type
	// (object/array/number/bool/string) so downstream expressions see structured
	// data without jsondecode().
	stepVals := make(map[string]cty.Value, len(outputs))
	for k, v := range outputs {
		stepVals[k] = v
	}

	// Merge into the existing steps object.
	stepsAttrs := map[string]cty.Value{}
	if existing, ok := vars["steps"]; ok && existing != cty.NilVal && existing.Type().IsObjectType() {
		for k := range existing.Type().AttributeTypes() {
			stepsAttrs[k] = existing.GetAttr(k)
		}
	}
	if len(stepVals) > 0 {
		stepsAttrs[stepName] = cty.ObjectVal(stepVals)
	}

	// Shallow copy of vars with updated steps.
	newVars := make(map[string]cty.Value, len(vars))
	for k, v := range vars {
		newVars[k] = v
	}
	if len(stepsAttrs) > 0 {
		newVars["steps"] = cty.ObjectVal(stepsAttrs)
	} else {
		newVars["steps"] = cty.EmptyObjectVal
	}
	return newVars
}

// EachBinding carries the per-iteration binding values for step-level
// for_each / count iteration (W10). The engine sets these on each step entry.
type EachBinding struct {
	// Value is the current item value (list element or map value).
	Value cty.Value
	// Key is the current item key. For lists/count this is the string
	// representation of the zero-based index. For maps it is the map key.
	Key cty.Value
	// Index is the zero-based iteration index.
	Index int
	// Total is the total number of iterations.
	Total int
	// First is true on the first iteration (Index == 0).
	First bool
	// Last is true on the last iteration (Index == Total-1).
	Last bool
	// Prev is the cty.Value of the previous iteration's item, or cty.NilVal
	// on the first iteration.
	Prev cty.Value
}

// WhileBinding carries the per-iteration binding values for while-driven
// iteration steps. The engine sets these before each iteration so that input
// expressions can reference while.index, while.first, and while._prev.
type WhileBinding struct {
	// Index is the zero-based iteration counter.
	Index int
	// First is true on the first iteration (Index == 0).
	First bool
	// Prev is the output of the previous iteration, or cty.NilVal before
	// the first iteration.
	Prev cty.Value
}

// WithEachBinding returns a new vars map with each.* bound for the current
// step-level iteration. Called by the engine node before executing the
// iteration step so that input expressions can reference each.value, each.key,
// each._idx, each._total, each._first, each._last, and each._prev.
func WithEachBinding(vars map[string]cty.Value, b *EachBinding) map[string]cty.Value {
	newVars := make(map[string]cty.Value, len(vars)+1)
	for k, v := range vars {
		newVars[k] = v
	}
	key := b.Key
	if key == cty.NilVal {
		key = cty.StringVal(fmt.Sprintf("%d", b.Index))
	}
	prev := b.Prev
	if prev == cty.NilVal {
		prev = cty.NullVal(cty.DynamicPseudoType)
	}
	newVars["each"] = cty.ObjectVal(map[string]cty.Value{
		"value":  b.Value,
		"key":    key,
		"_idx":   cty.NumberIntVal(int64(b.Index)),
		"_total": cty.NumberIntVal(int64(b.Total)),
		"_first": cty.BoolVal(b.First),
		"_last":  cty.BoolVal(b.Last),
		"_prev":  prev,
	})
	return newVars
}

// ClearEachBinding returns a new vars map with the each bindings removed.
// Called by the engine loop after a _continue interception to ensure each.*
// is not accessible outside the per-iteration step.
func ClearEachBinding(vars map[string]cty.Value) map[string]cty.Value {
	if _, ok := vars["each"]; !ok {
		return vars
	}
	newVars := make(map[string]cty.Value, len(vars))
	for k, v := range vars {
		if k != "each" {
			newVars[k] = v
		}
	}
	return newVars
}

// WithWhileBinding returns a new vars map with while.* bound for the current
// iteration of a while-driven step. Called by the engine before each iteration
// body so that input expressions can reference while.index, while.first, and
// while._prev.
func WithWhileBinding(vars map[string]cty.Value, b *WhileBinding) map[string]cty.Value {
	newVars := make(map[string]cty.Value, len(vars)+1)
	for k, v := range vars {
		newVars[k] = v
	}
	prev := b.Prev
	if prev == cty.NilVal {
		prev = cty.NullVal(cty.DynamicPseudoType)
	}
	newVars["while"] = cty.ObjectVal(map[string]cty.Value{
		"index": cty.NumberIntVal(int64(b.Index)),
		"first": cty.BoolVal(b.First),
		"_prev": prev,
	})
	return newVars
}

// ClearWhileBinding returns a new vars map with the while bindings removed.
// Called by the engine loop after a while iteration completes.
func ClearWhileBinding(vars map[string]cty.Value) map[string]cty.Value {
	if _, ok := vars["while"]; !ok {
		return vars
	}
	newVars := make(map[string]cty.Value, len(vars))
	for k, v := range vars {
		if k != "while" {
			newVars[k] = v
		}
	}
	return newVars
}

// WithIndexedStepOutput returns a new vars map with an indexed step output
// entry added. For list/count iteration, index is a cty.Number (the
// zero-based iteration index). For map iteration, index is a cty.String (the
// map key). The result allows expressions like steps.foo[0].x and
// steps.foo["key"].x.
//
// Internally, indexed outputs are stored under vars["steps"][stepName] as a
// cty map or tuple. The engine accumulates entries across iterations.
func WithIndexedStepOutput(vars map[string]cty.Value, stepName string, index cty.Value, outputs map[string]cty.Value) map[string]cty.Value {
	if vars == nil {
		vars = map[string]cty.Value{
			"var":   cty.EmptyObjectVal,
			"steps": cty.EmptyObjectVal,
		}
	}

	// Build the new entry object for this iteration's outputs, preserving the
	// native cty type of each value.
	entryVals := make(map[string]cty.Value, len(outputs))
	for k, v := range outputs {
		entryVals[k] = v
	}
	var entry cty.Value
	if len(entryVals) > 0 {
		entry = cty.ObjectVal(entryVals)
	} else {
		entry = cty.EmptyObjectVal
	}

	// Retrieve existing step entry (if any) which may be an object (scalar),
	// a list-in-progress (map[string]cty.Value with numeric string keys), or absent.
	stepsAttrs := map[string]cty.Value{}
	if existing, ok := vars["steps"]; ok && existing != cty.NilVal && existing.Type().IsObjectType() {
		for k := range existing.Type().AttributeTypes() {
			stepsAttrs[k] = existing.GetAttr(k)
		}
	}

	// Store the indexed entry. We accumulate entries in an object keyed by the
	// string representation of the index. This is later reconstructable by the
	// engine into a cty tuple (numeric index) or map (string key).
	indexKey := CtyValueToString(index)

	// Get or create the accumulator object for this step.
	accum := map[string]cty.Value{}
	if existing, ok := stepsAttrs[stepName]; ok && existing != cty.NilVal && existing.Type().IsObjectType() {
		for k := range existing.Type().AttributeTypes() {
			accum[k] = existing.GetAttr(k)
		}
	}
	accum[indexKey] = entry

	if len(accum) > 0 {
		stepsAttrs[stepName] = cty.ObjectVal(accum)
	}

	newVars := make(map[string]cty.Value, len(vars))
	for k, v := range vars {
		newVars[k] = v
	}
	if len(stepsAttrs) > 0 {
		newVars["steps"] = cty.ObjectVal(stepsAttrs)
	} else {
		newVars["steps"] = cty.EmptyObjectVal
	}
	return newVars
}

// SerializeVarScope encodes the run vars map and optional iteration cursor
// stack into a JSON string for persistence. The format is:
//
//	{"var": {"name": "value"}, "steps": {"step": {"key": "value"}}, "iter": [...]}
//
// The "var" map uses string values only; null-valued variables are omitted
// (key absent). On restore, absent keys fall back to FSMGraph defaults.
// The iter field is a JSON array of cursor objects; it is omitted when the
// stack is empty. The server stores this blob verbatim without interpreting
// cursor internals. See IterCursor for field documentation.
func SerializeVarScope(vars map[string]cty.Value, cursorStack ...[]IterCursor) (string, error) {
	scope := map[string]interface{}{}
	if varObj, ok := vars["var"]; ok && varObj != cty.NilVal && varObj.Type().IsObjectType() {
		varMap := map[string]string{}
		for k := range varObj.Type().AttributeTypes() {
			v := varObj.GetAttr(k)
			if !v.IsKnown() {
				return "", fmt.Errorf("cannot serialize unknown value for variable %q", k)
			}
			if !v.IsNull() {
				varMap[k] = CtyValueToString(v)
			}
		}
		scope["var"] = varMap
	}
	if stepsObj, ok := vars["steps"]; ok && stepsObj != cty.NilVal && stepsObj.Type().IsObjectType() {
		// Step outputs are stored with their native cty type. Serialize the whole
		// steps object as a cty-JSON value plus its type so structured/typed
		// outputs (object/array/number/bool) round-trip losslessly on reattach.
		// A legacy string-map "steps" form is still read on restore for in-flight
		// runs checkpointed before this format existed.
		valBytes, errV := ctyjson.Marshal(stepsObj, stepsObj.Type())
		typeBytes, errT := ctyjson.MarshalType(stepsObj.Type())
		if err := errors.Join(errV, errT); err != nil {
			return "", fmt.Errorf("cannot serialize step outputs: %w", err)
		}
		scope["steps_typed"] = string(valBytes)
		scope["steps_typed_type"] = string(typeBytes)
	}
	// Encode the iteration cursor stack when provided. Items are intentionally
	// omitted from each cursor (re-evaluated on reattach).
	var stack []IterCursor
	if len(cursorStack) > 0 {
		stack = cursorStack[0]
	}
	if len(stack) > 0 {
		cursorList := make([]interface{}, 0, len(stack))
		for i := range stack {
			c := &stack[i]
			cm := map[string]interface{}{
				"step":        c.StepName,
				"index":       c.Index,
				"total":       c.Total,
				"any_failed":  c.AnyFailed,
				"in_progress": c.InProgress,
			}
			if c.OnFailure != "" {
				cm["on_failure"] = c.OnFailure
			}
			if c.Key != cty.NilVal {
				cm["key"] = CtyValueToString(c.Key)
			}
			if c.Prev != cty.NilVal {
				typeBytes, err1 := ctyjson.MarshalType(c.Prev.Type())
				valBytes, err2 := ctyjson.Marshal(c.Prev, c.Prev.Type())
				if err1 == nil && err2 == nil {
					cm["prev"] = string(valBytes)
					cm["prev_type"] = string(typeBytes)
				}
			}
			cursorList = append(cursorList, cm)
		}
		scope["iter"] = cursorList
	}
	b, err := json.Marshal(scope)
	return string(b), err
}

// typedStepsFromScope extracts the typed (cty-JSON) step-output value and type
// bytes from a decoded scope map. It returns ok=false when the typed form is
// absent (e.g. a legacy checkpoint), so callers can fall back to the string form.
func typedStepsFromScope(raw map[string]interface{}) (valBytes, typeBytes []byte, ok bool) {
	v, vok := raw["steps_typed"].(string)
	t, tok := raw["steps_typed_type"].(string)
	if !vok || !tok {
		return nil, nil, false
	}
	return []byte(v), []byte(t), true
}

// restoreStepOutputs rebuilds the vars["steps"] object from a decoded scope map.
// It prefers the typed (cty-JSON) form, which preserves structured/native types,
// and falls back to the legacy string-map form for runs checkpointed before the
// typed format existed. Returns cty.NilVal when no step outputs are present.
func restoreStepOutputs(raw map[string]interface{}) (cty.Value, error) {
	if stepsVal, stepsTypeBytes, ok := typedStepsFromScope(raw); ok {
		stepsType, err := ctyjson.UnmarshalType(stepsTypeBytes)
		if err != nil {
			return cty.NilVal, fmt.Errorf("restore typed step outputs: type: %w", err)
		}
		v, err := ctyjson.Unmarshal(stepsVal, stepsType)
		if err != nil {
			return cty.NilVal, fmt.Errorf("restore typed step outputs: %w", err)
		}
		if v.Type().IsObjectType() {
			return v, nil
		}
		return cty.NilVal, nil
	}

	stepsData, ok := raw["steps"].(map[string]interface{})
	if !ok {
		return cty.NilVal, nil
	}
	stepsAttrs := map[string]cty.Value{}
	for stepName, stepOutputsRaw := range stepsData {
		outputMap, ok := stepOutputsRaw.(map[string]interface{})
		if !ok {
			continue
		}
		stepVals := make(map[string]cty.Value, len(outputMap))
		for k, v := range outputMap {
			if sv, ok := v.(string); ok {
				stepVals[k] = cty.StringVal(sv)
			}
		}
		if len(stepVals) > 0 {
			stepsAttrs[stepName] = cty.ObjectVal(stepVals)
		}
	}
	if len(stepsAttrs) > 0 {
		return cty.ObjectVal(stepsAttrs), nil
	}
	return cty.NilVal, nil
}

// RestoreVarScope rebuilds a run's vars map and iteration cursor stack from
// a JSON-encoded scope snapshot and the compiled workflow graph. Variable
// defaults come from the graph; step outputs are restored from the JSON scope.
//
// The returned []IterCursor is non-nil only when the scope JSON contains an
// "iter" field. Each cursor's Items field is nil; the step re-evaluates the
// expression on re-entry.
func RestoreVarScope(scopeJSON string, g *FSMGraph) (map[string]cty.Value, []IterCursor, error) {
	vars := SeedVarsFromGraph(g)

	if scopeJSON == "" {
		return vars, nil, nil
	}

	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(scopeJSON), &raw); err != nil {
		return vars, nil, fmt.Errorf("restore scope: %w", err)
	}

	// Restore step outputs (typed cty-JSON form, or legacy string-map fallback).
	if steps, err := restoreStepOutputs(raw); err != nil {
		return vars, nil, err
	} else if steps != cty.NilVal {
		vars["steps"] = steps
	}

	// Restore iteration cursor stack.
	// Support both W10 array format and W07 single-object format.
	var stack []IterCursor
	if iterRaw, ok := raw["iter"]; ok {
		switch v := iterRaw.(type) {
		case []interface{}:
			for _, elem := range v {
				if m, ok := elem.(map[string]interface{}); ok {
					stack = append(stack, deserializeIterCursor(m))
				}
			}
		case map[string]interface{}:
			// Legacy W07 single-cursor format.
			stack = []IterCursor{deserializeIterCursor(v)}
		}
	}

	return vars, stack, nil
}
