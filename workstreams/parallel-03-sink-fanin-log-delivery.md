# parallel-03 — Sink fan-in for parallel log delivery

**Owner:** Workstream executor · **Depends on:** none · **Coordinates with:** parallel-01, parallel-02 (independent)

## Context

`evaluateParallel` wraps the shared `Sink` in a `lockedSink` before launching
goroutines. Every `Sink` method — including `StepEventSink` and the
`Log`/`Adapter` calls on the returned `EventSink` — serializes under a single
`sync.Mutex`. The intent is correct: prevent data races on the underlying sink
(e.g. `ConsoleSink`, gRPC transport writer).

The problem is **back-pressure propagation**. If the underlying sink is slow to
handle one goroutine's `Log` call (gRPC flow control, disk I/O, a slow test
spy), all other goroutines block waiting for the same mutex. In the worst case,
adapter log delivery fully serializes parallel execution even when the adapters
themselves are concurrent.

Concrete scenario:
- Parallel step with `parallel_max = 8`, all adapters are `parallel_safe`.
- Each adapter streams 500 KB of output in 100-ms chunks.
- The gRPC sink has 4 MB/s of write bandwidth.
- Each goroutine's `Log` hold time: ~2 ms per chunk.
- With the current single mutex, goroutines queue behind each other: effective
  throughput is ≈ 1/8 of theoretical maximum.

### Root cause

```go
// lockedSink.StepEventSink — current implementation
func (s *lockedSink) StepEventSink(step string) adapter.EventSink {
    s.mu.Lock()
    inner := s.Sink.StepEventSink(step)
    s.mu.Unlock()
    return &lockedEventSink{EventSink: inner, mu: &s.mu}  // shares the SAME mutex
}
```

Each goroutine gets a `lockedEventSink` that shares the parent `*sync.Mutex`.
High-frequency `Log` and `Adapter` calls from N goroutines all queue behind
one lock.

### Proposed fix (sketch)

Replace the shared-mutex `lockedEventSink` with per-goroutine **buffered
channels** and a single fan-in goroutine that drains them into the underlying
sink:

```
Goroutine 0 → chan0 ──┐
Goroutine 1 → chan1 ──┤ fan-in goroutine → underlying sink (serialized)
Goroutine 2 → chan2 ──┘
```

Key properties:
- `Log`/`Adapter` calls on each per-goroutine channel are non-blocking up to
  the buffer size. Goroutines do not wait on each other.
- The fan-in goroutine serializes delivery to the underlying sink, so the
  sink implementation never needs to be thread-safe.
- Metadata/lifecycle events (e.g. `OnStepStarted`, `OnStepCompleted`) still go
  through the shared `lockedSink` mutex — they are rare and ordering matters.
- Only `Log` and `Adapter` streaming events go through channels.

Implementation sketch:

```go
type fanInSink struct {
    // inner is the underlying per-step EventSink from lockedSink.StepEventSink.
    inner  adapter.EventSink
    ch     chan sinkEvent
    done   chan struct{}
}

type sinkEvent struct {
    stream string
    chunk  []byte
    kind   string
    data   any
}

func newFanInSink(inner adapter.EventSink, bufSize int) *fanInSink {
    f := &fanInSink{inner: inner, ch: make(chan sinkEvent, bufSize), done: make(chan struct{})}
    go f.drain()
    return f
}

func (f *fanInSink) drain() {
    defer close(f.done)
    for e := range f.ch {
        if e.chunk != nil {
            f.inner.Log(e.stream, e.chunk)
        } else {
            f.inner.Adapter(e.kind, e.data)
        }
    }
}

func (f *fanInSink) Log(stream string, chunk []byte) {
    // Non-blocking send; if full, fall back to direct (blocking) send
    // so we never lose output.
    f.ch <- sinkEvent{stream: stream, chunk: append([]byte(nil), chunk...)}
}

func (f *fanInSink) Adapter(kind string, data any) {
    f.ch <- sinkEvent{kind: kind, data: data}
}

func (f *fanInSink) Close() {
    close(f.ch)
    <-f.done
}
```

`runParallelIterations` would create one `fanInSink` per iteration (replacing
the shared `lockedEventSink`), and close all of them after goroutines finish.

### Scope gate

This workstream is **low priority** for the initial parallel correctness fix
(parallel-01 + parallel-02). It becomes material when:
- Adapters stream large volumes of log output (shell + large programs), AND
- `parallel_max` > 4, AND
- The underlying sink has non-trivial delivery latency (gRPC back-pressure,
  server runs).

