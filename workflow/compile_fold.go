package workflow

// compile_fold.go — constant-fold evaluator for compile-time expression
// reduction over the closure (var ∪ local ∪ literal ∪ funcs).

import (
	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/function"
)

// runtimeOnlyNamespaces is the set of variable-root names whose values are
// only available at runtime. Expressions that reference any of these roots are
// deferred to the engine; they are not errors at compile time.
var runtimeOnlyNamespaces = map[string]bool{
	"each":            true,
	"steps":           true,
	"shared_variable": true,
}

// FoldExpr evaluates expr in the closure (var ∪ local ∪ literal ∪ funcs).
// Returns the cty.Value if the expression folds, or (cty.NilVal, false, nil)
// when the expression references runtime-only namespaces (each, steps,
// shared_variable). Runtime-only refs are not errors — they signal "leave
// this expression for the engine".
//
// Diagnostics are returned for fold-time failures (unknown var/local name,
// type mismatch, file-not-found via file()/fileexists()). When any diagnostic
// has DiagError severity the expression is a compile failure; the caller
// should propagate the diagnostics.
//
// vars maps variable names to their resolved cty.Values (i.e. the flat
// map used as the "var" object — keys are variable names, not "var").
// locals maps local names to their resolved cty.Values — keys are local
// names, not "local". Both maps may be nil or empty.
//
// workflowDir is forwarded to the function registry so file() and
// fileexists() can resolve relative paths. When workflowDir is "", file()
// and fileexists() are replaced with stubs that return unknown values so
// that var/local reference checks still run without triggering
// "workflow directory not configured" errors.
func FoldExpr(
	expr hcl.Expression,
	vars map[string]cty.Value,
	locals map[string]cty.Value,
	workflowDir string,
) (cty.Value, bool, hcl.Diagnostics) {
	// Walk the variable traversals. If any root belongs to a runtime-only
	// namespace, the entire expression is deferred — not an error.
	for _, traversal := range expr.Variables() {
		if len(traversal) == 0 {
			continue
		}
		root, ok := traversal[0].(hcl.TraverseRoot)
		if !ok {
			continue
		}
		if runtimeOnlyNamespaces[root.Name] {
			return cty.NilVal, false, nil
		}
	}

	funcs := workflowFunctions(DefaultFunctionOptions(workflowDir))
	if workflowDir == "" {
		// Stub file() and fileexists() so var/local reference checks still run
		// but path validation is not attempted when no workflow directory is
		// configured. Literal-string args return unknown rather than erroring.
		funcs["file"] = function.New(&function.Spec{
			Params: []function.Parameter{{Name: "path", Type: cty.String}},
			Type:   function.StaticReturnType(cty.String),
			Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
				return cty.UnknownVal(cty.String), nil
			},
		})
		funcs["fileexists"] = function.New(&function.Spec{
			Params: []function.Parameter{{Name: "path", Type: cty.String}},
			Type:   function.StaticReturnType(cty.Bool),
			Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
				return cty.UnknownVal(cty.Bool), nil
			},
		})
	}

	ctx := &hcl.EvalContext{
		Variables: map[string]cty.Value{
			"var":   ctyObjectOrEmpty(vars),
			"local": ctyObjectOrEmpty(locals),
		},
		Functions: funcs,
	}

	val, diags := expr.Value(ctx)
	return val, true, diags
}

// ctyObjectOrEmpty converts a flat name→value map into a cty object value.
// Returns cty.EmptyObjectVal when the map is nil or empty so callers do not
// need to guard against nil-map panics.
func ctyObjectOrEmpty(m map[string]cty.Value) cty.Value {
	if len(m) == 0 {
		return cty.EmptyObjectVal
	}
	return cty.ObjectVal(m)
}

// graphVars builds a flat name→value map from g.Variables for use with
// FoldExpr. Variables without a default are represented as cty.UnknownVal so
// that expressions referencing them (including file(var.x)) are treated as
// producing an unknown result rather than causing a type error or spurious
// "workflow directory not configured" failure. Only declared variables with a
// known default are considered foldable.
func graphVars(g *FSMGraph) map[string]cty.Value {
	if len(g.Variables) == 0 {
		return nil
	}
	m := make(map[string]cty.Value, len(g.Variables))
	for name, node := range g.Variables {
		if node.Default != cty.NilVal {
			m[name] = node.Default
		} else {
			m[name] = cty.UnknownVal(node.Type)
		}
	}
	return m
}

// graphLocals builds a flat name→value map from g.Locals for use with
// FoldExpr.
func graphLocals(g *FSMGraph) map[string]cty.Value {
	if len(g.Locals) == 0 {
		return nil
	}
	m := make(map[string]cty.Value, len(g.Locals))
	for name, node := range g.Locals {
		m[name] = node.Value
	}
	return m
}
