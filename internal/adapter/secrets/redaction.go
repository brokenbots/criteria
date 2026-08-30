package secrets

import (
	"io"
	"sort"
	"strings"
	"sync"
)

// Registry holds secret values that must be redacted from all host output.
type Registry struct {
	mu     sync.RWMutex
	values map[string]struct{}
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
}

// Redact replaces every registered value in the input with "[REDACTED]".
func (r *Registry) Redact(in string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.values) == 0 {
		return in
	}
	return strings.NewReplacer(r.replacerArgs()...).Replace(in)
}

// RedactBytes is the []byte variant of Redact.
func (r *Registry) RedactBytes(in []byte) []byte {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.values) == 0 {
		return in
	}
	return []byte(strings.NewReplacer(r.replacerArgs()...).Replace(string(in)))
}

// replacerArgs builds deterministic arguments for strings.NewReplacer.
// Longer secrets are ordered first so that overlapping secrets are redacted
// completely; shorter secrets that are substrings of longer ones cannot
// leave fragments behind. Equal-length secrets are sorted lexicographically
// so the order is stable across calls and goroutines.
func (r *Registry) replacerArgs() []string {
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

// RedactingEventSink wraps an EventSink and redacts log chunks before
// forwarding them. Structured Adapter events are passed through unchanged.
type RedactingEventSink struct {
	Inner    EventSink
	Registry *Registry
}

func (s *RedactingEventSink) Log(stream string, chunk []byte) {
	s.Inner.Log(stream, s.Registry.RedactBytes(chunk))
}

func (s *RedactingEventSink) Adapter(kind string, data any) {
	s.Inner.Adapter(kind, data)
}
