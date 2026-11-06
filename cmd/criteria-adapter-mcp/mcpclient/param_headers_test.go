package mcpclient

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestResolveToolParamHeaders(t *testing.T) {
	// The spec shape: the annotation is a JSON string named x-mcp-header
	// inside the annotated property's schema, its value the header name the
	// parameter's own value mirrors into.
	annotatedEcho := `{
		"type": "object",
		"properties": {
			"trace": {"type": "string", "x-mcp-header": "X-Trace"}
		}
	}`

	cases := []struct {
		name       string
		schema     string
		wantCount  int // when >0: only the binding count is pinned (map-ordered)
		wantBinds  []toolParamHeader
		wantValid  bool
		wantNoBind bool
	}{
		{
			name:      "annotated top-level property",
			schema:    annotatedEcho,
			wantBinds: []toolParamHeader{{path: []string{"trace"}, header: "X-Trace"}},
			wantValid: true,
		},
		{
			name: "annotated nested property",
			schema: `{"type":"object","properties":{"meta":{"type":"object","properties":{
				"region":{"type":"string","x-mcp-header":"X-Trace-ID"}}}}}`,
			wantBinds: []toolParamHeader{{path: []string{"meta", "region"}, header: "X-Trace-ID"}},
			wantValid: true,
		},
		{
			name: "several annotations on one schema",
			schema: `{"type":"object","properties":{
				"a":{"type":"string","x-mcp-header":"X-A"},
				"b":{"type":"boolean","x-mcp-header":"X-B"},
				"meta":{"type":"object","properties":{
					"c":{"type":"integer","x-mcp-header":"X-C"}}}}}`,
			wantCount: 3,
			wantValid: true,
		},
		{
			name:      "boolean annotation value is not a header name",
			schema:    `{"type":"object","properties":{"trace":{"type":"string","x-mcp-header":true}}}`,
			wantValid: false,
		},
		{
			name:      "object annotation value is not a header name",
			schema:    `{"type":"object","properties":{"trace":{"type":"string","x-mcp-header":{}}}}`,
			wantValid: false,
		},
		{
			name:      "empty annotation name",
			schema:    `{"type":"object","properties":{"trace":{"type":"string","x-mcp-header":""}}}`,
			wantValid: false,
		},
		{
			name:      "annotation name with a space is not an HTTP token",
			schema:    `{"type":"object","properties":{"trace":{"type":"string","x-mcp-header":"X Trace"}}}`,
			wantValid: false,
		},
		{
			name:      "annotation name carrying CR/LF",
			schema:    `{"type":"object","properties":{"trace":{"type":"string","x-mcp-header":"X-Trace\n"}}}`,
			wantValid: false,
		},
		{
			name:      "number-typed annotated property is not permitted",
			schema:    `{"type":"object","properties":{"trace":{"type":"number","x-mcp-header":"X-Trace"}}}`,
			wantValid: false,
		},
		{
			name:      "annotated property without a declared type",
			schema:    `{"type":"object","properties":{"trace":{"x-mcp-header":"X-Trace"}}}`,
			wantValid: false,
		},
		{
			name: "names colliding case-insensitively across two annotations",
			schema: `{"type":"object","properties":{
				"a":{"type":"string","x-mcp-header":"X-Trace"},
				"b":{"type":"string","x-mcp-header":"x-trace"}}}`,
			wantValid: false,
		},
		{
			name:      "annotation at the schema root is not a property path",
			schema:    `{"type":"object","x-mcp-header":"X-Trace","properties":{"trace":{"type":"string"}}}`,
			wantValid: false,
		},
		{
			name: "annotation behind oneOf is foreign",
			schema: `{"type":"object","properties":{"trace":{"type":"object","oneOf":[
				{"type":"string","x-mcp-header":"X-T"}]}}}`,
			wantValid: false,
		},
		{
			name: "annotation behind allOf is foreign",
			schema: `{"type":"object","properties":{"trace":{"type":"object","allOf":[
				{"type":"string","x-mcp-header":"X-T"}]}}}`,
			wantValid: false,
		},
		{
			name: "annotation behind patternProperties is foreign",
			schema: `{"type":"object","properties":{"trace":{"type":"object","patternProperties":{
				"^x$":{"type":"string","x-mcp-header":"X-T"}}}}}`,
			wantValid: false,
		},
		{
			name: "annotation behind items is foreign",
			schema: `{"type":"object","properties":{"trace":{"type":"object","items":{
				"type":"string","x-mcp-header":"X-T"}}}}`,
			wantValid: false,
		},
		{
			name: "annotation behind additionalProperties is foreign",
			schema: `{"type":"object","properties":{"trace":{"type":"object","additionalProperties":{
				"type":"string","x-mcp-header":"X-T"}}}}`,
			wantValid: false,
		},
		{
			name:       "annotation in a non-properties root key with no properties table",
			schema:     `{"type":"object","oneOf":[{"type":"object","properties":{"a":{"type":"string","x-mcp-header":"X-A"}}}]}`,
			wantValid:  false,
			wantNoBind: true,
		},
		{
			name: "annotation hiding inside the annotated property's own value schema",
			schema: `{"type":"object","properties":{"region":{"type":"object","x-mcp-header":"X-Region",
				"properties":{"deep":{"type":"string","x-mcp-header":"X-Deep"}}}}}`,
			wantValid: false,
		},
		{
			name: "a property named x-mcp-header is an ordinary property",
			schema: `{"type":"object","properties":{
				"x-mcp-header":{"type":"string"}}}`,
			wantValid:  true,
			wantNoBind: true,
		},
		{
			name:       "unannotated schema has no bindings",
			schema:     `{"type":"object","properties":{"trace":{"type":"string"}},"required":["trace"]}`,
			wantValid:  true,
			wantNoBind: true,
		},
		{
			name:       "empty schema",
			schema:     "",
			wantValid:  true,
			wantNoBind: true,
		},
		{
			name:       "unparseable schema",
			schema:     "{not json",
			wantValid:  true,
			wantNoBind: true, // a non-object schema carries no resolvable annotations
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bindings, valid := resolveToolParamHeaders(json.RawMessage(tc.schema))
			if valid != tc.wantValid {
				t.Fatalf("valid = %v, want %v", valid, tc.wantValid)
			}
			if !valid || tc.wantNoBind {
				if len(bindings) != 0 {
					t.Fatalf("bindings = %v, want none", bindings)
				}
				return
			}
			if tc.wantCount > 0 {
				if len(bindings) != tc.wantCount {
					t.Fatalf("bindings = %v, want %d entries", bindings, tc.wantCount)
				}
				return
			}
			if !reflect.DeepEqual(bindings, tc.wantBinds) {
				t.Fatalf("bindings = %v, want %v", bindings, tc.wantBinds)
			}
		})
	}
}

