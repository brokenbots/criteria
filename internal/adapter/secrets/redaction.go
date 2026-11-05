package secrets

import (
	"encoding/json"
	"io"
	"sort"
	"strings"
	"sync"
)

// Registry holds secret values that must be redacted from all host output.
type Registry struct {
	mu     sync.RWMutex
	values map[string]struct{}
	// replacer caches the compiled strings.Replacer for the current value
	// set. Register clears it and the next redaction rebuilds it under the
	// write lock; strings.Replacer is immutable and safe for concurrent use,
	// so a reader that snapshots the pointer keeps serving the pre-register
	// set — the same semantics the old per-call rebuild had, without the
	// per-call cost. The cache is what keeps repeated redactions (log
	// chunks, every string leaf of a structured event) affordable as the
	// registry grows.
	replacer *strings.Replacer
}

// NewRegistry creates an empty redaction registry.
func NewRegistry() *Registry {
	return &Registry{values: make(map[string]struct{})}
}

// Register adds a value to the redaction set. Empty values are ignored.
func (r *Registry) Register(value string) {
	if value == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.values[value] = struct{}{}
	r.replacer = nil
}

// Len returns the number of registered values. Sink boundaries gate on it
// to bypass structured redaction entirely when nothing is declared.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.values)
}

// Redact replaces every registered value in the input with "[REDACTED]".
func (r *Registry) Redact(in string) string {
	rp := r.replacerFor()
	if rp == nil {
		return in
	}
	return rp.Replace(in)
}

// RedactBytes is the []byte variant of Redact.
func (r *Registry) RedactBytes(in []byte) []byte {
	rp := r.replacerFor()
	if rp == nil {
		return in
	}
	return []byte(rp.Replace(string(in)))
}

