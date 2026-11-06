package mcpclient

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// The x-mcp-header schema annotation: a tool inputSchema may mirror a
// parameter's own value into an Mcp-Param-{Name} header on the tools/call
// POST. The annotation is a JSON string named x-mcp-header inside the
// annotated property's schema, its value the header name: a `region`
// property whose schema is {"type":"string","x-mcp-header":"Region"}
// mirrors arguments.region as Mcp-Param-Region on the call, and the value
// itself stays in the call body.
const paramHeaderAnnotation = "x-mcp-header"

// maxMirroredInteger is the largest mirrored magnitude (2^53-1). JSON
// numbers decode as float64 and integers beyond ±(2^53-1) no longer
// round-trip exactly, so a mirrored header carrying one would be lossy; a
// call whose mirrored integer exceeds the bound is refused before the POST
// with a ParamBoundError naming the offending parameter path.
const maxMirroredInteger = int64(9007199254740991)

// jsonSchemaType is the JSON-Schema primitive set a mirrored parameter may
// be declared as.
type jsonSchemaType string

const (
	schemaTypeString  jsonSchemaType = "string"
	schemaTypeInteger jsonSchemaType = "integer"
	schemaTypeBoolean jsonSchemaType = "boolean"
)

// isPrimitiveSchemaType reports whether t is one of the permitted mirrored
// types. A number-typed parameter is not permitted: rendering arbitrary
// JSON numbers as header values is outside the mirror's defined shape.
func isPrimitiveSchemaType(t jsonSchemaType) bool {
	return t == schemaTypeString || t == schemaTypeInteger ||
		t == schemaTypeBoolean
}

// toolParamHeader is one resolved x-mcp-header binding for a tool: the
// plain properties path to the annotated parameter and the header name the
// annotation's string value declares. The parameter's own value mirrors as
// Mcp-Param-{header}.
type toolParamHeader struct {
	path   []string
	header string
}

// resolveToolParamHeaders resolves a tool's inputSchema into Mcp-Param-*
// bindings (nil, true when the schema carries no annotations).
//
// An annotation is honored only on a plain properties chain from the schema
// root: the annotated property declares a permitted mirrored type
// (string/boolean/integer — number is not mirror-permitted) and a header
// name that is a non-empty HTTP token, unique case-insensitively across
// all annotations in the schema. A tool is invalid (false) when an
// annotation sits outside that chain — at the schema root, under
// oneOf/anyOf/allOf, patternProperties/additionalProperties or "items", any
// other non-properties key, or deeper inside an annotated property's own
// value schema (the plain chain ends at the annotated property, so such an
// annotation has no statically reachable mirror path and the tool is
// rejected rather than partially honored) — when an annotation value is
// not a JSON string, when a name is not a valid HTTP token, when the
// annotated property's declared type is not permitted, or when two names
// collide case-insensitively.
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

	// An annotation in any non-properties root key is unreachable and
	// rejected; the plain chain below the properties table is walked next.
	for key, child := range root {
		if key == "properties" {
			continue
		}
		if carriesForeignAnnotation(child) {
			return nil, false
		}
	}
	var (
		bindings []toolParamHeader
		seen     = map[string]struct{}{}
	)
	if props, ok := root["properties"].(map[string]any); ok {
		if !walkProperties(nil, props, &bindings, seen) {
			return nil, false
		}
	}
	return bindings, true
}

