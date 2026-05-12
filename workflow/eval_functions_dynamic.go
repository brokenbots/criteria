package workflow

// eval_functions_dynamic.go — uuid() and timestamp() HCL functions.
//
// Both functions are NON-DETERMINISTIC: each call produces a new value.
// Workflow authors who need stable values across crash-resume cycles should
// capture the result into a step output (steps.<name>.<key>) and reference
// that downstream instead of calling these functions repeatedly.

import (
	"time"

	"github.com/google/uuid"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/function"
)

func registerDynamicFunctions() map[string]function.Function {
	return map[string]function.Function{
		"uuid":      uuidFunction(),
		"timestamp": timestampFunction(),
	}
}

// uuidFunction returns an RFC 4122 v4 UUID string. NON-DETERMINISTIC:
// each call produces a new value. Capture into a step output for crash-resume
// stability.
func uuidFunction() function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{},
		Type:   function.StaticReturnType(cty.String),
		Impl: func(_ []cty.Value, _ cty.Type) (cty.Value, error) {
			return cty.StringVal(uuid.NewString()), nil
		},
	})
}

// timestampFunction returns the current UTC time in RFC 3339 format.
// NON-DETERMINISTIC: successive calls return different values. Capture into
// a step output for crash-resume stability.
func timestampFunction() function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{},
		Type:   function.StaticReturnType(cty.String),
		Impl: func(_ []cty.Value, _ cty.Type) (cty.Value, error) {
			return cty.StringVal(time.Now().UTC().Format(time.RFC3339)), nil
		},
	})
}
