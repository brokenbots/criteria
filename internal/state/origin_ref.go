package state

import (
	"encoding/json"
	"fmt"
)

// OriginRef tracks the provenance of a secret value in the Criteria state
// machine. It is used by the taint engine to record where a secret came from
// so that diagnostics and audit logs can provide precise source information.
type OriginRef struct {
	// Kind is the category of origin: "variable", "shared_variable",
	// "adapter_secret", "step_secret_input", or "environment_secret".
	Kind string `json:"kind"`
	// Name is the specific identifier within the kind (e.g. the variable name,
	// adapter key, or step name).
	Name string `json:"name"`
}

// MarshalJSON serialises an OriginRef as a compact "kind:name" string so
// that values remain human-readable in JSON output.
func (o OriginRef) MarshalJSON() ([]byte, error) {
	return json.Marshal(fmt.Sprintf("%s:%s", o.Kind, o.Name))
}

// UnmarshalJSON restores an OriginRef from its compact "kind:name" JSON form.
func (o *OriginRef) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	return o.unmarshalFromString(s)
}

// MarshalText implements encoding.TextMarshaler for OriginRef.
func (o OriginRef) MarshalText() ([]byte, error) {
	return []byte(fmt.Sprintf("%s:%s", o.Kind, o.Name)), nil
}

// UnmarshalText implements encoding.TextUnmarshaler for OriginRef.
func (o *OriginRef) UnmarshalText(text []byte) error {
	return o.unmarshalFromString(string(text))
}

func (o *OriginRef) unmarshalFromString(s string) error {
	// Find the first colon to split kind and name.
	idx := -1
	for i := 0; i < len(s); i++ {
		if s[i] == ':' {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("origin_ref: expected \"kind:name\" form, got %q", s)
	}
	o.Kind = s[:idx]
	o.Name = s[idx+1:]
	return nil
}
