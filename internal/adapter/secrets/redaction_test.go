package secrets

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"sync"
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
	// If one secret is a substring of another, the longer one must be
	// redacted first so no fragment of the longer secret survives.
	r := NewRegistry()
	r.Register("secret")
	r.Register("secret123")

	in := "secret123 and secret"
	out := r.Redact(in)
	require.Equal(t, "[REDACTED] and [REDACTED]", out)
	require.NotContains(t, out, "secret123")
	require.NotContains(t, out, "secret")
	require.NotContains(t, out, "123")
}

func TestRegistry_RedactBytes_Overlapping(t *testing.T) {
	r := NewRegistry()
	r.Register("secret")
	r.Register("secret123")

	in := []byte("secret123 and secret")
	out := r.RedactBytes(in)
	require.Equal(t, "[REDACTED] and [REDACTED]", string(out))
	require.NotContains(t, string(out), "secret123")
	require.NotContains(t, string(out), "secret")
	require.NotContains(t, string(out), "123")
}

func TestRegistry_Redact_PrefixChain(t *testing.T) {
	// A chain of secrets where each is a prefix of the next. Redaction
	// must be complete regardless of registration order.
	r := NewRegistry()
	r.Register("p")
	r.Register("password")
	r.Register("password123")
	r.Register("password123!")

	in := "p password password123 password123!"
	out := r.Redact(in)
	require.Equal(t, "[REDACTED] [REDACTED] [REDACTED] [REDACTED]", out)
	require.NotContains(t, out, "p")
	require.NotContains(t, out, "password")
	require.NotContains(t, out, "password123")
	require.NotContains(t, out, "password123!")
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

// recordingEventSink captures Log and Adapter calls.
type recordingEventSink struct {
	logs     []logCall
	adapters []adapterCall
}

type logCall struct {
	stream string
	chunk  []byte
}

type adapterCall struct {
	kind string
	data any
}

func (s *recordingEventSink) Log(stream string, chunk []byte) {
	s.logs = append(s.logs, logCall{stream: stream, chunk: chunk})
}
func (s *recordingEventSink) Adapter(kind string, data any) {
	s.adapters = append(s.adapters, adapterCall{kind: kind, data: data})
}

func TestRedactingEventSink_Log_RedactsBytes(t *testing.T) {
	inner := &recordingEventSink{}
	reg := NewRegistry()
	reg.Register("secret123")

	sink := &RedactingEventSink{Inner: inner, Registry: reg}
	sink.Log("stdout", []byte("hello secret123 world"))

	require.Len(t, inner.logs, 1)
	require.Equal(t, "stdout", inner.logs[0].stream)
	require.Equal(t, "hello [REDACTED] world", string(inner.logs[0].chunk))
}

func TestRedactingEventSink_Adapter_MasksDeclaredSecrets(t *testing.T) {
	reg := NewRegistry()
	reg.Register("secret123")

	// Nested containers: values and map keys on the masked path.
	nested := map[string]any{
		"plain": "visible",
		"secret123": map[string]any{
			"token":  "secret123",
			"list":   []any{"safe", "secret123", 7, true, nil},
			"nested": []string{"secret123", "ok"},
		},
	}
	// map[string]string payloads straight from an adapter emit.
	strMap := map[string]string{"task": "secret123", "note": "safe"}
	// Raw string payload.
	single := "prefix secret123 suffix"

	assert := func(call adapterCall) {
		require.NotContains(t, fmt.Sprintf("%v", call), "secret123")
	}

	assert(sinkCall(t, reg, "callee.started", nested))
	assert(sinkCall(t, reg, "callee.started", strMap))
	assert(sinkCall(t, reg, "callee.started", single))
	assert(sinkCall(t, reg, "kind-secret123-call", "no payload secret"))

	// Shape checks on the nested case.
	call := sinkCall(t, reg, "callee.started", nested)
	got, ok := call.data.(map[string]any)
	require.True(t, ok, "data should be a cloned, redacted map")
	require.Equal(t, "visible", got["plain"])
	require.NotContains(t, got, "secret123", "keys carrying secrets are masked too")
	// The key "secret123" is masked to "[REDACTED]" and its subtree walked.
	inner, ok := got["[REDACTED]"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "[REDACTED]", inner["token"])
	list, ok := inner["list"].([]any)
	require.True(t, ok)
	require.Equal(t, "[REDACTED]", list[1])
	require.Equal(t, 7, list[2])
	require.Equal(t, true, list[3])
	require.Nil(t, list[4])
	tags, ok := inner["nested"].([]string)
	require.True(t, ok)
	require.Equal(t, []string{"[REDACTED]", "ok"}, tags)

	// The caller's map must not be mutated by the walk.
	require.Contains(t, fmt.Sprintf("%v", nested), "secret123")

	// The kind is masked too: kinds are free-form adapter strings.
	kindCall := sinkCall(t, reg, "kind-secret123-call", nil)
	require.Equal(t, "kind-[REDACTED]-call", kindCall.kind)
	require.Nil(t, kindCall.data)
}

func sinkCall(t *testing.T, reg *Registry, kind string, data any) adapterCall {
	t.Helper()
	inner := &recordingEventSink{}
	sink := &RedactingEventSink{Inner: inner, Registry: reg}
	sink.Adapter(kind, data)
	require.Len(t, inner.adapters, 1)
	return inner.adapters[0]
}

func TestRedactingEventSink_Adapter_EmptyRegistryBypass(t *testing.T) {
	inner := &recordingEventSink{}
	sink := &RedactingEventSink{Inner: inner, Registry: NewRegistry()}

	sent := map[string]any{"task": "untouched"}
	sink.Adapter("callee.started", sent)

	require.Len(t, inner.adapters, 1)
	require.Equal(t, "callee.started", inner.adapters[0].kind)
	// Forwarded by reference: no conversion, no copy — pointer identity.
	require.Equal(
		t,
		reflect.ValueOf(sent).Pointer(),
		reflect.ValueOf(inner.adapters[0].data).Pointer(),
		"empty registry must forward the payload by reference",
	)
}

func TestRedactingEventSink_NilRegistry(t *testing.T) {
	inner := &recordingEventSink{}
	sink := &RedactingEventSink{Inner: inner, Registry: nil}

	// A nil registry has nothing to mask: both paths forward unchanged
	// instead of taking the run down.
	require.NotPanics(t, func() {
		sink.Log("stdout", []byte("hello"))
		sink.Adapter("callee.started", map[string]any{"k": "v"})
	})
	require.Len(t, inner.logs, 1)
	require.Len(t, inner.adapters, 1)
	require.Equal(t, map[string]any{"k": "v"}, inner.adapters[0].data)
}

func TestRedactingEventSink_Adapter_JSONFallback(t *testing.T) {
	reg := NewRegistry()

	t.Run("escapes survive byte-replace", func(t *testing.T) {
		reg.Register("tok\"\n<>&")
		// A struct payload rides the JSON fallback, and the secret contains
		// characters JSON escapes — a naive byte-level replace on encoded
		// JSON would miss it.
		payload := struct{ Token string }{Token: "pre tok\"\n<>& post"}
		call := sinkCall(t, reg, "callee.started", payload)
		require.NotContains(t, fmt.Sprintf("%v", call.data), "tok\"")
		require.Contains(t, fmt.Sprintf("%v", call.data), "[REDACTED]")
		require.Contains(t, fmt.Sprintf("%v", call.data), "pre ")
		require.Contains(t, fmt.Sprintf("%v", call.data), " post")
	})

	t.Run("preserves non-string scalars", func(t *testing.T) {
		reg2 := NewRegistry()
		reg2.Register("secret123")
		type counters struct {
			Name string
			N    int
			OK   bool
		}
		payload := counters{Name: "secret123", N: 42, OK: true}
		call := sinkCall(t, reg2, "callee.started", payload)
		got, ok := call.data.(map[string]any)
		require.True(t, ok, "JSON-walked payload decodes to a map tree")
		require.Equal(t, "[REDACTED]", got["Name"])
		require.Equal(t, float64(42), got["N"])
		require.Equal(t, true, got["OK"])
	})

	t.Run("marshal failure forwards unchanged", func(t *testing.T) {
		reg3 := NewRegistry()
		reg3.Register("secret123")
		sent := fakeUnmarshalable{err: true}
		call := sinkCall(t, reg3, "callee.started", sent)
		require.Equal(t, "callee.started", call.kind)
		require.Equal(t, sent, call.data)
	})
}

// fakeUnmarshalable fails json.Marshal via its MarshalJSON method.
type fakeUnmarshalable struct{ err bool }

func (f fakeUnmarshalable) MarshalJSON() ([]byte, error) {
	if f.err {
		return nil, errors.New("no")
	}
	return []byte(`{}`), nil
}

func TestRegistry_ReplacerCacheLifecycle(t *testing.T) {
	r := NewRegistry()
	require.Equal(t, 0, r.Len())
	require.Nil(t, r.replacerFor(), "empty registry must not build a replacer")

	r.Register("secret123")
	require.Equal(t, 1, r.Len())
	require.Nil(t, r.replacer, "no replacer until first use")

	require.Equal(t, "[REDACTED]", r.Redact("secret123"))
	require.NotNil(t, r.replacerFor(), "first redaction builds the cache")
	cached := r.replacer

	// Register invalidates the cache; the next redaction picks up the new
	// value.
	r.Register("second")
	require.Nil(t, r.replacer, "Register must invalidate the cached replacer")
	require.Equal(t, "[REDACTED] and [REDACTED]", r.Redact("secret123 and second"))
	require.NotSame(t, cached, r.replacerFor())
}

func TestRegistry_ConcurrentRegisterAndRedact(t *testing.T) {
	// Race-detector probe: Register invalidation and replacer rebuild run
	// concurrently with reads from redaction consumers.
	r := NewRegistry()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			val := fmt.Sprintf("secret%d", i)
			r.Register(val)
			for j := 0; j < 200; j++ {
				out := r.Redact("x " + val + " y")
				require.NotContains(t, out, val)
			}
		}(i)
	}
	wg.Wait()
}