func TestMirrorToolParamHeaders(t *testing.T) {
	bindings, valid := resolveToolParamHeaders(json.RawMessage(`{
		"type": "object",
		"properties": {
			"region": {"type": "string", "x-mcp-header": "X-Region"},
			"flag": {"type": "boolean", "x-mcp-header": "X-Flag"},
			"count": {"type": "integer", "x-mcp-header": "X-Count"},
			"meta": {"type": "object", "properties": {
				"region": {"type": "string", "x-mcp-header": "X-Meta-Region"}}
			}
		}
	}`))
	if !valid || len(bindings) != 4 {
		t.Fatalf("bindings = %v (valid=%v), want 4 (valid)", bindings, valid)
	}

	arguments := map[string]any{
		"region": "caf\u00e9",
		"flag":   true,
		"count":  float64(7),
		"meta":   map[string]any{"region": "abc"},
	}
	entries, err := mirrorToolParamHeaders(bindings, arguments)
	if err != nil {
		t.Fatalf("mirror err = %v, want nil", err)
	}
	got := map[string]string{}
	for _, e := range entries {
		got[e.Name] = e.Value
	}
	want := map[string]string{
		// café: the é byte is outside the header-safe set, so the value
		// rides the revision's Base64 sentinel.
		"Mcp-Param-X-Region":      "=?base64?Y2Fmw6k=?=",
		"Mcp-Param-X-Flag":        "true",
		"Mcp-Param-X-Count":       "7",
		"Mcp-Param-X-Meta-Region": "abc",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mirrored = %v, want %v", got, want)
	}

	// A value absent from the arguments, null, a non-primitive runtime
	// value, a non-object at a nested path, or a non-integer number (the
	// argument shape violated the declared schema) emits nothing.
	absent, err := mirrorToolParamHeaders(bindings, map[string]any{
		"region": nil,
		"flag":   map[string]any{"nested": true},
		"count":  float64(0.5),
		"meta":   "not-an-object",
	})
	if err != nil || len(absent) != 0 {
		t.Fatalf("violated-shape mirror = %v err=%v, want none/nil", absent, err)
	}

	// An integer exactly at the ±(2^53-1) bound renders exactly.
	edge, err := mirrorToolParamHeaders(bindings, map[string]any{"count": float64(9007199254740991)})
	if err != nil || len(edge) != 1 || edge[0].Value != "9007199254740991" {
		t.Fatalf("edge integer mirror = %v err=%v, want exactly 9007199254740991", edge, err)
	}
}

func TestMirrorToolParamHeaders_BoundRefusal(t *testing.T) {
	bindings, valid := resolveToolParamHeaders(json.RawMessage(`{
		"type": "object",
		"properties": {"count": {"type": "integer", "x-mcp-header": "X-Count"}}
	}`))
	if !valid || len(bindings) != 1 {
		t.Fatalf("bindings = %v (valid=%v), want 1 (valid)", bindings, valid)
	}

	// 2^53 is the first integer float64 cannot render exactly: the call is
	// refused with a ParamBoundError naming the parameter path, never the
	// value, and nothing is mirrored.
	for _, offender := range []float64{9007199254740993, -9007199254740992} {
		entries, err := mirrorToolParamHeaders(bindings, map[string]any{"count": offender})
		if len(entries) != 0 {
			t.Fatalf("mirror = %v, want none", entries)
		}
		var bound *ParamBoundError
		if !errors.As(err, &bound) {
			t.Fatalf("err = %v (%T), want *ParamBoundError", err, err)
		}
		if !reflect.DeepEqual(bound.Path, []string{"count"}) {
			t.Fatalf("ParamBoundError.Path = %v, want [count]", bound.Path)
		}
		if msg := bound.Error(); !strings.Contains(msg, "count") ||
			strings.Contains(msg, "9007199254740992") ||
			strings.Contains(msg, "9007199254740993") {
			t.Fatalf("error message %q must name the path and never the value", msg)
		}
	}
}
