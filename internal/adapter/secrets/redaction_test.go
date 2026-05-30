package secrets

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRegistry_Redact(t *testing.T) {
	r := NewRegistry()
	r.Register("secret1")
	r.Register("secret2")

	in := "hello secret1 world secret2 end"
	out := r.Redact(in)
	require.Equal(t, "hello [REDACTED] world [REDACTED] end", out)
}

func TestRegistry_Redact_NoMatch(t *testing.T) {
	r := NewRegistry()
	r.Register("secret1")

	in := "hello world"
	out := r.Redact(in)
	require.Equal(t, "hello world", out)
}

func TestRegistry_Redact_Overlapping(t *testing.T) {
	// If one secret is a substring of another, the longer one should be
	// redacted first (Replacer handles this deterministically).
	r := NewRegistry()
	r.Register("secret")
	r.Register("secret123")

	in := "secret123 and secret"
	out := r.Redact(in)
	// Replacer processes in registration order, but both get replaced.
	require.NotContains(t, out, "secret123")
	require.NotContains(t, out, "secret")
	require.Contains(t, out, "[REDACTED]")
}

func TestRegistry_RedactBytes(t *testing.T) {
	r := NewRegistry()
	r.Register("foo")

	in := []byte("hello foo world")
	out := r.RedactBytes(in)
	require.Equal(t, "hello [REDACTED] world", string(out))
}

func TestRegistry_Wrap(t *testing.T) {
	r := NewRegistry()
	r.Register("shh")

	var buf bytes.Buffer
	w := r.Wrap(&buf)

	_, err := w.Write([]byte("before shh after"))
	require.NoError(t, err)
	require.Equal(t, "before [REDACTED] after", buf.String())
}

func TestRegistry_Wrap_SingleChunk(t *testing.T) {
	// The writer redacts each Write call independently. Secrets that span
	// multiple Write calls are not redacted; callers should write complete
	// chunks (e.g. lines or messages).
	r := NewRegistry()
	r.Register("secret")

	var buf bytes.Buffer
	w := r.Wrap(&buf)

	_, err := w.Write([]byte("prefix "))
	require.NoError(t, err)
	_, err = w.Write([]byte("secret suffix"))
	require.NoError(t, err)

	// "secret" is fully contained in the second Write, so it is redacted.
	require.Equal(t, "prefix [REDACTED] suffix", buf.String())
}

func TestRegistry_Wrap_MultipleChunks(t *testing.T) {
	r := NewRegistry()
	r.Register("abc")
	r.Register("xyz")

	var buf bytes.Buffer
	w := r.Wrap(&buf)

	_, err := w.Write([]byte("start abc middle xyz end"))
	require.NoError(t, err)
	require.Equal(t, "start [REDACTED] middle [REDACTED] end", buf.String())
}

func TestRegistry_DuplicateRegister(t *testing.T) {
	r := NewRegistry()
	r.Register("dup")
	r.Register("dup")

	out := r.Redact("dup")
	require.Equal(t, "[REDACTED]", out)
}
