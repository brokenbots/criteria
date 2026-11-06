package mcpclient

import (
	"encoding/json"
	"strconv"
)

// The x-mcp-header schema annotation: a tool inputSchema may mirror one
// property's value members into Mcp-Param-{Name} headers on the tools/call
// POST. Two spellings declare it
//   - marker: a property carries the marker key beside its value schema;
//     the annotated schema's own properties are the header names;
//   - property name: a property named x-mcp-header is itself the
//     annotation; its schema is the header-name table.
//
// In both spellings the table's properties are the header names, each typed
// primitive, with statically known names (additionalProperties false or
// absent). The transport emits one Mcp-Param-{Name} header per entry,
// mirroring the value at the annotated property path; the annotated value
// itself stays in the call body.
const paramHeaderAnnotation = "x-mcp-header"

// jsonSchemaType is the JSON-Schema primitive set a mirrored header value
// may be declared as.
type jsonSchemaType string

const (
	schemaTypeString  jsonSchemaType = "string"
	schemaTypeNumber  jsonSchemaType = "number"
	schemaTypeInteger jsonSchemaType = "integer"
	schemaTypeBoolean jsonSchemaType = "boolean"
)

// isPrimitiveSchemaType reports whether t is one of the four primitive
// JSON-Schema types a mirrored header value may be declared as.
func isPrimitiveSchemaType(t jsonSchemaType) bool {
	return t == schemaTypeString || t == schemaTypeNumber ||
		t == schemaTypeInteger || t == schemaTypeBoolean
}

// toolParamHeader is one resolved x-mcp-header binding for a tool: the
// statically reachable property path the annotation sits on and the header
// name it mirrors the value's member into.
type toolParamHeader struct {
	path   []string
	header string
}

// resolveToolParamHeaders resolves a tool's inputSchema into Mcp-Param-*
// bindings (nil, true when the schema carries no annotations).
//
// An annotation of either spelling is honored only on a plain properties
// chain from the schema root; the annotated table must be an object whose
// properties are the header names, each typed primitive, with statically
// known names (additionalProperties false or absent). A tool is invalid
// (false) when an annotation sits outside that chain (inside
// oneOf/anyOf/allOf, patternProperties/additionalProperties, "items", or
// at the schema root — the plain chain is the only walkable path, so an
// annotation the mirror cannot resolve statically has no mirrorable path
// and the tool is rejected rather than partially honored), when a header
// name is not a valid HTTP token, when a value schema's type is not
// primitive, or when two names collide case-insensitively.
func resolveToolParamHeaders(schema json.RawMessage) ([]toolParamHeader, bool) {
	if len(schema) == 0 {
		return nil, true
	}
	var root map[string]any
	if err := json.Unmarshal(schema, &root); err != nil {
		return nil, true
	}
	if root == nil {
		return nil, true
	}
	if _, has := root[paramHeaderAnnotation]; has {
		// The root is not a property path: an annotation there has no
		// mirrorable value.
		return nil, false
	}

	props, ok := root["properties"].(map[string]any)
	if !ok {
		// No plain chain to walk; an annotation in any other root key is
		// still unreachable and still rejected.
		for key, child := range root {
			if key == "properties" {
				continue
			}
			if carriesForeignAnnotation(child) {
				return nil, false
			}
		}
		return nil, true
	}

	var (
		bindings []toolParamHeader
		seen     = map[string]struct{}{}
	)
	if !walkProperties(nil, props, &bindings, seen) {
		return nil, false
	}
	for key, child := range root {
		if key == "properties" {
			continue
		}
		if carriesForeignAnnotation(child) {
			return nil, false
		}
	}
	return bindings, true
}

