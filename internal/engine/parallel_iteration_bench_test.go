package engine

// parallel_iteration_bench_test.go — Benchmarks for parallel sink delivery
// throughput. BenchmarkParallelSinkContention measures the old shared-mutex
// approach (lockedEventSink) where goroutines block on every Log call.
// BenchmarkParallelSinkContention_WithFanIn measures the new fanInEventSink
// approach where goroutines send to a buffered channel and a drain goroutine
// serializes writes to the underlying sink in the background.
//
// Both benchmarks use 8 goroutines and 200 Log calls per goroutine.
// The sink is a no-op; timing captures goroutine scheduling and synchronization
// overhead rather than I/O latency. In production, the fan-in benefit is most
// visible when the underlying sink has non-trivial write latency (e.g., gRPC
// flow control, buffered I/O): goroutines are not blocked during those writes,
// so parallel adapter execution is not serialized by sink contention.
//
// Run with:
//
//	go test -bench=BenchmarkParallelSink -benchtime=3s ./internal/engine/

import (
	"context"
	"runtime"
	"sync"
	"testing"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/plugin"
	"github.com/brokenbots/criteria/workflow"
)

// --- shared helpers ---

const (
	benchParallelMax   = 8
	benchEventsPerIter = 200 // Log calls per iteration
	benchChunkSize     = 512 // bytes per Log call
)

// noopEventSink is an adapter.EventSink that discards all events.
// Benchmarks use this to measure synchronization overhead without I/O noise.
type noopEventSink struct{}

func (e *noopEventSink) Log(string, []byte)  {}
func (e *noopEventSink) Adapter(string, any) {}

// throughputSink counts bytes delivered to Log so the benchmark can report
// bytes/sec. The count field is only read after all goroutines have finished.
type throughputSink struct {
	fakeSink
	mu    sync.Mutex
	bytes int64
}

func (s *throughputSink) StepEventSink(string) adapter.EventSink {
	return &throughputEventSink{parent: s}
}

type throughputEventSink struct {
	parent *throughputSink
}

func (e *throughputEventSink) Log(_ string, chunk []byte) {
	e.parent.mu.Lock()
	e.parent.bytes += int64(len(chunk))
	e.parent.mu.Unlock()
}
func (e *throughputEventSink) Adapter(string, any) {}

// highLogPlugin calls sink.Log benchEventsPerIter times per Execute call,
// simulating a shell adapter streaming continuous output.
type highLogPlugin struct {
	name  string
	chunk []byte
}

func (p *highLogPlugin) Info(context.Context) (plugin.Info, error) {
	return plugin.Info{Name: p.name, Version: "bench", Capabilities: []string{"parallel_safe"}}, nil
}
func (p *highLogPlugin) OpenSession(context.Context, string, map[string]string) error { return nil }
func (p *highLogPlugin) Execute(_ context.Context, _ string, _ *workflow.StepNode, sink adapter.EventSink) (adapter.Result, error) {
	for i := 0; i < benchEventsPerIter; i++ {
		sink.Log("stdout", p.chunk)
	}
	return adapter.Result{Outcome: "success"}, nil
}
func (p *highLogPlugin) Permit(context.Context, string, string, bool, string) error { return nil }
func (p *highLogPlugin) CloseSession(context.Context, string) error                 { return nil }
func (p *highLogPlugin) Kill()                                                      {}

// buildParallelBenchWorkflow compiles a parallel step with n items all using
// the "fake" adapter. Uses injectDefaultAdapters (same package) to resolve
// the bare adapter reference.
func buildParallelBenchWorkflow(b *testing.B, n int) *workflow.FSMGraph {
	b.Helper()
	items := `["a"`
	for i := 1; i < n; i++ {
		items += `, "x"`
	}
	items += `]`
	src := `
workflow "bench" {
  version       = "0.1"
  initial_state = "work"
  target_state  = "done"
}
step "work" {
  target       = adapter.fake
  parallel     = ` + items + `
  parallel_max = ` + itoa(n) + `
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "failed" }
}
state "done" {
  terminal = true
  success  = true
}
state "failed" {
  terminal = true
  success  = false
}
`
	src = injectDefaultAdapters(src)
	spec, diags := workflow.Parse("bench.hcl", []byte(src))
	if diags.HasErrors() {
		b.Fatalf("parse: %s", diags.Error())
	}
	graph, diags := workflow.Compile(spec, nil)
	if diags.HasErrors() {
		b.Fatalf("compile: %s", diags.Error())
	}
	return graph
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	buf := make([]byte, 0, 10)
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	return string(buf)
}

