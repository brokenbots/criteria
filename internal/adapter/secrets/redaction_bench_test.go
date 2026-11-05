package secrets

import (
	"fmt"
	"strings"
	"testing"
)

// sinkBenchInner discards events; it stands in for the downstream encode +
// publish path so the benchmarks isolate redaction cost.
type sinkBenchInner struct{}

func (sinkBenchInner) Log(stream string, chunk []byte) {}
func (sinkBenchInner) Adapter(kind string, data any)   {}

// adapterPayload builds event data shaped like real adapter emissions: a
// handful of string leaves and nested scalar containers.
func adapterPayload(i int) map[string]any {
	return map[string]any{
		"task":      fmt.Sprintf("run-%d", i),
		"detail":    "payload leaf that never matches a declared secret",
		"exit_code": 0,
		"steps":     []any{"checkout", "build", "test"},
		"meta": map[string]string{
			"branch": fmt.Sprintf("feature/%d", i),
			"sha":    fmt.Sprintf("%012x", i),
		},
	}
}

// Benchmarks redaction cost across registry sizes. Two facts to keep an eye
// on: the registry == 0 row must stay at forward-by-reference cost (the
// bypass is load-bearing for the no-declared-secrets common case), and the
// miss rows must stay roughly flat as the registry grows (the cached
// replacer, not a per-call rebuild).
func BenchmarkRedactingEventSink_Adapter(b *testing.B) {
	const secretCount = 16
	payloadKeys := make([]string, secretCount)
	for i := range payloadKeys {
		payloadKeys[i] = fmt.Sprintf("secret-%04d", i)
	}

	sizes := []struct {
		name   string
		fill   func(*Registry)
		hitKey string
	}{
		{name: "registry-0"},
		{name: "registry-8", fill: func(r *Registry) {
			for _, k := range payloadKeys[:8] {
				r.Register(k)
			}
		}, hitKey: "secret-0000"},
		{name: "registry-1024", fill: func(r *Registry) {
			for i := 0; i < 1024; i++ {
				r.Register(fmt.Sprintf("secret-%04d", i))
			}
		}, hitKey: "secret-0500"},
	}

	for _, size := range sizes {
		size := size
		for _, scenario := range []struct {
			name    string
			mutator func(*map[string]any)
		}{
			{name: "miss"},
			{name: "hit", mutator: func(m *map[string]any) {
				(*m)["task"] = size.hitKey
			}},
		} {
			size := size
			scenario := scenario
			b.Run(size.name+"/"+scenario.name, func(b *testing.B) {
				reg := NewRegistry()
				if size.fill != nil {
					size.fill(reg)
				}
				sink := &RedactingEventSink{Inner: sinkBenchInner{}, Registry: reg}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					payload := adapterPayload(i)
					if scenario.mutator != nil {
						scenario.mutator(&payload)
					}
					sink.Adapter("callee.started", payload)
				}
			})
		}
	}
}

// BenchmarkRedact walks string-only redaction to keep an eye on the cached
// replacer against larger haystacks (log-chunk-shaped work).
func BenchmarkRegistry_Redact(b *testing.B) {
	for _, secretCount := range []int{8, 1024} {
		secretCount := secretCount
		b.Run(fmt.Sprintf("registry-%d", secretCount), func(b *testing.B) {
			r := NewRegistry()
			for i := 0; i < secretCount; i++ {
				r.Register(fmt.Sprintf("secret-%04d", i))
			}
			line := strings.Repeat("payload leaf that never matches ", 8)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = r.Redact(line)
			}
		})
	}
}
