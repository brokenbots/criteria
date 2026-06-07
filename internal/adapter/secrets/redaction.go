package secrets

import (
	"io"
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
	// Use strings.Replacer for efficient multi-string replacement.
	oldnew := make([]string, 0, len(r.values)*2)
	for v := range r.values {
		oldnew = append(oldnew, v, "[REDACTED]")
	}
	return strings.NewReplacer(oldnew...).Replace(in)
}

// RedactBytes is the []byte variant of Redact.
func (r *Registry) RedactBytes(in []byte) []byte {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.values) == 0 {
		return in
	}
	oldnew := make([]string, 0, len(r.values)*2)
	for v := range r.values {
		oldnew = append(oldnew, v, "[REDACTED]")
	}
	return []byte(strings.NewReplacer(oldnew...).Replace(string(in)))
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