// walkProperties resolves the annotation on each property of a plain
// properties table, per walkChain. A property with a non-map schema is not
// a resolvable node; under the annotation's own name (the property-name
// spelling gone malformed) that makes the tool invalid, elsewhere it is
// simply not a schema node.
func walkProperties(path []string, props map[string]any, out *[]toolParamHeader, seen map[string]struct{}) bool {
	for name, prop := range props {
		node, ok := prop.(map[string]any)
		if !ok {
			if name == paramHeaderAnnotation {
				return false
			}
			continue
		}
		child := make([]string, 0, len(path)+1)
		child = append(child, path...)
		child = append(child, name)
		if !walkChain(child, node, out, seen) {
			return false
		}
	}
	return true
}

// walkChain descends one plain properties chain, resolving the annotation
// on each chain node in either spelling: a node carrying the marker key,
// or a node whose property name is the annotation (its schema IS the
// header-name table). An annotated node's table fully describes the
// mirrored value, so its subtree ends the chain; unannotated nodes
// continue it through their own properties. The bool result is tool
// validity.
func walkChain(path []string, node map[string]any, out *[]toolParamHeader, seen map[string]struct{}) bool {
	if _, has := node[paramHeaderAnnotation]; has {
		return walkAnnotatedNode(path, node, out, seen)
	}
	if len(path) > 0 && path[len(path)-1] == paramHeaderAnnotation {
		return walkAnnotatedNode(path, node, out, seen)
	}
	return walkUnannotatedNode(path, node, out, seen)
}

// walkAnnotatedNode records the annotated node's toolParamHeader bindings
// and validates that no hidden annotation survives inside the annotated
// node's value schemas. Its subtree ends the plain chain, so only the
// header-name children are scanned.
func walkAnnotatedNode(path []string, node map[string]any, out *[]toolParamHeader, seen map[string]struct{}) bool {
	names, ok := annotatedHeaderNames(node)
	if !ok {
		return false
	}
	props, _ := node["properties"].(map[string]any)
	for _, header := range names {
		if !isTokenHeaderName(header) {
			return false
		}
		key := lowerASCII(header)
		if _, dup := seen[key]; dup {
			return false
		}
		seen[key] = struct{}{}
		*out = append(*out, toolParamHeader{path: path, header: header})
		// A foreign annotation may not hide inside a header's value
		// schema: the annotated node ends the plain chain, so an annotation
		// deeper inside any member is unreachable.
		if probe, _ := props[header].(map[string]any); probe != nil && carriesForeignAnnotation(probe) {
			return false
		}
	}
	return true
}

// walkUnannotatedNode validates and continues the plain chain below an
// unannotated node: any annotation reachable from its non-properties keys
// (oneOf/anyOf/allOf, pattern/additional subtrees, "items", and the rest)
// is outside the plain chain and invalidates the tool; annotations inside
// the plain properties chain are resolved by the recursing walk.
func walkUnannotatedNode(path []string, node map[string]any, out *[]toolParamHeader, seen map[string]struct{}) bool {
	for key, child := range node {
		if key == "properties" {
			continue
		}
		if carriesForeignAnnotation(child) {
			return false
		}
	}
	props, ok := node["properties"].(map[string]any)
	if !ok {
		return true
	}
	return walkProperties(path, props, out, seen)
}

// carriesForeignAnnotation reports whether an x-mcp-header annotation of
// either spelling exists anywhere inside node outside the plain properties
// chain: the marker key inside any schema map, or a property whose name is
// the annotation inside any properties table. The chain walk resolves the
// reachable annotations, so every annotation this scan finds sits outside
// the chain and invalidates the tool.
func carriesForeignAnnotation(node any) bool {
	switch v := node.(type) {
	case map[string]any:
		if _, ok := v[paramHeaderAnnotation]; ok {
			return true
		}
		if props, ok := v["properties"].(map[string]any); ok {
			if _, ok := props[paramHeaderAnnotation]; ok {
				return true
			}
		}
		for _, child := range v {
			if carriesForeignAnnotation(child) {
				return true
			}
		}
		return false
	case []any:
		for _, child := range v {
			if carriesForeignAnnotation(child) {
				return true
			}
		}
		return false
	default:
		return false
	}
}

// isTokenHeaderName reports whether a mirrored header name is a valid HTTP
// token (RFC 9110 tchar, the same set NewHeaderEntry accepts).
func isTokenHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		if !isTokenRune(r) {
			return false
		}
	}
	return true
}