// walkProperties resolves the annotation on each property of a plain
// properties table, per walkChain. A property with a non-map schema is not
// a schema node and carries no annotation key.
func walkProperties(path []string, props map[string]any, out *[]toolParamHeader, seen map[string]struct{}) bool {
	for name, prop := range props {
		node, ok := prop.(map[string]any)
		if !ok {
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
// on each chain node: an annotated node's string value declares the header
// name its own value mirrors into, so its subtree ends the chain;
// unannotated nodes continue it through their own properties. The bool
// result is tool validity.
func walkChain(path []string, node map[string]any, out *[]toolParamHeader, seen map[string]struct{}) bool {
	if _, has := node[paramHeaderAnnotation]; has {
		return walkAnnotatedNode(path, node, out, seen)
	}
	return walkUnannotatedNode(path, node, out, seen)
}

// walkAnnotatedNode records the annotated node's toolParamHeader binding:
// the annotation's JSON string is the header name the node's own value
// mirrors into, the node's declared type must be a permitted mirrored
// type, and the name must be unique case-insensitively across the schema's
// annotations. No annotation may hide inside the annotated node's value
// schema: its subtree ends the plain chain, so a deeper annotation is
// unreachable and invalidates the tool.
func walkAnnotatedNode(path []string, node map[string]any, out *[]toolParamHeader, seen map[string]struct{}) bool {
	header, ok := node[paramHeaderAnnotation].(string)
	if !ok {
		return false
	}
	if !isTokenHeaderName(header) {
		return false
	}
	typ, has := node["type"].(string)
	if !has || !isPrimitiveSchemaType(jsonSchemaType(typ)) {
		return false
	}
	key := lowerASCII(header)
	if _, dup := seen[key]; dup {
		return false
	}
	seen[key] = struct{}{}
	*out = append(*out, toolParamHeader{path: append([]string(nil), path...), header: header})
	for _, child := range node {
		if carriesForeignAnnotation(child) {
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

// carriesForeignAnnotation reports whether an x-mcp-header annotation
// exists anywhere inside node outside the plain properties chain: the
// annotation key inside any schema map. The chain walk resolves the
// reachable annotations, so every annotation this scan finds sits outside
// the chain and invalidates the tool.
func carriesForeignAnnotation(node any) bool {
	switch v := node.(type) {
	case map[string]any:
		if _, ok := v[paramHeaderAnnotation]; ok {
			return true
		}
		for _, child := range v {
			if carriesForeignAnnotation(child) {
				return true
			}
		}
	case []any:
		for _, child := range v {
			if carriesForeignAnnotation(child) {
				return true
			}
		}
	}
	return false
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

// ParamBoundError reports a mirrored integer argument outside the
// ±(2^53-1) mirrored bound (maxMirroredInteger): rendering such a value as
// a header would be lossy, so the mirror is not emitted and the call is
// refused before the POST. Path names the offending parameter; the value
// never appears in the message.
type ParamBoundError struct {
	Path []string
}

func (e *ParamBoundError) Error() string {
	return fmt.Sprintf("mcpclient: mirrored parameter path %s carries an integer argument outside the ±%d (2^53-1) mirrored bound; the mirror would be lossy so the call is refused",
		paramPathString(e.Path), maxMirroredInteger)
}

// paramPathString renders a binding path for error messages.
func paramPathString(path []string) string {
	return strings.Join(path, ".")
}

// headerEntry renders the mirrored HTTP header for one annotated
// parameter: the parameter's own value at its binding path, encoded with
// the standard header-value rules (the Base64 sentinel covers values
// outside the header-safe set). A string is carried verbatim; a boolean is
// "true"/"false"; a JSON number that is an integer renders exactly within
// the ±(2^53-1) bound — outside it the call is refused with a
// ParamBoundError, and a non-integer number is not a permitted mirrored
// value. A path absent from the arguments, null, or a non-primitive
// runtime value (the argument shape violated the declared schema) is not
// emitted — the server's own required-Mcp-Param-* rule governs that.
func (b toolParamHeader) headerEntry(arguments map[string]any) (HeaderEntry, bool, error) {
	var value any = arguments
	for _, key := range b.path {
		obj, ok := value.(map[string]any)
		if !ok {
			return HeaderEntry{}, false, nil
		}
		value, ok = obj[key]
		if !ok {
			return HeaderEntry{}, false, nil
		}
	}
	name := ParamHeaderPrefix + b.header
	switch v := value.(type) {
	case nil:
		return HeaderEntry{}, false, nil
	case string:
		return HeaderEntry{Name: name, Value: encodeHeaderValue(v)}, true, nil
	case bool:
		return HeaderEntry{Name: name, Value: strconv.FormatBool(v)}, true, nil
	case float64:
		if math.Trunc(v) != v {
			return HeaderEntry{}, false, nil
		}
		if v > float64(maxMirroredInteger) || v < -float64(maxMirroredInteger) {
			return HeaderEntry{}, false, &ParamBoundError{Path: append([]string(nil), b.path...)}
		}
		return HeaderEntry{Name: name, Value: strconv.FormatFloat(v, 'f', -1, 64)}, true, nil
	default:
		return HeaderEntry{}, false, nil
	}
}

// mirrorToolParamHeaders renders one Mcp-Param-{Name} header per annotated
// path present in the call arguments, in binding order. An out-of-bound
// mirrored integer stops the call before anything is sent.
func mirrorToolParamHeaders(bindings []toolParamHeader, arguments map[string]any) ([]HeaderEntry, error) {
	var entries []HeaderEntry
	for _, binding := range bindings {
		entry, ok, err := binding.headerEntry(arguments)
		if err != nil {
			return nil, err
		}
		if ok {
			entries = append(entries, entry)
		}
	}
	return entries, nil
}
