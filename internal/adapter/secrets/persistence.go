package secrets

import (
	"context"
	"encoding/json"
)

// Snapshot is a serializable record of how a set of secrets was resolved.
// It stores only the OriginRef for each secret name (never the raw value),
// so that resume can re-run resolution and re-populate the redaction registry.
//
// This is intentionally separate from the proto Snapshot message (WS18);
// the host can embed this JSON blob in its checkpoint.
type Snapshot struct {
	Secrets map[string]OriginRef `json:"secrets,omitempty"`
}

// MarshalSnapshot serialises a Snapshot to JSON.
func MarshalSnapshot(s Snapshot) ([]byte, error) {
	return json.Marshal(s)
}

// UnmarshalSnapshot deserialises JSON into a Snapshot.
func UnmarshalSnapshot(data []byte) (Snapshot, error) {
	var s Snapshot
	if err := json.Unmarshal(data, &s); err != nil {
		return Snapshot{}, err
	}
	if s.Secrets == nil {
		s.Secrets = make(map[string]OriginRef)
	}
	return s, nil
}

// BuildSnapshot creates a Snapshot from an adapter-level secret map.
// The map keys are secret names and the values are the OriginRefs that
// produced them.
func BuildSnapshot(refs map[string]OriginRef) Snapshot {
	if refs == nil {
		return Snapshot{Secrets: map[string]OriginRef{}}
	}
	cp := make(map[string]OriginRef, len(refs))
	for k, v := range refs {
		cp[k] = v
	}
	return Snapshot{Secrets: cp}
}

// ResolveAndRegister re-runs resolution for every secret in a Snapshot,
// using the given provider stack, and registers each resolved value with
// the redaction registry. It returns the resolved secret map suitable for
// passing to OpenSession.
func (s Snapshot) ResolveAndRegister(ctx context.Context, stack *Stack, reg *Registry) (map[string]string, error) {
	out := make(map[string]string, len(s.Secrets))
	for name, ref := range s.Secrets {
		val, err := stack.Resolve(ctx, ref)
		if err != nil {
			return nil, &ResolveError{Name: name, Origin: ref, Cause: err}
		}
		out[name] = val
		if reg != nil {
			reg.Register(val)
		}
	}
	return out, nil
}