// annotatedHeaderNames validates an x-mcp-header-annotated schema node
// (either spelling) and reports whether the annotated table is a usable
// mirror table: an object whose properties are the header names, each
// typed primitive, with statically known names (additionalProperties false
// or absent; present and not — true, or a schema — leaves the value's
// names open and rejects the tool). A table whose shape or member schemas
// do not conform yields no names and invalidates the tool.
func annotatedHeaderNames(node map[string]any) ([]string, bool) {
	typ, ok := node["type"].(string)
	if !ok || typ != "object" {
		return nil, false
	}
	switch addl := node["additionalProperties"].(type) {
	case nil:
	case bool:
		if addl {
			return nil, false
		}
	default:
		return nil, false
	}
	props, ok := node["properties"].(map[string]any)
	if !ok || len(props) == 0 {
		return nil, false
	}
	names := make([]string, 0, len(props))
	for header, valueSchema := range props {
		schemaObj, ok := valueSchema.(map[string]any)
		if !ok {
			return nil, false
		}
		typRaw, has := schemaObj["type"]
		if !has {
			return nil, false
		}
		typName, isString := typRaw.(string)
		if !isString || !isPrimitiveSchemaType(jsonSchemaType(typName)) {
			return nil, false
		}
		names = append(names, header)
	}
	return names, true
}

// lowerASCII lowercases only ASCII letters: header names are US-ASCII
// tokens, so case-insensitive collision detection needs no full Unicode
// casing.
func lowerASCII(s string) string {
	out := []byte(s)
	for i := 0; i < len(out); i++ {
		if out[i] >= 'A' && out[i] <= 'Z' {
			out[i] += 'a' - 'A'
		}
	}
	return string(out)
}

// ParamHeaderPrefix is the transport-controlled header namespace
// protocol-mandated mirrors ride.
// The fixture's Server Validation shares the namespace so both ends agree.
const ParamHeaderPrefix = "Mcp-Param-"

// headerEntry renders one mirrored HTTP header: the member of the value at
// the annotated path whose key is the header name, encoded with the
// standard header-value rules (the Base64 sentinel covers values outside
// the header-safe set). A string is carried verbatim; a boolean is
// "true"/"false"; a number is shortest round-trip formatted. A member
// missing from the value, null, or non-primitive (the argument shape
// violated the declared schema) is not emitted — the server's own
// required-Mcp-Param-* rule governs that.
func (b toolParamHeader) headerEntry(arguments map[string]any) (HeaderEntry, bool) {
	var value any
	current := any(arguments)
	for _, key := range b.path {
		obj, ok := current.(map[string]any)
		if !ok {
			return HeaderEntry{}, false
		}
		value, ok = obj[key]
		if !ok {
			return HeaderEntry{}, false
		}
		current = value
	}
	members, ok := value.(map[string]any)
	if !ok {
		return HeaderEntry{}, false
	}
	raw, ok := members[b.header]
	if !ok {
		return HeaderEntry{}, false
	}
	name := ParamHeaderPrefix + b.header
	switch v := raw.(type) {
	case nil:
		return HeaderEntry{}, false
	case string:
		return HeaderEntry{Name: name, Value: encodeHeaderValue(v)}, true
	case bool:
		return HeaderEntry{Name: name, Value: strconv.FormatBool(v)}, true
	case float64:
		return HeaderEntry{Name: name, Value: strconv.FormatFloat(v, 'g', -1, 64)}, true
	default:
		return HeaderEntry{}, false
	}
}

// mirrorToolParamHeaders renders one Mcp-Param-{Name} header per annotated
// path present in the call arguments, in binding order.
func mirrorToolParamHeaders(bindings []toolParamHeader, arguments map[string]any) []HeaderEntry {
	var entries []HeaderEntry
	for _, binding := range bindings {
		if entry, ok := binding.headerEntry(arguments); ok {
			entries = append(entries, entry)
		}
	}
	return entries
}
