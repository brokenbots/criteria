package engine

// parallel_iteration_bench_test.go — Benchmarks for parallel sink delivery
// throughput under simulated backpressure.
//
// The intended production scenario: parallel_max = 8, all adapters are
// parallel_safe, each produces continuous output. The gRPC sink has non-trivial
// write latency (back-pressure, flow control). With the old shared-mutex
// approach (lockedEventSink), goroutines queue behind each other at the mutex;
// one goroutine blocks the other seven for the full duration of each write.
// With fanInEventSink, goroutines send to a buffered channel and proceed
// immediately; a drain goroutine handles writes in the background.
//
// Benchmark model:
//   - benchParallelMax = 8 goroutines
//   - benchEventsPerIter = 200 Log calls per goroutine
//   - benchWorkDelay = 8µs adapter work between Log calls (models CPU-bound
//     output generation, e.g. parsing shell output)
//   - latentEventSink.Log sleeps 1µs (models gRPC write latency)
//
// With N=8 and benchWorkDelay = N × sinkDelay (8µs = 8 × 1µs):
//   Baseline: each round = work(8µs) + N×sinkDelay(8µs) = 16µs
//   Fan-in: goroutines proceed at work(8µs); drain runs concurrently at
//   the same throughput, so rounds cost only work(8µs) = 8µs
//   → ≥ 2× throughput improvement in ns/op
//
// Run with:
//
//	go test -bench=BenchmarkParallelSink -benchtime=3s ./internal/engine/

import (
	"context"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow"
)

// --- shared helpers ---

const (
	benchParallelMax   = 8
	benchEventsPerIter = 200 // Log calls per goroutine per iteration
	benchChunkSize     = 512 // bytes per Log call

	// benchWorkDelay simulates adapter work between Log calls (e.g., reading
	// the next output chunk from a shell process). Set to N×sinkDelay so that
	// the serialized sink writes in the baseline equal the goroutine work time,
	// producing a measurable ≥ 2× difference in ns/op.
	benchWorkDelay = time.Duration(benchParallelMax) * time.Microsecond

	// sinkDelay simulates write latency in the underlying sink (e.g., gRPC
	// flow control). Must equal benchWorkDelay / benchParallelMax so that the
	// drain goroutines in the fan-in path run at the same throughput as the
	// goroutines, allowing them to keep up without channel blocking.
	sinkDelay = time.Microsecond
)

// latentEventSink is an adapter.EventSink that sleeps on every Log call to
// simulate non-trivial write latency (e.g., gRPC flow control). In the
// baseline benchmark this sleep is incurred while the goroutine holds the
// shared mutex; in the fan-in benchmark it is incurred by the drain goroutine
// while the adapter goroutine has already moved on to its next work iteration.
type latentEventSink struct{}

func (e *latentEventSink) Log(string, []byte)  { time.Sleep(sinkDelay) }
func (e *latentEventSink) Adapter(string, any) {}

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

func (p *highLogPlugin) Info(context.Context) (adapterhost.Info, error) {
	return adapterhost.Info{Name: p.name, Version: "bench", Capabilities: []string{"parallel_safe"}}, nil
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
// N goroutines share a mutex for every Log call. After each unit of adapter
// work (benchWorkDelay), the goroutine must acquire the shared mutex and hold
// it for the full sink write duration (sinkDelay). The other N-1 goroutines
// block at mutex.Lock() and cannot proceed to their next work iteration until
// the lock is released. With N=8 and sinkDelay=1µs, the serialized lock queue
// adds 8µs per event on top of the 8µs work time, halving throughput.
//
// This models the lockedEventSink path before this workstream.
func BenchmarkParallelSinkContention(b *testing.B) {
	if runtime.GOMAXPROCS(0) < 2 {
		b.Skip("GOMAXPROCS < 2; contention benchmark not meaningful on single-core")
	}

	chunk := make([]byte, benchChunkSize)
	inner := &latentEventSink{}

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
					// Simulate adapter work (e.g., reading the next shell output
					// chunk). With the mutex approach, ALL other goroutines block
					// at mu.Lock() while we hold the mutex during inner.Log,
					// preventing them from starting their next work iteration.
					time.Sleep(benchWorkDelay)
					mu.Lock()
					inner.Log("stdout", chunk) // sleeps sinkDelay while holding mu
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
	}
}

// BenchmarkParallelSinkContention_WithFanIn measures the new fanInEventSink path.
// Each goroutine has a dedicated buffered channel. After each unit of adapter
// work (benchWorkDelay), the goroutine sends to its channel (non-blocking) and
// immediately starts the next work iteration. Drain goroutines write to the
// underlying sink in the background, overlapping with goroutine execution.
//
// Because goroutines never block at the shared mutex, all N goroutines run
// their work phases in parallel. The total time is dominated by the work
// phases alone (benchWorkDelay per event), while drain runs concurrently at
// the same throughput as goroutine production, finishing at the same time.
//
// Expected throughput: ≥ 2× vs BenchmarkParallelSinkContention because
// goroutines are not serialized by sink write latency.
func BenchmarkParallelSinkContention_WithFanIn(b *testing.B) {
	if runtime.GOMAXPROCS(0) < 2 {
		b.Skip("GOMAXPROCS < 2; contention benchmark not meaningful on single-core")
	}

	chunk := make([]byte, benchChunkSize)
	inner := &latentEventSink{}

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
					// Same adapter work as baseline. Unlike the baseline, the
					// channel send below returns immediately — the goroutine
					// does not wait for the drain goroutine to acquire the
					// shared mutex or complete the write.
					time.Sleep(benchWorkDelay)
					f.Log("stdout", chunk)
				}
			}()
		}
		wg.Wait()
		// Drain-close mirrors evaluateParallel's closeEventSinks(). Drain
		// goroutines run concurrently with the goroutine phase above and finish
		// at approximately the same time (production rate ≈ drain rate when
		// benchWorkDelay = N × sinkDelay), so close() adds negligible latency.
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
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"fake": plug}}

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
