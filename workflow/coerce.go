package workflow

// coerce.go — the single primitive for turning a raw adapter string output into
// a typed cty.Value against a declared type. Lives in the workflow package so
// both the engine (data writes) and the typed-output storage helpers can share
// one implementation; the engine package depends on workflow, not the reverse.

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"
)

// CoerceStringToCty converts a raw adapter string output to the given cty type.
//
// Scalars (string, number, bool) are parsed directly. Structured declared types
// (object, list, map, set, tuple) and cty.DynamicPseudoType are decoded from JSON
// via the cty JSON codec so that an adapter which emits a JSON-encoded structure
// is consumed as a native typed value — no jsondecode() needed downstream.
//
// Leniency: when t is cty.DynamicPseudoType (the adapter declared "object"/"array"
// with no sub-schema) and s does not parse as JSON, the raw string is returned as
// a cty.String rather than failing the run. A concrete declared type that fails to
// decode is a hard error, since the adapter promised that shape.
func CoerceStringToCty(s string, t cty.Type) (cty.Value, error) {
	switch t {
	case cty.NilType:
		// No declared type: preserve the raw string (permissive).
		return cty.StringVal(s), nil
	case cty.String:
		return cty.StringVal(s), nil
	case cty.Number:
		f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil {
			return cty.NilVal, fmt.Errorf("cannot coerce %q to type number: %w", s, err)
		}
		return cty.NumberFloatVal(f), nil
	case cty.Bool:
		switch strings.TrimSpace(s) {
		case "true", "1":
			return cty.BoolVal(true), nil
		case "false", "0":
			return cty.BoolVal(false), nil
		default:
			return cty.NilVal, fmt.Errorf("cannot coerce %q to type bool: expected true/false/1/0", s)
		}
	case cty.DynamicPseudoType:
		return coerceDynamicJSON(s)
	default:
		// Concrete structured type (object/list/map/set/tuple): decode JSON
		// strictly against the declared shape.
		v, err := ctyjson.Unmarshal([]byte(s), t)
		if err != nil {
			return cty.NilVal, fmt.Errorf("cannot coerce %q to type %s: %w", s, t.FriendlyName(), err)
		}
		return v, nil
	}
}

// coerceDynamicJSON decodes s as JSON to whatever cty type the content implies,
// falling back to a bare string when s is not valid JSON.
func coerceDynamicJSON(s string) (cty.Value, error) {
	ty, err := ctyjson.ImpliedType([]byte(s))
	if err != nil {
		// Not valid JSON — treat the value as a plain string.
		return cty.StringVal(s), nil //nolint:nilerr // lenient fallback is intentional
	}
	v, err := ctyjson.Unmarshal([]byte(s), ty)
	if err != nil {
		return cty.StringVal(s), nil //nolint:nilerr // lenient fallback is intentional
	}
	return v, nil
}