// BenchmarkParallelSinkContention measures the OLD shared-mutex approach:
// N goroutines all contend on a single sync.Mutex for every Log call.
// Each goroutine must hold the shared mutex for the full duration of each
// Log call. With a slow underlying sink (e.g., gRPC write), goroutines
// serialize and cannot proceed with their own adapter work in parallel.
//
// This directly models the lockedEventSink path before this workstream.
func BenchmarkParallelSinkContention(b *testing.B) {
	if runtime.GOMAXPROCS(0) < 2 {
		b.Skip("GOMAXPROCS < 2; contention benchmark not meaningful on single-core")
	}

	chunk := make([]byte, benchChunkSize)
	inner := &noopEventSink{}

	b.ResetTimer()
	b.ReportAllocs()
	b.SetBytes(int64(benchParallelMax * benchEventsPerIter * benchChunkSize))

	for i := 0; i < b.N; i++ {
		var mu sync.Mutex
		var wg sync.WaitGroup
		for g := 0; g < benchParallelMax; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < benchEventsPerIter; j++ {
					mu.Lock()
					inner.Log("stdout", chunk)
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
	}
}

// BenchmarkParallelSinkContention_WithFanIn measures the new fanInEventSink path.
// N goroutines each have a buffered channel. Log calls are non-blocking channel
// sends; per-goroutine drain goroutines write to the underlying sink under the
// shared mutex. Goroutines can proceed immediately after a Log call without
// waiting for the shared mutex. With a slow underlying sink, this decouples
// adapter execution time from sink write latency.
//
// Total measurement includes goroutine phase + drain-close, matching the
// evaluateParallel lifecycle (runParallelIterations + closeEventSinks).
func BenchmarkParallelSinkContention_WithFanIn(b *testing.B) {
	if runtime.GOMAXPROCS(0) < 2 {
		b.Skip("GOMAXPROCS < 2; contention benchmark not meaningful on single-core")
	}

	chunk := make([]byte, benchChunkSize)
	inner := &noopEventSink{}

	b.ResetTimer()
	b.ReportAllocs()
	b.SetBytes(int64(benchParallelMax * benchEventsPerIter * benchChunkSize))

	for i := 0; i < b.N; i++ {
		var mu sync.Mutex
		fans := make([]*fanInEventSink, benchParallelMax)
		for g := range fans {
			fans[g] = newFanInEventSink(inner, &mu, parallelLogBufSize)
		}

		var wg sync.WaitGroup
		for _, f := range fans {
			f := f
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < benchEventsPerIter; j++ {
					f.Log("stdout", chunk)
				}
			}()
		}
		wg.Wait()

		// Drain-close mirrors evaluateParallel's closeEventSinks() call.
		for _, f := range fans {
			f.close()
		}
	}
}

// BenchmarkParallelEngine_WithFanIn measures end-to-end parallel step throughput
// through the full engine with a high-log-volume adapter, confirming that the
// fanInEventSink integration in evaluateParallel works correctly at benchmark scale.
func BenchmarkParallelEngine_WithFanIn(b *testing.B) {
	if runtime.GOMAXPROCS(0) < 2 {
		b.Skip("GOMAXPROCS < 2; benchmark not meaningful on single-core")
	}

	graph := buildParallelBenchWorkflow(b, benchParallelMax)
	chunk := make([]byte, benchChunkSize)
	plug := &highLogPlugin{name: "fake", chunk: chunk}
	loader := &fakeLoader{plugins: map[string]plugin.Plugin{"fake": plug}}

	b.ResetTimer()
	b.ReportAllocs()
	b.SetBytes(int64(benchParallelMax * benchEventsPerIter * benchChunkSize))

	for i := 0; i < b.N; i++ {
		sink := &throughputSink{}
		if err := New(graph, loader, sink).Run(context.Background()); err != nil {
			b.Fatalf("run: %v", err)
		}
	}
}
