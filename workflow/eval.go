// Package workflow — eval.go provides the HCL evaluation context builder and
// helpers for runtime expression evaluation introduced in W04.
package workflow

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
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
func BuildEvalContext(vars map[string]cty.Value) *hcl.EvalContext {
	varObj := cty.EmptyObjectVal
	stepsObj := cty.EmptyObjectVal

	if v, ok := vars["var"]; ok && v != cty.NilVal && v.Type().IsObjectType() {
		varObj = v
	}
	if s, ok := vars["steps"]; ok && s != cty.NilVal && s.Type().IsObjectType() {
		stepsObj = s
	}

	ctxVars := map[string]cty.Value{
		"var":   varObj,
		"steps": stepsObj,
	}

	// Include "each" bindings when inside a for_each iteration (W07).
	if each, ok := vars["each"]; ok && each != cty.NilVal && each.Type().IsObjectType() {
		ctxVars["each"] = each
	}

	return &hcl.EvalContext{
		Variables: ctxVars,
	}
}

// ResolveInputExprs evaluates a map of HCL expressions against the provided
// vars map and returns the resulting string map. Each expression is evaluated
// with BuildEvalContext(vars). If any expression fails to evaluate, the error
// is returned so callers can fail fast. References to each.* are detected via
// expression variable analysis and produce the planned message
// "each is only valid inside for_each".
func ResolveInputExprs(exprs map[string]hcl.Expression, vars map[string]cty.Value) (map[string]string, error) {
	if len(exprs) == 0 {
		return nil, nil
	}
	ctx := BuildEvalContext(vars)
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

// refsEach returns true if the expression contains any traversal whose root
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

// ApplyVarOverrides merges CLI-supplied key=value pairs into an existing vars
// map produced by SeedVarsFromGraph. Only keys that match declared variables
// are applied; unknown keys are silently ignored. Values are coerced to the
// declared variable type (only string is supported today).
func ApplyVarOverrides(g *FSMGraph, vars map[string]cty.Value, overrides map[string]string) map[string]cty.Value {
	if len(overrides) == 0 {
		return vars
	}
	varObj, _ := vars["var"]
	existing := map[string]cty.Value{}
	if varObj != cty.NilVal && varObj.Type().IsObjectType() {
		for k := range varObj.Type().AttributeTypes() {
			existing[k] = varObj.GetAttr(k)
		}
	}
	for name, raw := range overrides {
		node, ok := g.Variables[name]
		if !ok {
			continue
		}
		switch node.Type {
		case cty.String:
			existing[name] = cty.StringVal(raw)
		case cty.Number:
			var f float64
			if _, err := fmt.Sscanf(raw, "%g", &f); err == nil {
				existing[name] = cty.NumberFloatVal(f)
			}
		case cty.Bool:
			existing[name] = cty.BoolVal(raw == "true" || raw == "1")
		}
	}
	out := map[string]cty.Value{"steps": vars["steps"]}
	if len(existing) > 0 {
		out["var"] = cty.ObjectVal(existing)
	} else {
		out["var"] = cty.EmptyObjectVal
	}
	return out
}

// WithStepOutputs returns a new vars map with the given step's outputs merged
// under vars["steps"][stepName]. Existing step entries are preserved.
func WithStepOutputs(vars map[string]cty.Value, stepName string, outputs map[string]string) map[string]cty.Value {
	if vars == nil {
		vars = map[string]cty.Value{
			"var":   cty.EmptyObjectVal,
			"steps": cty.EmptyObjectVal,
		}
	}

	// Build the step output object.
	stepVals := make(map[string]cty.Value, len(outputs))
	for k, v := range outputs {
		stepVals[k] = cty.StringVal(v)
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

// WithEachBinding returns a new vars map with each.value and each.index bound
// for the current for_each iteration (W07). Called by the for_each engine node
// before dispatching the per-iteration step so that input expressions can
// reference each.value and each.index.
func WithEachBinding(vars map[string]cty.Value, value cty.Value, index int) map[string]cty.Value {
	newVars := make(map[string]cty.Value, len(vars)+1)
	for k, v := range vars {
		newVars[k] = v
	}
	newVars["each"] = cty.ObjectVal(map[string]cty.Value{
		"value": value,
		"index": cty.NumberIntVal(int64(index)),
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

// SerializeVarScope encodes the run vars map and optional iteration cursor
// into a JSON string for persistence in the server. The format is:
//
//	{"var": {"name": "value"}, "steps": {"step": {"key": "value"}}, "iter": {...}}
//
// The iter field is only present when cursor is non-nil. The server stores this
// blob verbatim and does not interpret the iter shape. See IterCursor for the
// field documentation (authoritative for Phase 1.6 SDK extraction).
func SerializeVarScope(vars map[string]cty.Value, cursor ...*IterCursor) (string, error) {
	scope := map[string]interface{}{}
	if varObj, ok := vars["var"]; ok && varObj != cty.NilVal && varObj.Type().IsObjectType() {
		varMap := map[string]string{}
		for k := range varObj.Type().AttributeTypes() {
			v := varObj.GetAttr(k)
			varMap[k] = CtyValueToString(v)
		}
		scope["var"] = varMap
	}
	if stepsObj, ok := vars["steps"]; ok && stepsObj != cty.NilVal && stepsObj.Type().IsObjectType() {
		stepsMap := map[string]map[string]string{}
		for stepName := range stepsObj.Type().AttributeTypes() {
			stepObj := stepsObj.GetAttr(stepName)
			if !stepObj.Type().IsObjectType() {
				continue
			}
			stepOutputs := map[string]string{}
			for k := range stepObj.Type().AttributeTypes() {
				stepOutputs[k] = CtyValueToString(stepObj.GetAttr(k))
			}
			stepsMap[stepName] = stepOutputs
		}
		scope["steps"] = stepsMap
	}
	// Encode the iteration cursor when provided (W07). The server stores the iter
	// blob verbatim; Items are intentionally omitted (re-evaluated on reattach).
	var cur *IterCursor
	if len(cursor) > 0 {
		cur = cursor[0]
	}
	if cur != nil {
		scope["iter"] = map[string]interface{}{
			"node":        cur.NodeName,
			"index":       cur.Index,
			"any_failed":  cur.AnyFailed,
			"in_progress": cur.InProgress,
		}
	}
	b, err := json.Marshal(scope)
	return string(b), err
}

// RestoreVarScope rebuilds a run's vars map and optional iteration cursor from
// a JSON-encoded scope snapshot and the compiled workflow graph. Variable
// defaults come from the graph; step outputs are restored from the JSON scope.
// The returned IterCursor is non-nil only when the scope JSON contains an
// "iter" field (i.e. a for_each was active when the scope was last persisted).
// The cursor's Items field is intentionally nil; the for_each engine node
// re-evaluates the items expression on re-entry (W07).
func RestoreVarScope(scopeJSON string, g *FSMGraph) (map[string]cty.Value, *IterCursor, error) {
	// Start from graph defaults so any new variables added since the crash
	// are seeded correctly.
	vars := SeedVarsFromGraph(g)

	if scopeJSON == "" {
		return vars, nil, nil
	}

	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(scopeJSON), &raw); err != nil {
		return vars, nil, fmt.Errorf("restore scope: %w", err)
	}

	// Restore steps outputs. Variable values come from graph defaults (read-only
	// in W04), so the "var" section in the JSON is intentionally ignored.
	if stepsData, ok := raw["steps"].(map[string]interface{}); ok {
		stepsAttrs := map[string]cty.Value{}
		for stepName, stepOutputsRaw := range stepsData {
			if outputMap, ok := stepOutputsRaw.(map[string]interface{}); ok {
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
		}
		if len(stepsAttrs) > 0 {
			vars["steps"] = cty.ObjectVal(stepsAttrs)
		}
	}

	// Restore iteration cursor if present (W07). Items are intentionally not
	// persisted; the for_each node re-evaluates them on re-entry.
	var cursor *IterCursor
	if iterData, ok := raw["iter"].(map[string]interface{}); ok {
		cursor = &IterCursor{}
		if v, ok := iterData["node"].(string); ok {
			cursor.NodeName = v
		}
		if v, ok := iterData["index"].(float64); ok {
			cursor.Index = int(v)
		}
		if v, ok := iterData["any_failed"].(bool); ok {
			cursor.AnyFailed = v
		}
		if v, ok := iterData["in_progress"].(bool); ok {
			cursor.InProgress = v
		}
	}

	return vars, cursor, nil
}
