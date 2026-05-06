package engine

// parallel_iteration_test.go — W19 tests for the parallel step modifier engine
// implementation: concurrency, bounded fan-out, on_failure semantics, output
// aggregation, context cancellation.

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/plugin"
	"github.com/brokenbots/criteria/workflow"
)

// parallelSink extends iterSink to also track step-entered events for
// concurrency assertions.
type parallelSink struct {
	iterSink
}

// parallelWorkflowHCL wraps HCL step definitions in a minimal workflow scaffold.
// Uses "adapter.fake" (bare), which compile() auto-injects as adapter "fake" "default".
func parallelWorkflowHCL(steps string) string {
	return `
workflow "t" {
  version       = "0.1"
  initial_state = "work"
  target_state  = "done"
}
` + steps + `
state "done" {
  terminal = true
  success  = true
}
state "failed" {
  terminal = true
  success  = false
}
`
}

// barrierPlugin blocks until N goroutines have reached Execute, then releases
// all at once. Used to assert that goroutines run concurrently.
type barrierPlugin struct {
	name    string
	barrier chan struct{}
	ready   int32
	n       int32
	outcome string
}

func newBarrierPlugin(name string, n int, outcome string) *barrierPlugin {
	return &barrierPlugin{
		name:    name,
		barrier: make(chan struct{}),
		n:       int32(n),
		outcome: outcome,
	}
}

func (p *barrierPlugin) Info(context.Context) (plugin.Info, error) {
	return plugin.Info{Name: p.name, Version: "test"}, nil
}
func (p *barrierPlugin) OpenSession(context.Context, string, map[string]string) error { return nil }
func (p *barrierPlugin) Execute(ctx context.Context, _ string, _ *workflow.StepNode, _ adapter.EventSink) (adapter.Result, error) {
	count := atomic.AddInt32(&p.ready, 1)
	if count == p.n {
		close(p.barrier) // release all waiting goroutines
	}
	select {
	case <-p.barrier:
		return adapter.Result{Outcome: p.outcome}, nil
	case <-ctx.Done():
		return adapter.Result{}, ctx.Err()
	}
}
func (p *barrierPlugin) Permit(context.Context, string, string, bool, string) error { return nil }
func (p *barrierPlugin) CloseSession(context.Context, string) error                 { return nil }
func (p *barrierPlugin) Kill()                                                      {}

// concurrencyTrackingPlugin records the peak number of concurrent Execute calls.
type concurrencyTrackingPlugin struct {
	name          string
	outcome       string
	mu            *sync.Mutex
	active        *int
	peakActive    *int
	executionTime time.Duration
}

func (p *concurrencyTrackingPlugin) Info(context.Context) (plugin.Info, error) {
	return plugin.Info{Name: p.name, Version: "test"}, nil
}
func (p *concurrencyTrackingPlugin) OpenSession(context.Context, string, map[string]string) error {
	return nil
}
func (p *concurrencyTrackingPlugin) Execute(ctx context.Context, _ string, _ *workflow.StepNode, _ adapter.EventSink) (adapter.Result, error) {
	p.mu.Lock()
	*p.active++
	if *p.active > *p.peakActive {
		*p.peakActive = *p.active
	}
	p.mu.Unlock()

	select {
	case <-time.After(p.executionTime):
	case <-ctx.Done():
	}

	p.mu.Lock()
	*p.active--
	p.mu.Unlock()

	return adapter.Result{Outcome: p.outcome}, nil
}
func (p *concurrencyTrackingPlugin) Permit(context.Context, string, string, bool, string) error {
	return nil
}
func (p *concurrencyTrackingPlugin) CloseSession(context.Context, string) error { return nil }
func (p *concurrencyTrackingPlugin) Kill()                                      {}

// contextAwarePlugin calls fn with the goroutine-specific context and a
// monotonic call index. Safe for concurrent use.
type contextAwarePlugin struct {
	name      string
	fn        func(ctx context.Context, call int) (adapter.Result, error)
	callCount *int32
}