For the Copilot adapter (`parallel_safe = false`), this workstream is
irrelevant — Copilot steps cannot use `parallel = [...]` after parallel-02.

**Implement this workstream only after parallel-01 and parallel-02 are merged
and a profiling trace confirms sink contention is a measurable bottleneck.**

## Prerequisites

- parallel-01 and parallel-02 are merged and green.
- A profiling trace or benchmark that demonstrates sink lock contention at
  realistic `parallel_max` values (suggested: `parallel_max = 8`, shell adapter
  with a command that produces continuous output).

## In scope

### Step 1 — Benchmark to quantify the problem

**File:** `internal/engine/parallel_iteration_bench_test.go` (new)

Write a benchmark `BenchmarkParallelSinkContention` that:
1. Runs a parallel step with `parallel_max = 8` against a shell adapter step
   (or a test adapter that calls `sink.Log` in a tight loop).
2. Measures wall-clock throughput (bytes/sec delivered to the sink).
3. Reports with/without the shared mutex path so regression is detectable.

This benchmark gates the implementation decision.

---

### Step 2 — Implement `fanInEventSink` in `parallel_iteration.go`

Replace `lockedEventSink` usage in `StepEventSink` with a per-goroutine
`fanInEventSink` (channel-based). The exact buffer size is configurable via a
constant (suggest `parallelLogBufSize = 256` events).

`runParallelIterations` returns only after all goroutines complete AND all
fan-in goroutines have drained. Add a `closeEventSinks()` call in the
post-goroutine cleanup path to close channel writers and wait for `done`.

---

### Step 3 — Metadata events remain on the shared mutex

All `Sink` methods other than `StepEventSink`-derived `Log`/`Adapter` continue
to use the `lockedSink` mutex. This preserves ordering guarantees for lifecycle
events.

---

### Step 4 — Tests

```
BenchmarkParallelSinkContention_WithFanIn   // should show ≥ 2× throughput vs baseline
TestFanInEventSink_AllEventsDelivered       // no events dropped under concurrent load
TestFanInEventSink_RaceDetector             // go test -race passes
```

---

## Behavior change

**Yes (observable only at high throughput).** Log event delivery order across
goroutines changes from "whichever goroutine holds the mutex first" to
"whichever goroutine's channel the fan-in goroutine services next" (FIFO per
goroutine, interleaved across goroutines). This is acceptable — parallel log
interleaving has no defined order guarantee.

## Reuse

- `lockedSink` / `lockedEventSink` remain for metadata events; `fanInEventSink`
  is a drop-in `adapter.EventSink` replacement only for streaming events.

## Out of scope

- Changes to `Sink` interface methods (non-streaming lifecycle events).
- Ordering guarantees across goroutines (none are promised for `Log`).
- Backpressure signaling to adapters — out of scope.

## Files this workstream may modify

- `internal/engine/parallel_iteration.go`
- `internal/engine/parallel_iteration_bench_test.go` (new)
- `internal/engine/parallel_iteration_test.go`

This workstream may **not** edit `README.md`, `PLAN.md`, `AGENTS.md`,
`CHANGELOG.md`, `CONTRIBUTING.md`, `workstreams/README.md`, `sdk/CHANGELOG.md`,
or any other workstream file.

## Tasks

- [ ] Write `BenchmarkParallelSinkContention` and confirm baseline contention is measurable
- [ ] Implement `fanInEventSink` with channel-based drain goroutine
- [ ] Update `StepEventSink` in `lockedSink` to return `fanInEventSink`
- [ ] Integrate fan-in close into `runParallelIterations` post-goroutine cleanup
- [ ] Write `TestFanInEventSink_AllEventsDelivered` under `-race`
- [ ] Confirm `BenchmarkParallelSinkContention_WithFanIn` shows improvement

## Exit criteria

- `go test -race ./internal/engine/...` passes.
- `BenchmarkParallelSinkContention_WithFanIn` shows ≥ 2× throughput vs the
  shared-mutex baseline at `parallel_max = 8` with a high-log-volume adapter.
- `TestFanInEventSink_AllEventsDelivered` verifies zero log event loss under
  concurrent sends.
- `make test` passes.

---

> **Deferral note:** This workstream is intentionally deferred until after
> parallel-01 and parallel-02 land. Do not begin implementation until a
> profiling trace demonstrates that sink lock contention is a measurable
> bottleneck in a real workflow run.