// replacerFor returns the cached replacer, or nil when nothing is
// registered. Rebuilds once under the write lock when a Register
// invalidated the cache.
func (r *Registry) replacerFor() *strings.Replacer {
	r.mu.RLock()
	rp, empty := r.replacer, len(r.values) == 0
	r.mu.RUnlock()
	if rp != nil || empty {
		return rp
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.replacer != nil || len(r.values) == 0 {
		return r.replacer
	}
	r.replacer = strings.NewReplacer(r.replacerArgsLocked()...)
	return r.replacer
}

// replacerArgsLocked builds deterministic arguments for strings.NewReplacer.
// Longer secrets are ordered first so that overlapping secrets are redacted
// completely; shorter secrets that are substrings of longer ones cannot
// leave fragments behind. Equal-length secrets are sorted lexicographically
// so the order is stable across calls and goroutines. Caller must hold r.mu
// exclusively.
func (r *Registry) replacerArgsLocked() []string {
	values := make([]string, 0, len(r.values))
	for v := range r.values {
		values = append(values, v)
	}
	sort.Slice(values, func(i, j int) bool {
		if len(values[i]) != len(values[j]) {
			return len(values[i]) > len(values[j])
		}
		return values[i] < values[j]
	})

	oldnew := make([]string, 0, len(values)*2)
	for _, v := range values {
		oldnew = append(oldnew, v, "[REDACTED]")
	}
	return oldnew
}

// Wrap returns an io.Writer that redacts all written bytes before forwarding
// to the underlying writer.
func (r *Registry) Wrap(w io.Writer) io.Writer {
	return &redactingWriter{registry: r, inner: w}
}

type redactingWriter struct {
	registry *Registry
	inner    io.Writer
}

func (w *redactingWriter) Write(p []byte) (n int, err error) {
	redacted := w.registry.RedactBytes(p)
	_, err = w.inner.Write(redacted)
	// Return the original length so callers don't see short writes.
	return len(p), err
}

// EventSink is the minimal adapter event sink interface that redaction wraps.
type EventSink interface {
	Log(stream string, chunk []byte)
	Adapter(kind string, data any)
}

// RedactingEventSink wraps an EventSink and redacts log chunks and
// structured adapter payloads before forwarding them.
//
// The structured path is gated per the registry design intent (bypass): when
// no values are declared — the common run — Adapter events are forwarded by
// reference with zero conversion cost. When values ARE declared, the payload
// is walked so every string that will land inside the published adapter
// Struct — string leaves and object keys — is masked at the JSON-string
// level, the same strings the run-side Struct conversion serializes to the
// event store. Walked containers are cloned, never mutated in place: a
// caller may keep using its map after emitting the event.
//
// Residual gap: this bounds the known-secret leak, not the unknown one.
// Value-shaped secrets that were never registered (e.g. a high-entropy
// string the workflow never declared as a sensitive input or output) cannot
// be recognized here; declaring them so the host registers them with the
// redaction registry is the only way to bring a value under the mask.
type RedactingEventSink struct {
	Inner    EventSink
	Registry *Registry
}

func (s *RedactingEventSink) Log(stream string, chunk []byte) {
	// A nil Registry forwards unchanged: a sink with nothing to mask has no
	// business taking the run down.
	if s.Registry == nil {
		s.Inner.Log(stream, chunk)
		return
	}
	s.Inner.Log(stream, s.Registry.RedactBytes(chunk))
}

func (s *RedactingEventSink) Adapter(kind string, data any) {
	if s.Registry == nil || s.Registry.Len() == 0 {
		// Bypass: nothing is declared for this run, so the payload cannot
		// carry a registered secret. Forward by reference.
		s.Inner.Adapter(kind, data)
		return
	}
	s.Inner.Adapter(s.Registry.Redact(kind), s.redactData(data))
}

// redactData returns a redacted copy of a structured adapter payload.
// Callers must hold a non-empty registry. Object keys are masked alongside
// values: engine-side structured redaction (step outputs, typed values)
// masks keys, and a key carrying a secret is a leak like any other. Only
// the JSON-encodable shapes ride this path — AdapterEvent data is a
// google.protobuf.Struct — so anything else is normalized through
// redactViaJSON.
func (s *RedactingEventSink) redactData(data any) any {
	switch v := data.(type) {
	case nil:
		return nil
	case string:
		return s.Registry.Redact(v)
	case map[string]any, []any, map[string]string, []string:
		// redactContainer re-switches on the dynamic type; every type in
		// this case is always handled there.
		out, _ := s.redactContainer(data)
		return out
	default:
		// Numeric/boolean scalars cannot hold a secret; passing them
		// through preserves the caller's types instead of widening ints
		// to float64 via the JSON fallback. json.Number is numeric text,
		// never quoted, so there is no string to mask.
		switch data.(type) {
		case bool, int, int8, int16, int32, int64,
			uint, uint8, uint16, uint32, uint64, uintptr,
			float32, float64, json.Number:
			return data
		}
		return s.redactViaJSON(data)
	}
}

// redactContainer clones the four container shapes the JSON conversion
// handles natively, masking keys and string values as it goes. handled is
// false only when data is none of those types (an unreachable case from
// redactData, kept explicit for the tests' direct calls).
//
// Typed-nil containers must stay nil: the run-side conversion encodes them
// as JSON null, and the masked path must publish the same shape the
// unchanged path does.
func (s *RedactingEventSink) redactContainer(data any) (any, bool) {
	switch v := data.(type) {
	case map[string]any:
		if v == nil {
			return nil, true
		}
		out := make(map[string]any, len(v))
		for k, item := range v {
			out[s.Registry.Redact(k)] = s.redactData(item)
		}
		return out, true
	case []any:
		if v == nil {
			return nil, true
		}
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = s.redactData(item)
		}
		return out, true
	case map[string]string:
		if v == nil {
			return nil, true
		}
		out := make(map[string]string, len(v))
		for k, item := range v {
			out[s.Registry.Redact(k)] = s.Registry.Redact(item)
		}
		return out, true
	case []string:
		if v == nil {
			return nil, true
		}
		out := make([]string, len(v))
		for i, item := range v {
			out[i] = s.Registry.Redact(item)
		}
		return out, true
	default:
		return nil, false
	}
}

// redactViaJSON masks payload shapes the typed walk does not cover. The
// value is re-encoded exactly as the run-side Struct conversion
// (encodeAdapterData) will encode it, then decoded and walked so masks
// apply to the DECODED string values — a byte-level replace on the encoded
// JSON would miss a secret containing characters JSON escapes (quotes,
// newlines, HTML-escaped <>&). The returned tree carries decoded JSON
// scalars, which the downstream conversion reproduces deterministically.
func (s *RedactingEventSink) redactViaJSON(data any) any {
	// Encode failures are forwarded unchanged: the same value fails the
	// run-side conversion too, which records an _encode_error struct (with
	// no raw payload) instead of the value, so nothing unmasked is stored.
	raw, err := json.Marshal(data)
	if err != nil {
		return data
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return data
	}
	return s.redactDecoded(decoded)
}

// redactDecoded walks a decoded JSON tree. Numbers, booleans, and null pass
// through; strings and containers are masked. Decoded trees contain only
// JSON scalars, so the default arm must not recurse back into redactData
// (whose JSON fallback would loop on those same values).
func (s *RedactingEventSink) redactDecoded(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, item := range t {
			out[s.Registry.Redact(k)] = s.redactDecoded(item)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = s.redactDecoded(item)
		}
		return out
	case string:
		return s.Registry.Redact(t)
	default:
		return v
	}
}
