package workflow

// jsonschema.go — deterministic cty-type → JSON Schema converter for outcome
// payload contracts (KB-45). The output is the pinned conformance subset the
// adapter-SDK host validator understands (see the criteria-adapter-proto
// v0.7.0 conformance README): root type "object", properties with leaf types
// string|number|boolean|object|array, a required-name list, optional() default
// values surfaced as "default", and collections folded to array/items.
//
// Named `type.<name>` resolutions and inline constraints produce the same
// cty.Type here, so byte equality between the two authoring forms is a
// property of cty type construction rather than of this converter.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/hashicorp/hcl/v2/ext/typeexpr"
	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"
)

// jsonSchemaNode is the canonical emission shape. Field order fixes key order
// in the output; map-valued properties are marshaled with sorted keys by
// encoding/json, so identical types always produce identical bytes.
type jsonSchemaNode struct {
	Type       string                     `json:"type,omitempty"`
	Properties map[string]*jsonSchemaNode `json:"properties,omitempty"`
	Required   []string                   `json:"required,omitempty"`
	Items      *jsonSchemaNode            `json:"items,omitempty"`
	Default    any                        `json:"default,omitempty"`
}

// CTypeToJSONSchema converts a parsed outcome-schema type constraint into
// compact deterministic JSON Schema bytes. defs carries the constraint's
// optional() defaults (may be nil). Nested positions receive the matching
// child defaults via typeexpr's Children map ("" for collection elements).
func CTypeToJSONSchema(typ cty.Type, defs *typeexpr.Defaults) ([]byte, error) {
	node, err := ctyToSchemaNode(typ, defs)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(node); err != nil {
		return nil, fmt.Errorf("encoding outcome schema JSON: %w", err)
	}
	// json.Encoder appends a newline; strip it for stable on-wire bytes.
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// ctyToSchemaNode maps one cty type node to its Schema subtree. defs is the
// node's own defaults: DefaultValues indexes this node's attributes, and
// Children[""] addresses a collection element.
func ctyToSchemaNode(typ cty.Type, defs *typeexpr.Defaults) (*jsonSchemaNode, error) {
	if typ == cty.NilType {
		return nil, fmt.Errorf("nil type cannot be converted to a schema")
	}

	switch {
	case typ == cty.DynamicPseudoType:
		// An unconstrained payload: empty schema, permissive on the wire.
		return &jsonSchemaNode{}, nil

	case typ == cty.String, typ == cty.Number, typ == cty.Bool:
		name := "string"
		if typ == cty.Number {
			name = "number"
		} else if typ == cty.Bool {
			name = "boolean"
		}
		return &jsonSchemaNode{Type: name}, nil

	case typ.IsMapType():
		// Map payloads only constrain their surface shape on the wire: the
		// pinned validator walks properties/required of objects, so a keyed
		// map is simply {"type":"object"}.
		return &jsonSchemaNode{Type: "object"}, nil

	case typ.IsListType(), typ.IsSetType(), typ.IsTupleType():
		n := &jsonSchemaNode{Type: "array"}
		if !typ.IsTupleType() {
			// A homogeneous collection constrains its element; a tuple's
			// heterogeneous elements cannot be expressed in the pinned
			// subset, so they are left unconstrained.
			elem, err := ctyToSchemaNode(typ.ElementType(), childDefaults(defs, ""))
			if err != nil {
				return nil, err
			}
			n.Items = elem
		}
		return n, nil

	case typ.IsObjectType():
		return objectSchemaNode(typ, defs)

	default:
		return nil, fmt.Errorf("unsupported outcome schema type %s", typ.FriendlyName())
	}
}

// objectSchemaNode folds an object type into the pinned object shape:
// properties sorted, non-optional attributes listed in required (sorted),
// optional() defaults surfaced per attribute.
func objectSchemaNode(typ cty.Type, defs *typeexpr.Defaults) (*jsonSchemaNode, error) {
	attrs := typ.AttributeTypes()
	if len(attrs) == 0 {
		return &jsonSchemaNode{Type: "object"}, nil
	}

	names := make([]string, 0, len(attrs))
	for name := range attrs {
		names = append(names, name)
	}
	sort.Strings(names)

	props := make(map[string]*jsonSchemaNode, len(names))
	var required []string
	for _, name := range names {
		child, err := ctyToSchemaNode(attrs[name], childDefaults(defs, name))
		if err != nil {
			return nil, err
		}
		if d, ok := nodeDefault(defs, name); ok {
			child.Default = d
		}
		props[name] = child
		if !typ.AttributeOptional(name) {
			required = append(required, name)
		}
	}

	return &jsonSchemaNode{Type: "object", Properties: props, Required: required}, nil
}

// nodeDefault renders one attribute default for the wire when declared.
func nodeDefault(defs *typeexpr.Defaults, name string) (any, bool) {
	if defs == nil {
		return nil, false
	}
	v, ok := defs.DefaultValues[name]
	if !ok {
		return nil, false
	}
	raw := ctyJSONRaw(v)
	if raw == nil {
		return nil, false
	}
	return raw, true
}

// childDefaults returns the nested Defaults for one position (attribute name,
// or "" for a collection element), or nil.
func childDefaults(defs *typeexpr.Defaults, key string) *typeexpr.Defaults {
	if defs == nil {
		return nil
	}
	return defs.Children[key]
}

// ctyJSONRaw marshals a cty value with cty's own JSON codec so defaults keep
// their rendered form (numbers, strings, booleans, and structures serialize
// naturally). Returns nil for unknown values.
func ctyJSONRaw(v cty.Value) any {
	if !v.IsKnown() {
		return nil
	}
	raw, err := json.Marshal(ctyjson.SimpleJSONValue{Value: v})
	if err != nil {
		return nil
	}
	return json.RawMessage(raw)
}
