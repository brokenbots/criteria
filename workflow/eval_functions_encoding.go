package workflow

// eval_functions_encoding.go — base64, JSON, URL, and YAML HCL functions.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

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
			data, err := ctyjson.Marshal(args[0], args[0].Type())
			if err != nil {
				return cty.StringVal(""), fmt.Errorf("jsonencode(): %w", err)
			}
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
			jsonBytes, err := ctyjson.Marshal(args[0], args[0].Type())
			if err != nil {
				return cty.StringVal(""), fmt.Errorf("yamlencode(): cty→json: %w", err)
			}
			var goVal any
			if err := json.Unmarshal(jsonBytes, &goVal); err != nil {
				return cty.StringVal(""), fmt.Errorf("yamlencode(): json→go: %w", err)
			}
			yamlBytes, err := yaml.Marshal(goVal)
			if err != nil {
				return cty.StringVal(""), fmt.Errorf("yamlencode(): %w", err)
			}
			// Strip the trailing newline that yaml.Marshal appends to match
			// Terraform's yamlencode behaviour.
			return cty.StringVal(strings.TrimRight(string(yamlBytes), "\n")), nil
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
			jsonBytes, err := json.Marshal(goVal)
			if err != nil {
				return cty.NilVal, fmt.Errorf("yamldecode(): go→json: %w", err)
			}
			ty, err := ctyjson.ImpliedType(jsonBytes)
			if err != nil {
				return cty.NilVal, fmt.Errorf("yamldecode(): impliedtype: %w", err)
			}
			v, err := ctyjson.Unmarshal(jsonBytes, ty)
			if err != nil {
				return cty.NilVal, fmt.Errorf("yamldecode(): json→cty: %w", err)
			}
			return v, nil
		},
	})
}