func (p *contextAwarePlugin) Info(context.Context) (plugin.Info, error) {
	return plugin.Info{Name: p.name, Version: "test"}, nil
}
func (p *contextAwarePlugin) OpenSession(context.Context, string, map[string]string) error {
	return nil
}
func (p *contextAwarePlugin) Execute(ctx context.Context, _ string, _ *workflow.StepNode, _ adapter.EventSink) (adapter.Result, error) {
	idx := int(atomic.AddInt32(p.callCount, 1)) - 1
	return p.fn(ctx, idx)
}
func (p *contextAwarePlugin) Permit(context.Context, string, string, bool, string) error {
	return nil
}
func (p *contextAwarePlugin) CloseSession(context.Context, string) error { return nil }
func (p *contextAwarePlugin) Kill()                                      {}

// --- Tests ---

// TestParallelIteration_DefaultMax_RunsConcurrently uses a barrier to assert
// that at least N goroutines reach Execute simultaneously (up to GOMAXPROCS).
func TestParallelIteration_DefaultMax_RunsConcurrently(t *testing.T) {
	n := 4
	if runtime.GOMAXPROCS(0) < n {
		t.Skipf("GOMAXPROCS=%d < %d; skip concurrency assertion", runtime.GOMAXPROCS(0), n)
	}
	barrier := newBarrierPlugin("fake", n, "success")
	g := compile(t, parallelWorkflowHCL(`
step "work" {
  target   = adapter.fake
  parallel = ["a", "b", "c", "d"]
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "failed" }
}`))
	sink := &parallelSink{}
	loader := &fakeLoader{plugins: map[string]plugin.Plugin{"fake": barrier}}
	if err := New(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(sink.iterationsCompleted) != 1 {
		t.Fatalf("iterations completed: got %d want 1", len(sink.iterationsCompleted))
	}
	if sink.iterationsCompleted[0].outcome != "all_succeeded" {
		t.Errorf("aggregate: got %q want 'all_succeeded'", sink.iterationsCompleted[0].outcome)
	}
}

// TestParallelIteration_BoundedByParallelMax verifies that at most parallel_max
// goroutines are active at any time.
func TestParallelIteration_BoundedByParallelMax(t *testing.T) {
	const maxConcurrent = 2
	var (
		mu         sync.Mutex
		active     int
		peakActive int
	)

	p := &concurrencyTrackingPlugin{
		name:          "fake",
		outcome:       "success",
		mu:            &mu,
		active:        &active,
		peakActive:    &peakActive,
		executionTime: 10 * time.Millisecond,
	}

	g := compile(t, parallelWorkflowHCL(`
step "work" {
  target       = adapter.fake
  parallel     = ["a", "b", "c", "d", "e", "f"]
  parallel_max = 2
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "failed" }
}`))
	sink := &parallelSink{}
	loader := &fakeLoader{plugins: map[string]plugin.Plugin{"fake": p}}
	if err := New(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	mu.Lock()
	peak := peakActive
	mu.Unlock()

	if peak > maxConcurrent {
		t.Errorf("peak concurrent executions = %d; want <= %d (parallel_max)", peak, maxConcurrent)
	}
}

// TestParallelIteration_AbortOnFirstFailure verifies that abort mode (default)
// cancels remaining goroutines when the first iteration fails.
func TestParallelIteration_AbortOnFirstFailure(t *testing.T) {
	g := compile(t, parallelWorkflowHCL(`
step "work" {
  target     = adapter.fake
  parallel   = ["a", "b", "c", "d"]
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "failed" }
}`))

	// First goroutine to execute fails immediately; the rest block until
	// cancelled via context.
	var callCount int32
	p := &contextAwarePlugin{
		name: "fake",
		fn: func(ctx context.Context, call int) (adapter.Result, error) {
			if call == 0 {
				return adapter.Result{Outcome: "failure"}, nil
			}
			<-ctx.Done()
			return adapter.Result{}, ctx.Err()
		},
		callCount: &callCount,
	}

	sink := &parallelSink{}
	loader := &fakeLoader{plugins: map[string]plugin.Plugin{"fake": p}}
	if err := New(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	if len(sink.iterationsCompleted) != 1 {
		t.Fatalf("iterations completed: %d; want 1", len(sink.iterationsCompleted))
	}
	if sink.iterationsCompleted[0].outcome != "any_failed" {
		t.Errorf("aggregate: got %q; want 'any_failed'", sink.iterationsCompleted[0].outcome)
	}
}

// TestParallelIteration_ContinueOnFailure verifies that continue mode collects
// all results even when some iterations fail.
func TestParallelIteration_ContinueOnFailure(t *testing.T) {
	g := compile(t, parallelWorkflowHCL(`
step "work" {
  target     = adapter.fake
  parallel   = ["a", "b", "c"]
  on_failure = "continue"
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "failed" }
}`))

	var callCount int32
	p := &contextAwarePlugin{
		name: "fake",
		fn: func(ctx context.Context, call int) (adapter.Result, error) {
			if call == 1 {
				return adapter.Result{Outcome: "failure"}, nil
			}
			return adapter.Result{Outcome: "success"}, nil
		},
		callCount: &callCount,
	}

	sink := &parallelSink{}
	loader := &fakeLoader{plugins: map[string]plugin.Plugin{"fake": p}}
	if err := New(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	if len(sink.iterationsCompleted) != 1 {
		t.Fatalf("iterations completed: %d; want 1", len(sink.iterationsCompleted))
	}
	if sink.iterationsCompleted[0].outcome != "any_failed" {
		t.Errorf("aggregate: got %q; want 'any_failed'", sink.iterationsCompleted[0].outcome)
	}
	// All 3 iterations should have been attempted.
	if int(atomic.LoadInt32(&callCount)) != 3 {
		t.Errorf("calls: got %d; want 3 (continue mode must not abort)", int(atomic.LoadInt32(&callCount)))
	}
}

// TestParallelIteration_IgnoreOnFailure verifies that ignore mode always routes
// to all_succeeded regardless of per-iteration outcomes.
func TestParallelIteration_IgnoreOnFailure(t *testing.T) {
	g := compile(t, parallelWorkflowHCL(`
step "work" {
  target     = adapter.fake
  parallel   = ["a", "b"]
  on_failure = "ignore"
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "failed" }
}`))

	p := &fakePlugin{name: "fake", outcome: "failure"}
	sink := &parallelSink{}
	loader := &fakeLoader{plugins: map[string]plugin.Plugin{"fake": p}}
	if err := New(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	if len(sink.iterationsCompleted) != 1 {
		t.Fatalf("iterations completed: %d; want 1", len(sink.iterationsCompleted))
	}
	if sink.iterationsCompleted[0].outcome != "all_succeeded" {
		t.Errorf("aggregate: got %q; want 'all_succeeded' (ignore mode)", sink.iterationsCompleted[0].outcome)
	}
	if sink.terminal != "done" || !sink.terminalOK {
		t.Errorf("terminal: %s ok=%v; want done/true", sink.terminal, sink.terminalOK)
	}
}

// TestParallelIteration_OutputAggregationOrder verifies that indexed outputs
// are stored in declaration order regardless of goroutine completion order.
// The step passes each._idx to the plugin via input. The plugin sleeps for
// (2 - decl_idx) × 5ms so that iteration 2 completes first and iteration 0
// completes last. Outputs encode the declaration index; if aggregation stored
// them in completion order, steps.work[0].idx would be "2".
func TestParallelIteration_OutputAggregationOrder(t *testing.T) {
	g := compile(t, `
workflow "t" {
  version       = "0.1"
  initial_state = "work"
  target_state  = "done"
}
step "work" {
  target   = adapter.fake_work
  parallel = ["a", "b", "c"]
  input {
    decl_idx = "${each._idx}"
  }
  outcome "all_succeeded" { next = "check" }
  outcome "any_failed"    { next = "failed" }
}
step "check" {
  target = adapter.fake_check
  input {
    idx0 = steps.work[0].idx
    idx1 = steps.work[1].idx
    idx2 = steps.work[2].idx
  }
  outcome "success" { next = "done" }
}
state "done" {
  terminal = true
  success  = true
}
state "failed" {
  terminal = true
  success  = false
}
`)

	// declIdxPlugin reads decl_idx from its input, sleeps (2-decl_idx)×5ms
	// so completion order is [2, 1, 0], then returns {idx: decl_idx_value}.
	workPlug := &declIdxPlugin{name: "fake_work"}

	var capturedInputs []map[string]string
	checkPlug := &captureInputPlugin{outcome: "success", capture: &capturedInputs}

	sink := &parallelSink{}
	loader := &fakeLoader{plugins: map[string]plugin.Plugin{
		"fake_work":  workPlug,
		"fake_check": checkPlug,
	}}
	if err := New(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	if len(capturedInputs) != 1 {
		t.Fatalf("check ran %d times; want 1", len(capturedInputs))
	}
	// Declaration order: work[0]→"0", work[1]→"1", work[2]→"2".
	// Completion order would give work[0]→"2", work[1]→"1", work[2]→"0".
	inp := capturedInputs[0]
	if inp["idx0"] != "0" {
		t.Errorf("steps.work[0].idx = %q; want \"0\" (declaration order, not completion order)", inp["idx0"])
	}
	if inp["idx1"] != "1" {
		t.Errorf("steps.work[1].idx = %q; want \"1\"", inp["idx1"])
	}
	if inp["idx2"] != "2" {
		t.Errorf("steps.work[2].idx = %q; want \"2\"", inp["idx2"])
	}
}

// declIdxPlugin reads input["decl_idx"], sleeps proportionally (reversed), and
// returns {idx: <decl_idx>} so the caller can verify declaration-order storage.
type declIdxPlugin struct{ name string }

func (p *declIdxPlugin) Info(context.Context) (plugin.Info, error) {
	return plugin.Info{Name: p.name, Version: "test"}, nil
}
func (p *declIdxPlugin) OpenSession(context.Context, string, map[string]string) error { return nil }
func (p *declIdxPlugin) Execute(_ context.Context, _ string, step *workflow.StepNode, _ adapter.EventSink) (adapter.Result, error) {
	idx := step.Input["decl_idx"]
	// Sleep inversely proportional to declaration index so that later items finish first.
	switch idx {
	case "0":
		time.Sleep(15 * time.Millisecond)
	case "1":
		time.Sleep(8 * time.Millisecond)
	default: // "2" and any others
		// no sleep — finishes first
	}
	return adapter.Result{Outcome: "success", Outputs: map[string]string{"idx": idx}}, nil
}
func (p *declIdxPlugin) Permit(context.Context, string, string, bool, string) error { return nil }
func (p *declIdxPlugin) CloseSession(context.Context, string) error                 { return nil }
func (p *declIdxPlugin) Kill()                                                      {}

// TestParallelIteration_ContextCancellation verifies that cancelling the parent
// context propagates to all in-flight parallel goroutines without leaking.
// Goroutine leak detection is handled by goleak.VerifyTestMain in main_test.go.
func TestParallelIteration_ContextCancellation(t *testing.T) {
	g := compile(t, parallelWorkflowHCL(`
step "work" {
  target   = adapter.fake
  parallel = ["a", "b", "c", "d"]
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "failed" }
}`))

	// Plugin blocks until ctx is cancelled.
	p := &contextAwarePlugin{
		name: "fake",
		fn: func(ctx context.Context, _ int) (adapter.Result, error) {
			<-ctx.Done()
			return adapter.Result{}, ctx.Err()
		},
		callCount: new(int32),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	sink := &parallelSink{}
	loader := &fakeLoader{plugins: map[string]plugin.Plugin{"fake": p}}
	err := New(g, loader, sink).Run(ctx)
	// Run should return an error (context deadline or cancellation).
	if err == nil {
		t.Fatal("expected error from cancelled context; got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Errorf("expected context error; got: %v", err)
	}
}

// TestParallelIteration_EmptyList verifies that a parallel step with an empty
// list emits all_succeeded immediately without running any iterations.
func TestParallelIteration_EmptyList(t *testing.T) {
	g := compile(t, parallelWorkflowHCL(`
step "work" {
  target   = adapter.fake
  parallel = []
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "failed" }
}`))

	p := &fakePlugin{name: "fake", outcome: "success"}
	sink := &parallelSink{}
	loader := &fakeLoader{plugins: map[string]plugin.Plugin{"fake": p}}
	if err := New(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	if len(sink.iterationsStarted) != 0 {
		t.Errorf("expected 0 iterations started; got %d", len(sink.iterationsStarted))
	}
	if len(sink.iterationsCompleted) != 1 {
		t.Fatalf("iterations completed: %d; want 1", len(sink.iterationsCompleted))
	}
	if sink.iterationsCompleted[0].outcome != "all_succeeded" {
		t.Errorf("aggregate: %q; want 'all_succeeded'", sink.iterationsCompleted[0].outcome)
	}
}

// TestParallelIteration_LockedSink_NoConcurrentRace verifies that lockedSink
// serialises ALL Sink methods under the race detector. Running 8 items with
// parallel_max = 8 (matching the barrier) maximizes goroutine concurrency.
// The test would produce a DATA RACE failure under `go test -race` if any
// Sink method were missing its mutex override.
func TestParallelIteration_LockedSink_NoConcurrentRace(t *testing.T) {
	g := compile(t, parallelWorkflowHCL(`
step "work" {
  target       = adapter.fake
  parallel     = ["a", "b", "c", "d", "e", "f", "g", "h"]
  parallel_max = 8
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "failed" }
}`))

	// Use parallelSink (which embeds fakeSink) as the outer sink.
	// The engine wraps it in lockedSink inside evaluateParallel.
	// parallel_max = 8 matches the barrier count so all goroutines reach
	// Execute simultaneously — maximizes the chance of catching a race.
	sink := &parallelSink{}
	p := newBarrierPlugin("fake", 8, "success")
	loader := &fakeLoader{plugins: map[string]plugin.Plugin{"fake": p}}
	if err := New(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
}

// TestParallelIteration_AdapterEventSink_NoConcurrentRace verifies that the
// adapter.EventSink returned by StepEventSink is also concurrency-safe under
// the race detector. Parallel adapter goroutines call Log concurrently; if
// lockedEventSink did not wrap the returned sink under the same mutex, the
// shared write inside sharedLogSink.Log would produce a DATA RACE.
func TestParallelIteration_AdapterEventSink_NoConcurrentRace(t *testing.T) {
	const n = 6
	g := compile(t, parallelWorkflowHCL(`
step "work" {
  target       = adapter.fake
  parallel     = ["a", "b", "c", "d", "e", "f"]
  parallel_max = 6
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "failed" }
}`))

	// loggingBarrierPlugin waits for all n goroutines then emits a Log event
	// concurrently so the race detector can observe unsynchronized writes.
	p := newLoggingBarrierPlugin("fake", n, "success")
	sink := &sharedLogSink{}
	loader := &fakeLoader{plugins: map[string]plugin.Plugin{"fake": p}}
	if err := New(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if sink.count != n {
		t.Errorf("log count: got %d; want %d", sink.count, n)
	}
}

// loggingBarrierPlugin synchronizes all goroutines at a barrier then calls
// sink.Log from each, exercising concurrent adapter EventSink emission.
type loggingBarrierPlugin struct {
	name    string
	outcome string
	barrier chan struct{}
	ready   int32
	n       int32
}

func newLoggingBarrierPlugin(name string, n int, outcome string) *loggingBarrierPlugin {
	return &loggingBarrierPlugin{
		name:    name,
		outcome: outcome,
		barrier: make(chan struct{}),
		n:       int32(n),
	}
}

func (p *loggingBarrierPlugin) Info(context.Context) (plugin.Info, error) {
	return plugin.Info{Name: p.name, Version: "test"}, nil
}
func (p *loggingBarrierPlugin) OpenSession(context.Context, string, map[string]string) error {
	return nil
}
func (p *loggingBarrierPlugin) Execute(ctx context.Context, _ string, _ *workflow.StepNode, sink adapter.EventSink) (adapter.Result, error) {
	count := atomic.AddInt32(&p.ready, 1)
	if count == p.n {
		close(p.barrier)
	}
	select {
	case <-p.barrier:
	case <-ctx.Done():
		return adapter.Result{}, ctx.Err()
	}
	// All goroutines call Log concurrently — races on sharedLogSink.count if
	// the EventSink is not wrapped by lockedEventSink.
	sink.Log("stdout", []byte("parallel"))
	return adapter.Result{Outcome: p.outcome}, nil
}
func (p *loggingBarrierPlugin) Permit(context.Context, string, string, bool, string) error {
	return nil
}
func (p *loggingBarrierPlugin) CloseSession(context.Context, string) error { return nil }
func (p *loggingBarrierPlugin) Kill()                                      {}

// sharedLogSink is a test Sink whose StepEventSink returns an EventSink that
// writes to a shared non-atomic counter. Without lockedEventSink, concurrent
// calls to Log from parallel goroutines produce a DATA RACE.
type sharedLogSink struct {
	fakeSink
	count int // deliberately non-atomic; safe only when Log is serialized
}

func (s *sharedLogSink) StepEventSink(string) adapter.EventSink {
	return &sharedLogEventSink{parent: s}
}

type sharedLogEventSink struct {
	parent *sharedLogSink
}

func (e *sharedLogEventSink) Log(string, []byte)  { e.parent.count++ }
func (e *sharedLogEventSink) Adapter(string, any) {}
