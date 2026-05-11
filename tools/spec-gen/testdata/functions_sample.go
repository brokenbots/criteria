// Package testdata provides sample function definitions for spec-gen unit tests.
// This file intentionally mirrors the shape of workflow/eval_functions.go.
package testdata

import (
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/function"
)

// workflowFunctions returns test expression functions.
func workflowFunctions() map[string]function.Function {
	return map[string]function.Function{
		"greet": greetFunction(),
		"ping":  pingFunction(),
	}
}

// greetFunction implements the greet(name) → string function.
// Returns a greeting string for the provided name.
func greetFunction() function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{{Name: "name", Type: cty.String}},
		Type:   function.StaticReturnType(cty.String),
	})
}

// pingFunction implements the ping() → bool function.
// Returns true when the target is reachable.
func pingFunction() function.Function {
	return function.New(&function.Spec{
		Type: function.StaticReturnType(cty.Bool),
	})
}
