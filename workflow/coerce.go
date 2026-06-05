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

// DecodeTypedJSON decodes a single JSON-encoded output value against its declared
// cty type. For a concrete declared type (number, bool, string, object/list shape)
// it decodes strictly against that type. For cty.NilType (undeclared) or
// cty.DynamicPseudoType (declared object/array with no sub-schema) it infers the
// type from the JSON content, so natively-encoded structured values keep their
// shape. This is the wire counterpart to CoerceStringToCty for adapters that emit
// native JSON (ExecuteResult.outputs_json) rather than stringified outputs.
func DecodeTypedJSON(raw []byte, t cty.Type) (cty.Value, error) {
	if t != cty.NilType && t != cty.DynamicPseudoType {
		return ctyjson.Unmarshal(raw, t)
	}
	ty, err := ctyjson.ImpliedType(raw)
	if err != nil {
		return cty.NilVal, err
	}
	return ctyjson.Unmarshal(raw, ty)
}

// RenderOutputValue converts a typed step output cty.Value to the string form
// used by the string-based event surface (OnStepOutputCaptured) and redaction
// registration. Plain strings are rendered raw (unquoted) to match the historical
// adapter-output convention; all other types are JSON-encoded via the cty codec
// so structured values remain machine-parseable. Unknown values render as "null".
//
// This is intentionally distinct from the RunOutputs rendering path, which
// JSON-encodes strings (quoted) per its documented typed-consumer contract.
func RenderOutputValue(val cty.Value) (string, error) {
	if !val.IsKnown() {
		return "null", nil
	}
	if !val.IsNull() && val.Type() == cty.String {
		return val.AsString(), nil
	}
	b, err := ctyjson.Marshal(val, val.Type())
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// RenderOutputs renders a typed output map to the flat string map used by the
// event sink. It is best-effort: a value that fails to render is replaced with
// the cty codec's null token rather than failing the run, since events are a
// display surface, not the canonical store.
func RenderOutputs(outputs map[string]cty.Value) map[string]string {
	if len(outputs) == 0 {
		return nil
	}
	out := make(map[string]string, len(outputs))
	for k, v := range outputs {
		s, err := RenderOutputValue(v)
		if err != nil {
			s = "null"
		}
		out[k] = s
	}
	return out
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
