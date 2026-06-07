// Package secrets implements the provider stack, redaction registry, and
// secret-channel wiring for the Criteria host.
package secrets

import (
	"encoding/json"
	"fmt"
	"strings"
)

// OriginRef describes where a secret value should be resolved from.
// It is distinct from the WS09 state.OriginRef (taint tracking) — this type
// is provider-oriented and lives in the secrets package.
type OriginRef struct {
	Kind string `json:"kind"` // "env" | "file" | "keychain" | "vault" | "sops" | "literal"
	Ref  string `json:"ref"`  // provider-specific reference (e.g. "ANTHROPIC_API_KEY", "/run/secrets/key")
}

// String returns a compact "kind:ref" representation.
func (o OriginRef) String() string {
	return o.Kind + ":" + o.Ref
}

// ParseOriginRef parses a compact "kind:ref" string into an OriginRef.
// If the input does not contain a colon, it is treated as a literal value
// (Kind="literal", Ref=input).
func ParseOriginRef(s string) OriginRef {
	if idx := strings.Index(s, ":"); idx > 0 {
		return OriginRef{Kind: s[:idx], Ref: s[idx+1:]}
	}
	return OriginRef{Kind: "literal", Ref: s}
}

// MarshalJSON serializes as the compact string form.
func (o OriginRef) MarshalJSON() ([]byte, error) {
	return json.Marshal(o.String())
}

// UnmarshalJSON accepts either the compact string form or a JSON object.
func (o *OriginRef) UnmarshalJSON(data []byte) error {
	// Try compact string first.
	var compact string
	if err := json.Unmarshal(data, &compact); err == nil {
		*o = ParseOriginRef(compact)
		return nil
	}
	// Fall back to object form.
	type originRefJSON OriginRef
	var raw originRefJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("unmarshal OriginRef: %w", err)
	}
	*o = OriginRef(raw)
	return nil
}

// MarshalText implements encoding.TextMarshaler for compact serialization.
func (o OriginRef) MarshalText() ([]byte, error) {
	return []byte(o.String()), nil
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (o *OriginRef) UnmarshalText(data []byte) error {
	*o = ParseOriginRef(string(data))
	return nil
}
