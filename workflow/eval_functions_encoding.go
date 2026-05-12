package workflow

// eval_functions_encoding.go — base64, JSON, URL, and YAML HCL functions.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"

	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/function"
	ctyjson "github.com/zclconf/go-cty/cty/json"
	"gopkg.in/yaml.v3"
)

func registerEncodingFunctions() map[string]function.Function {
	return map[string]function.Function{
		"base64encode": base64EncodeFunction(),
		"base64decode": base64DecodeFunction(),
		"jsonencode":   jsonEncodeFunction(),
		"jsondecode":   jsonDecodeFunction(),
		"urlencode":    urlEncodeFunction(),
		"yamlencode":   yamlEncodeFunction(),
		"yamldecode":   yamlDecodeFunction(),
	}
}

func base64EncodeFunction() function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{{Name: "value", Type: cty.String}},
		Type:   function.StaticReturnType(cty.String),
		Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
			return cty.StringVal(base64.StdEncoding.EncodeToString([]byte(args[0].AsString()))), nil
		},
	})
}

func base64DecodeFunction() function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{{Name: "value", Type: cty.String}},
		Type:   function.StaticReturnType(cty.String),
		Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
			decoded, err := base64.StdEncoding.DecodeString(args[0].AsString())
			if err != nil {
				return cty.StringVal(""), fmt.Errorf("base64decode(): %w", err)
			}
			return cty.StringVal(string(decoded)), nil
		},
	})
}

func jsonEncodeFunction() function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{{Name: "value", Type: cty.DynamicPseudoType, AllowNull: true}},
		Type:   function.StaticReturnType(cty.String),
		Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
			// ctyjson.Marshal cannot fail for concrete, known values; the function
			// spec prevents unknown inputs from reaching this point.
			data, _ := ctyjson.Marshal(args[0], args[0].Type())
			return cty.StringVal(string(data)), nil
		},
	})
}

func jsonDecodeFunction() function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{{Name: "value", Type: cty.String}},
		Type: function.TypeFunc(func(_ []cty.Value) (cty.Type, error) {
			// Type is determined from the JSON content at call time.
			return cty.DynamicPseudoType, nil
		}),
		Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
			raw := []byte(args[0].AsString())
			ty, err := ctyjson.ImpliedType(raw)
			if err != nil {
				return cty.NilVal, fmt.Errorf("jsondecode(): %w", err)
			}
			v, err := ctyjson.Unmarshal(raw, ty)
			if err != nil {
				return cty.NilVal, fmt.Errorf("jsondecode(): %w", err)
			}
			return v, nil
		},
	})
}

func urlEncodeFunction() function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{{Name: "value", Type: cty.String}},
		Type:   function.StaticReturnType(cty.String),
		Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
			return cty.StringVal(url.QueryEscape(args[0].AsString())), nil
		},
	})
}

func yamlEncodeFunction() function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{{Name: "value", Type: cty.DynamicPseudoType, AllowNull: true}},
		Type:   function.StaticReturnType(cty.String),
		Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
			// Convert cty → Go via JSON round-trip for type-safe conversion,
			// then YAML-encode the resulting Go value.
			// ctyjson.Marshal, json.Unmarshal, and yaml.Marshal cannot fail for
			// the concrete, known cty values that the function spec admits.
			jsonBytes, _ := ctyjson.Marshal(args[0], args[0].Type())
			var goVal any
			_ = json.Unmarshal(jsonBytes, &goVal)
			yamlBytes, _ := yaml.Marshal(goVal)
			return cty.StringVal(string(yamlBytes)), nil
		},
	})
}

func yamlDecodeFunction() function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{{Name: "value", Type: cty.String}},
		Type: function.TypeFunc(func(_ []cty.Value) (cty.Type, error) {
			// Type is determined from the YAML content at call time.
			return cty.DynamicPseudoType, nil
		}),
		Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
			var goVal any
			if err := yaml.Unmarshal([]byte(args[0].AsString()), &goVal); err != nil {
				return cty.NilVal, fmt.Errorf("yamldecode(): %w", err)
			}
			// Convert Go value back to cty via JSON round-trip.
			// json.Marshal, ctyjson.ImpliedType, and ctyjson.Unmarshal cannot fail
			// for the standard Go types produced by yaml.Unmarshal.
			jsonBytes, _ := json.Marshal(goVal)
			ty, _ := ctyjson.ImpliedType(jsonBytes)
			v, _ := ctyjson.Unmarshal(jsonBytes, ty)
			return v, nil
		},
	})
}
