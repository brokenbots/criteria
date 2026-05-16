// Package audit provides helpers for constructing adapter audit trails.
//
// CanonicalJSON implements a subset of RFC 8785 (JCS — JSON Canonicalization
// Scheme): deterministic serialisation with lexicographically-sorted object
// keys and no whitespace.  The resulting bytes are used to produce the
// args_digest field in PermissionRequest (sha256(canonical_json(args))), so
// that audit logs can be compared across serialisation order differences.
package audit

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

// CanonicalJSON returns the RFC 8785-style canonical JSON encoding of v.
//
// v must be JSON-round-trippable: maps, slices, numbers, strings, booleans,
// and nil are all accepted.  Object keys are sorted lexicographically at every
// nesting level.  The output has no trailing newline and no whitespace between
// tokens.
//
// This is intentionally a subset of RFC 8785: it relies on encoding/json for
// number serialisation and does not perform the ES6-style float normalisation
// required by the full spec.  For PermissionRequest.args_digest the only
// requirement is determinism given the same logical value — the subset is
// sufficient for that use case.
func CanonicalJSON(v any) ([]byte, error) {
	// Round-trip through encoding/json to normalise Go-native types (e.g. int,
	// float64, struct) into a generic map/slice/scalar tree that we can
	// canonicalise recursively.
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("canonical json: marshal: %w", err)
	}

	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return nil, fmt.Errorf("canonical json: unmarshal: %w", err)
	}

	var buf bytes.Buffer
	if err := encodeCanonical(&buf, generic); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ArgsDigest returns the lowercase hex-encoded SHA-256 digest of the
// canonical JSON representation of v.  This is the value that must be stored
// in PermissionRequest.args_digest.
func ArgsDigest(v any) (string, error) {
	b, err := CanonicalJSON(v)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

// encodeCanonical recursively writes the canonical JSON of node into buf.
func encodeCanonical(buf *bytes.Buffer, node any) error {
	if node == nil {
		buf.WriteString("null")
		return nil
	}
	switch v := node.(type) {
	case bool:
		encodeBool(buf, v)
	case float64:
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Errorf("canonical json: encode float64: %w", err)
		}
		buf.Write(b)
	case string:
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Errorf("canonical json: encode string: %w", err)
		}
		buf.Write(b)
	case []any:
		return encodeArray(buf, v)
	case map[string]any:
		return encodeObject(buf, v)
	default:
		return fmt.Errorf("canonical json: unsupported type %T", node)
	}
	return nil
}

// encodeBool writes "true" or "false" into buf.
func encodeBool(buf *bytes.Buffer, v bool) {
	if v {
		buf.WriteString("true")
	} else {
		buf.WriteString("false")
	}
}

// encodeArray writes the canonical JSON of a JSON array into buf.
func encodeArray(buf *bytes.Buffer, v []any) error {
	buf.WriteByte('[')
	for i, elem := range v {
		if i > 0 {
			buf.WriteByte(',')
		}
		if err := encodeCanonical(buf, elem); err != nil {
			return err
		}
	}
	buf.WriteByte(']')
	return nil
}

// encodeObject writes the canonical JSON of a JSON object into buf, sorting
// keys lexicographically so the output is deterministic.
func encodeObject(buf *bytes.Buffer, v map[string]any) error {
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	buf.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		keyBytes, err := json.Marshal(k)
		if err != nil {
			return fmt.Errorf("canonical json: encode key: %w", err)
		}
		buf.Write(keyBytes)
		buf.WriteByte(':')
		if err := encodeCanonical(buf, v[k]); err != nil {
			return err
		}
	}
	buf.WriteByte('}')
	return nil
}
