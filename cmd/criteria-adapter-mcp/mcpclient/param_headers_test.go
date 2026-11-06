package mcpclient

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestResolveToolParamHeaders(t *testing.T) {
	annotatedEcho := `{
		"type": "object",
		"properties": {
			"trace": {
				"type": "object",
				"additionalProperties": false,
				"required": ["X-Trace"],
				"x-mcp-header": {},
				"properties": {"X-Trace": {"type": "string"}}
			}
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
				"trace":{"type":"object","x-mcp-header":{},"additionalProperties":false,
				"properties":{"X-Trace-ID":{"type":"string"}}}}}}}`,
			wantBinds: []toolParamHeader{{path: []string{"meta", "trace"}, header: "X-Trace-ID"}},
			wantValid: true,
		},
		{
			name: "multiple headers on one annotation",
			schema: `{"type":"object","properties":{"h":{"type":"object","x-mcp-header":{},
				"properties":{"X-One":{"type":"string"},"X-Two":{"type":"integer"},
				"X-Three":{"type":"number"},"X-Four":{"type":"boolean"}}}}}`,
			wantCount: 4,
			wantValid: true,
		},
		{
			name: "annotation absent from additionalProperties subtree is foreign",
			schema: `{"type":"object","properties":{"trace":{"type":"object",
				"properties":{"X-Trace":{"type":"string"}},"additionalProperties":{"x-mcp-header":{}}}}}`,
			wantValid: false,
		},
		{
			name: "annotation behind oneOf is foreign",
			schema: `{"type":"object","properties":{"trace":{"type":"object","oneOf":[
				{"type":"object","x-mcp-header":{},"properties":{"X-T":{"type":"string"}}}]}}}`,
			wantValid: false,
		},
		{
			name:      "annotation at the root is not a property path",
			schema:    `{"type":"object","x-mcp-header":{},"properties":{"trace":{"type":"string"}}}`,
			wantValid: false,
		},
		{
			name: "header name not an HTTP token",
			schema: `{"type":"object","properties":{"trace":{"type":"object","x-mcp-header":{},
				"properties":{"Bad Header":{"type":"string"}}}}}`,
			wantValid: false,
		},
		{
			name: "value type not primitive",
			schema: `{"type":"object","properties":{"trace":{"type":"object","x-mcp-header":{},
				"properties":{"X-Deep":{"type":"array","items":{"type":"string"}}}}}}`,
			wantValid: false,
		},
		{
			name: "value schema without a type",
			schema: `{"type":"object","properties":{"trace":{"type":"object","x-mcp-header":{},"properties":
				{"X-Free":{}}}}}`,
			wantValid: false,
		},
		{
			name:      "annotated schema not object-typed",
			schema:    `{"type":"object","properties":{"trace":{"type":"string","x-mcp-header":{}}}}`,
			wantValid: false,
		},
		{
			name: "additionalProperties open leaves names non-static",
			schema: `{"type":"object","properties":{"trace":{"type":"object","x-mcp-header":{},
				"additionalProperties":true,"properties":{"X-Trace":{"type":"string"}}}}}`,
			wantValid: false,
		},
		{
			name: "additionalProperties as a schema is not static",
			schema: `{"type":"object","properties":{"trace":{"type":"object","x-mcp-header":{},
				"additionalProperties":{"type":"string"},"properties":{"X-Trace":{"type":"string"}}}}}`,
			wantValid: false,
		},
		{
			name: "header names colliding case-insensitively",
			schema: `{"type":"object","properties":{"h":{"type":"object","x-mcp-header":{},
				"properties":{"X-Trace":{"type":"string"},"x-trace":{"type":"string"}}}}}`,
			wantValid: false,
		},
		{
			name: "collisions across two annotated paths",
			schema: `{"type":"object","properties":{"a":{"type":"object","x-mcp-header":{},
				"properties":{"X-Trace":{"type":"string"}}},
				"b":{"type":"object","x-mcp-header":{},"properties":{"x-trace":{"type":"string"}}}}}`,
			wantValid: false,
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
			"trace": {
				"type": "object",
				"x-mcp-header": {},
				"additionalProperties": false,
				"properties": {"X-Trace": {"type": "string"}}
			},
			"flags": {
				"type": "object",
				"x-mcp-header": {},
				"properties": {"X-Flag": {"type": "boolean"}, "X-Count": {"type": "integer"},
					"X-Ratio": {"type": "number"}, "X-Missing": {"type": "string"}}
			}
		}
	}`))
	if !valid || len(bindings) != 5 {
		t.Fatalf("bindings = %v (valid=%v), want 5 (valid)", bindings, valid)
	}

	arguments := map[string]any{
		"trace": map[string]any{"X-Trace": "caf\u00e9"},
		"flags": map[string]any{"X-Flag": true, "X-Count": float64(7), "X-Ratio": float64(0.5)},
	}
	entries := mirrorToolParamHeaders(bindings, arguments)
	got := map[string]string{}
	for _, e := range entries {
		got[e.Name] = e.Value
	}
	want := map[string]string{
		// café: the é byte is outside the header-safe set, so the value
		// rides the revision's Base64 sentinel.
		"Mcp-Param-X-Trace": "=?base64?Y2Fmw6k=?=",
		"Mcp-Param-X-Flag":  "true",
		"Mcp-Param-X-Count": "7",
		"Mcp-Param-X-Ratio": "0.5",
		// Mcp-Param-X-Missing: absent from the arguments → not emitted; the
		// server's own required rule governs that.
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mirrored = %v, want %v", got, want)
	}

	// A null member and a non-object value at an annotated path emit
	// nothing (the argument shape violated the declared schema).
	absent := mirrorToolParamHeaders(bindings, map[string]any{
		"trace": nil,
		"flags": "not-an-object",
	})
	if len(absent) != 0 {
		t.Fatalf("violated-shape arguments mirrored %v, want none", absent)
	}
}
