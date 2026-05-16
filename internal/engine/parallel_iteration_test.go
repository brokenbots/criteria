package engine

// parallel_iteration_test.go — W19 tests for the parallel step modifier engine
// implementation: concurrency, bounded fan-out, on_failure semantics, output
// aggregation, context cancellation.

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapterhost"
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

// barrierAdapter blocks until N goroutines have reached Execute, then releases
// all at once. Used to assert that goroutines run concurrently.
type barrierAdapter struct {
	name    string
	barrier chan struct{}
	ready   int32
	n       int32
	outcome string
}

func newBarrierAdapter(name string, n int, outcome string) *barrierAdapter {
	return &barrierAdapter{
		name:    name,
		barrier: make(chan struct{}),
		n:       int32(n),
		outcome: outcome,
	}
}

func (p *barrierAdapter) Info(context.Context) (adapterhost.Info, error) {
	return adapterhost.Info{Name: p.name, Version: "test", Capabilities: []string{"parallel_safe"}}, nil
}
func (p *barrierAdapter) OpenSession(context.Context, string, map[string]string) error { return nil }
func (p *barrierAdapter) Execute(ctx context.Context, _ string, _ *workflow.StepNode, _ adapter.EventSink) (adapter.Result, error) {
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
func (p *barrierAdapter) Permit(context.Context, string, string, bool, string) error { return nil }
func (p *barrierAdapter) CloseSession(context.Context, string) error                 { return nil }
func (p *barrierAdapter) Kill()                                                      {}

// concurrencyTrackingAdapter records the peak number of concurrent Execute calls.
type concurrencyTrackingAdapter struct {
	name          string
	outcome       string
	mu            *sync.Mutex
	active        *int
	peakActive    *int
	executionTime time.Duration
}

func (p *concurrencyTrackingAdapter) Info(context.Context) (adapterhost.Info, error) {
	return adapterhost.Info{Name: p.name, Version: "test", Capabilities: []string{"parallel_safe"}}, nil
}
func (p *concurrencyTrackingAdapter) OpenSession(context.Context, string, map[string]string) error {
	return nil
}
func (p *concurrencyTrackingAdapter) Execute(ctx context.Context, _ string, _ *workflow.StepNode, _ adapter.EventSink) (adapter.Result, error) {
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
func (p *concurrencyTrackingAdapter) Permit(context.Context, string, string, bool, string) error {
	return nil
}
func (p *concurrencyTrackingAdapter) CloseSession(context.Context, string) error { return nil }
func (p *concurrencyTrackingAdapter) Kill()                                      {}

// contextAwareAdapter calls fn with the goroutine-specific context and a
// monotonic call index. Safe for concurrent use.
type contextAwareAdapter struct {
	name      string
	fn        func(ctx context.Context, call int) (adapter.Result, error)
	callCount *int32
}

func (p *contextAwareAdapter) Info(context.Context) (adapterhost.Info, error) {
	return adapterhost.Info{Name: p.name, Version: "test", Capabilities: []string{"parallel_safe"}}, nil
}
func (p *contextAwareAdapter) OpenSession(context.Context, string, map[string]string) error {
	return nil
}
func (p *contextAwareAdapter) Execute(ctx context.Context, _ string, _ *workflow.StepNode, _ adapter.EventSink) (adapter.Result, error) {
	idx := int(atomic.AddInt32(p.callCount, 1)) - 1
	return p.fn(ctx, idx)
}
func (p *contextAwareAdapter) Permit(context.Context, string, string, bool, string) error {
	return nil
}
func (p *contextAwareAdapter) CloseSession(context.Context, string) error { return nil }
func (p *contextAwareAdapter) Kill()                                      {}

// parallelSafeAdapter is a fakeAdapter that declares the "parallel_safe" capability.
// Use this instead of fakeAdapter for parallel steps in tests.
type parallelSafeAdapter struct {
	name    string
	outcome string
	err     error
}

func (p *parallelSafeAdapter) Info(context.Context) (adapterhost.Info, error) {
	return adapterhost.Info{Name: p.name, Version: "test", Capabilities: []string{"parallel_safe"}}, nil
}

func (p *parallelSafeAdapter) OpenSession(context.Context, string, map[string]string) error {
	return nil
}
func (p *parallelSafeAdapter) Execute(_ context.Context, _ string, _ *workflow.StepNode, _ adapter.EventSink) (adapter.Result, error) {
	if p.err != nil {
		return adapter.Result{}, p.err
	}
	return adapter.Result{Outcome: p.outcome}, nil
}
func (p *parallelSafeAdapter) Permit(context.Context, string, string, bool, string) error { return nil }
func (p *parallelSafeAdapter) CloseSession(context.Context, string) error                 { return nil }
func (p *parallelSafeAdapter) Kill()                                                      {}

// --- Tests ---

// TestParallelIteration_DefaultMax_RunsConcurrently uses a barrier to assert
// that at least N goroutines reach Execute simultaneously (up to GOMAXPROCS).
func TestParallelIteration_DefaultMax_RunsConcurrently(t *testing.T) {
	n := 4
	if runtime.GOMAXPROCS(0) < n {
		t.Skipf("GOMAXPROCS=%d < %d; skip concurrency assertion", runtime.GOMAXPROCS(0), n)
	}
	barrier := newBarrierAdapter("fake", n, "success")
	g := compile(t, parallelWorkflowHCL(`
step "work" {
  target   = adapter.fake
  parallel = ["a", "b", "c", "d"]
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "failed" }
}`))
	sink := &parallelSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"fake": barrier}}
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

	p := &concurrencyTrackingAdapter{
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
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"fake": p}}
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
	p := &contextAwareAdapter{
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
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"fake": p}}
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
	p := &contextAwareAdapter{
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
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"fake": p}}
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

	p := &parallelSafeAdapter{name: "fake", outcome: "failure"}
	sink := &parallelSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"fake": p}}
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
// The step passes each._idx to the adapter via input. The adapter sleeps for
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

	// declIdxAdapter reads decl_idx from its input, sleeps (2-decl_idx)×5ms
	// so completion order is [2, 1, 0], then returns {idx: decl_idx_value}.
	workPlug := &declIdxAdapter{name: "fake_work"}

	var capturedInputs []map[string]string
	checkPlug := &captureInputAdapter{outcome: "success", capture: &capturedInputs}

	sink := &parallelSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
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

// declIdxAdapter reads input["decl_idx"], sleeps proportionally (reversed), and
// returns {idx: <decl_idx>} so the caller can verify declaration-order storage.
type declIdxAdapter struct{ name string }

func (p *declIdxAdapter) Info(context.Context) (adapterhost.Info, error) {
	return adapterhost.Info{Name: p.name, Version: "test", Capabilities: []string{"parallel_safe"}}, nil
}
func (p *declIdxAdapter) OpenSession(context.Context, string, map[string]string) error { return nil }
func (p *declIdxAdapter) Execute(_ context.Context, _ string, step *workflow.StepNode, _ adapter.EventSink) (adapter.Result, error) {
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
func (p *declIdxAdapter) Permit(context.Context, string, string, bool, string) error { return nil }
func (p *declIdxAdapter) CloseSession(context.Context, string) error                 { return nil }
func (p *declIdxAdapter) Kill()                                                      {}

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

	// Adapter blocks until ctx is cancelled.
	p := &contextAwareAdapter{
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
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"fake": p}}
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

	p := &parallelSafeAdapter{name: "fake", outcome: "success"}
	sink := &parallelSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"fake": p}}
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
	p := newBarrierAdapter("fake", 8, "success")
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"fake": p}}
	if err := New(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
}

// TestParallelIteration_AdapterEventSink_NoConcurrentRace verifies that the
// adapter.EventSink returned by StepEventSink is concurrency-safe under the
// race detector. Parallel adapter goroutines call Log concurrently; the
// fanInEventSink drain goroutine serializes writes to sharedLogSink under the
// shared mutex, so a DATA RACE on sharedLogSink.count would indicate that the
// fan-in path is not holding the lock correctly.
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

	// loggingBarrierAdapter waits for all n goroutines then emits a Log event
	// concurrently so the race detector can observe unsynchronized writes.
	p := newLoggingBarrierAdapter("fake", n, "success")
	sink := &sharedLogSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"fake": p}}
	if err := New(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if sink.count != n {
		t.Errorf("log count: got %d; want %d", sink.count, n)
	}
}

// loggingBarrierAdapter synchronizes all goroutines at a barrier then calls
// sink.Log from each, exercising concurrent adapter EventSink emission.
type loggingBarrierAdapter struct {
	name    string
	outcome string
	barrier chan struct{}
	ready   int32
	n       int32
}

func newLoggingBarrierAdapter(name string, n int, outcome string) *loggingBarrierAdapter {
	return &loggingBarrierAdapter{
		name:    name,
		outcome: outcome,
		barrier: make(chan struct{}),
		n:       int32(n),
	}
}

func (p *loggingBarrierAdapter) Info(context.Context) (adapterhost.Info, error) {
	return adapterhost.Info{Name: p.name, Version: "test", Capabilities: []string{"parallel_safe"}}, nil
}
func (p *loggingBarrierAdapter) OpenSession(context.Context, string, map[string]string) error {
	return nil
}
func (p *loggingBarrierAdapter) Execute(ctx context.Context, _ string, _ *workflow.StepNode, sink adapter.EventSink) (adapter.Result, error) {
	count := atomic.AddInt32(&p.ready, 1)
	if count == p.n {
		close(p.barrier)
	}
	select {
	case <-p.barrier:
	case <-ctx.Done():
		return adapter.Result{}, ctx.Err()
	}
	// All goroutines call Log concurrently — the fanInEventSink drain goroutine
	// must hold the shared mutex while writing to sharedLogSink.count, or the
	// race detector will fire.
	sink.Log("stdout", []byte("parallel"))
	return adapter.Result{Outcome: p.outcome}, nil
}
func (p *loggingBarrierAdapter) Permit(context.Context, string, string, bool, string) error {
	return nil
}
func (p *loggingBarrierAdapter) CloseSession(context.Context, string) error { return nil }
func (p *loggingBarrierAdapter) Kill()                                      {}

// sharedLogSink is a test Sink whose StepEventSink returns an EventSink that
// writes to a shared non-atomic counter. The counter is deliberately not
// atomic: concurrent calls to Log would produce a DATA RACE unless the
// fanInEventSink drain goroutine serializes writes under the shared mutex.
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

// --- Step-policy regression tests (W19 Review 2026-05-06-02) ---

// TestParallelIteration_MaxVisitsEnforced verifies that max_visits is shared
// across parallel goroutines. With max_visits=1 and 2 items, exactly one
// iteration can execute; the second exceeds the limit and fails, routing to
// "any_failed" (the "failed" terminal state). Before the fix, runParallelAdapterIteration
// called executeStep directly, skipping incrementVisit entirely, so both items
// succeeded and the run reached "done".
func TestParallelIteration_MaxVisitsEnforced(t *testing.T) {
	g := compile(t, parallelWorkflowHCL(`
step "work" {
  target     = adapter.fake
  parallel   = ["a", "b"]
  max_visits = 1
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "failed" }
}`))

	p := &parallelSafeAdapter{name: "fake", outcome: "success"}
	sink := &parallelSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"fake": p}}
	if err := New(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("unexpected run error: %v", err)
	}
	// max_visits=1 on a 2-item parallel step: the second iteration must fail.
	// The aggregate therefore routes to "any_failed" → terminal "failed", not "done".
	if sink.terminal == "done" && sink.terminalOK {
		t.Error("expected non-success terminal: max_visits=1 should prevent both parallel items from executing; got 'done'")
	}
}

// TestParallelIteration_TimeoutEnforced verifies that a per-step timeout is
// applied to each parallel adapter iteration. The adapter blocks indefinitely;
// with timeout="100ms" the iterations should be cancelled well before the
// natural completion time. Before the fix, runParallelAdapterIteration called
// executeStep directly without wrapping the context with a timeout, so the step
// ran to natural completion (~2s) ignoring the declared timeout.
func TestParallelIteration_TimeoutEnforced(t *testing.T) {
	g := compile(t, `
workflow "t" {
  version       = "0.1"
  initial_state = "work"
  target_state  = "done"
}
step "work" {
  target   = adapter.fake
  parallel = ["a", "b"]
  timeout  = "100ms"
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
`)

	// Adapter blocks until its context is cancelled; without timeout enforcement
	// it would run for the full parent-context lifetime (>> 100ms).
	p := &contextAwareAdapter{
		name: "fake",
		fn: func(ctx context.Context, _ int) (adapter.Result, error) {
			select {
			case <-time.After(2 * time.Second):
				return adapter.Result{Outcome: "success"}, nil
			case <-ctx.Done():
				return adapter.Result{}, ctx.Err()
			}
		},
		callCount: new(int32),
	}

	start := time.Now()
	sink := &parallelSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"fake": p}}
	if err := New(g, loader, sink).Run(context.Background()); err != nil {
		t.Logf("run error (expected due to timeout): %v", err)
	}
	elapsed := time.Since(start)

	// Should complete in roughly 100ms (step timeout), not 2s (natural completion).
	// Allow generous headroom for race-detector overhead.
	if elapsed > 1*time.Second {
		t.Errorf("run took %v; expected timeout to enforce ~100ms cancellation (not 2s)", elapsed)
	}
	// Timeout must cause a non-success outcome (not "done").
	if sink.terminal == "done" && sink.terminalOK {
		t.Error("expected non-success terminal: step timeout should have caused failure; got 'done'")
	}
}

// TestCtyOutputsToStrings_RenderFailurePropagated verifies that
// ctyOutputsToStrings returns an error when a subworkflow output value cannot
// be JSON-rendered, instead of silently substituting an empty string. Before the
// fix, renderCtyValue errors were discarded (result[k] = "") so the parallel
// subworkflow output path could silently lose output data and continue — weaker
// than the non-parallel evaluateSubworkflowStep path which returns an error.
//
// A capsule type wrapping a Go channel is used because encoding/json cannot
// serialize channel types, so renderCtyValue reliably returns an error for it.
func TestCtyOutputsToStrings_RenderFailurePropagated(t *testing.T) {
	type withChannel struct{ Ch chan int }
	capsuleType := cty.Capsule("withchan", reflect.TypeOf(withChannel{}))
	val := cty.CapsuleVal(capsuleType, &withChannel{Ch: make(chan int)})

	_, err := ctyOutputsToStrings("test_step", map[string]cty.Value{"out": val})
	if err == nil {
		t.Error("expected error from unrenderable capsule output value; got nil (silent failure)")
	}
}

// TestParallelIteration_SubworkflowOutputRenderErrorPropagated is an E2E test
// verifying that Engine.Run returns an error when a parallel subworkflow
// iteration produces an output value that cannot be JSON-rendered. Before the
// aggregateParallelResults fix, the error was downgraded to anyFailed=true and
// the engine would route to "any_failed" (or continue) instead of aborting.
//
// The callee subworkflow declares a single output whose value expression is a
// literal capsule wrapping a Go channel. encoding/json cannot serialize channel
// types, so renderCtyValue reliably errors.
func TestParallelIteration_SubworkflowOutputRenderErrorPropagated(t *testing.T) {
	type withChannel struct{ Ch chan int }
	capsuleType := cty.Capsule("withchan", reflect.TypeOf(withChannel{}))
	capsuleVal := cty.CapsuleVal(capsuleType, &withChannel{Ch: make(chan int)})

	// Callee subworkflow: one terminal state with an output whose value is the
	// unserializable capsule literal.
	calleeStep := &workflow.StepNode{
		Name: "done_step",
		// Immediately terminal — no adapter needed.
		TargetKind: workflow.StepTargetSubworkflow,
		Outcomes: map[string]*workflow.CompiledOutcome{
			"success": {Next: workflow.ReturnSentinel},
		},
	}
	// A tiny callee with a single terminal state reached directly (no real step).
	calleeDoneState := &workflow.StateNode{Name: "callee_done", Terminal: true, Success: true}
	_ = calleeStep

	calleeGraph := &workflow.FSMGraph{
		Name:         "callee",
		InitialState: "callee_done",
		TargetState:  "callee_done",
		Policy:       workflow.DefaultPolicy,
		Steps:        map[string]*workflow.StepNode{},
		States: map[string]*workflow.StateNode{
			"callee_done": calleeDoneState,
		},
		Adapters:     map[string]*workflow.AdapterNode{},
		Subworkflows: map[string]*workflow.SubworkflowNode{},
		Variables:    map[string]*workflow.VariableNode{},
		Environments: map[string]*workflow.EnvironmentNode{},
		Outputs: map[string]*workflow.OutputNode{
			"out": {
				Name:  "out",
				Value: &hclsyntax.LiteralValueExpr{Val: capsuleVal, SrcRange: hcl.Range{}},
			},
		},
		OutputOrder: []string{"out"},
	}

	swNode := &workflow.SubworkflowNode{
		Name:         "callee",
		Body:         calleeGraph,
		BodyEntry:    "callee_done",
		Inputs:       map[string]hcl.Expression{},
		DeclaredVars: map[string]*workflow.VariableNode{},
	}

	// Parent graph: one parallel subworkflow step over a single-element list.
	// parseExpr is defined in node_step_w15_test.go within package engine.
	parallelExpr := parseExpr(t, `["item"]`)
	parentStep := &workflow.StepNode{
		Name:           "call",
		TargetKind:     workflow.StepTargetSubworkflow,
		SubworkflowRef: "callee",
		Parallel:       parallelExpr,
		ParallelMax:    1,
		Outcomes: map[string]*workflow.CompiledOutcome{
			"all_succeeded": {Next: "done"},
			"any_failed":    {Next: "failed"},
		},
	}
	parentGraph := &workflow.FSMGraph{
		Name:         "parent",
		InitialState: "call",
		TargetState:  "done",
		Policy:       workflow.DefaultPolicy,
		Steps:        map[string]*workflow.StepNode{"call": parentStep},
		States: map[string]*workflow.StateNode{
			"done":   {Name: "done", Terminal: true, Success: true},
			"failed": {Name: "failed", Terminal: true, Success: false},
		},
		Adapters:     map[string]*workflow.AdapterNode{},
		Subworkflows: map[string]*workflow.SubworkflowNode{"callee": swNode},
		Variables:    map[string]*workflow.VariableNode{},
		Environments: map[string]*workflow.EnvironmentNode{},
	}

	sink := &parallelSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{}}
	err := New(parentGraph, loader, sink).Run(context.Background())
	if err == nil {
		t.Error("expected Engine.Run to return an error for unrenderable subworkflow output; got nil")
	}
}

// collapsed r.err into anyFailed so Engine.Run returned nil even for fatal errors.
func TestParallelIteration_FatalErrorPropagated(t *testing.T) {
	g := compile(t, parallelWorkflowHCL(`
step "work" {
  target   = adapter.fake
  parallel = ["a", "b"]
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "failed" }
}`))

	// Adapter returns a FatalRunError on every call.
	p := &contextAwareAdapter{
		name: "fake",
		fn: func(_ context.Context, _ int) (adapter.Result, error) {
			return adapter.Result{}, &adapterhost.FatalRunError{Err: fmt.Errorf("simulated fatal")}
		},
		callCount: new(int32),
	}

	sink := &parallelSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"fake": p}}
	// Fatal errors must surface as Engine.Run errors, not be silently routed
	// to "failed" terminal state.
	err := New(g, loader, sink).Run(context.Background())
	if err == nil {
		t.Error("expected Run to return a fatal error; got nil")
	}
	if sink.terminal == "done" && sink.terminalOK {
		t.Error("expected non-success terminal: fatal adapter error should not reach 'done'")
	}
}

// perResolveLoader is a adapterhost.Loader that creates a fresh statefulAdapter on
// every Resolve call, matching the production Loader contract ("Multiple calls
// with the same name return distinct Adapter handles — one per session"). All
// adapter instances share a rendezvous barrier and an open-session counter
// through the loader struct so that assertions can be made at the loader level.
type perResolveLoader struct {
	opens   atomic.Int32
	ready   atomic.Int32
	n       int32
	barrier chan struct{}
	delay   time.Duration
}

func (l *perResolveLoader) Resolve(_ context.Context, name string) (adapterhost.Handle, error) {
	return &statefulAdapter{loader: l, name: name}, nil
}
func (l *perResolveLoader) Shutdown(context.Context) error { return nil }

// statefulAdapter models a stateful adapter process with a per-instance execution
// mutex. Each instance is returned by exactly one perResolveLoader.Resolve call.
//
// The per-instance execMu is the key to the regression test: when goroutines
// share a single session (old broken behaviour) they all receive the same adapter
// instance from the session manager and must serialize behind its execMu,
// producing ≈ N × delay wall time. When each goroutine holds its own session
// (correct behaviour) each holds an independent instance and all N sleeps
// overlap, producing ≈ 1 × delay wall time.
type statefulAdapter struct {
	loader *perResolveLoader
	name   string
	execMu sync.Mutex // per-instance; models a Copilot-style per-session execution lock
}

func (p *statefulAdapter) Info(context.Context) (adapterhost.Info, error) {
	return adapterhost.Info{Name: p.name, Version: "test"}, nil
}
func (p *statefulAdapter) OpenSession(context.Context, string, map[string]string) error {
	p.loader.opens.Add(1)
	return nil
}
func (p *statefulAdapter) Execute(ctx context.Context, _ string, _ *workflow.StepNode, _ adapter.EventSink) (adapter.Result, error) {
	// Shared rendezvous: all n goroutines must reach Execute before any proceeds.
	// This ensures the per-instance mutex contention (or absence thereof) is the
	// sole source of timing difference between the broken and fixed implementations.
	if p.loader.ready.Add(1) == p.loader.n {
		close(p.loader.barrier)
	}
	select {
	case <-p.loader.barrier:
	case <-ctx.Done():
		return adapter.Result{}, ctx.Err()
	}
	// Per-instance execution lock: models a stateful adapter (e.g. Copilot) whose
	// internal session mutex prevents concurrent dispatches on the same session handle.
	// With isolated sessions each goroutine acquires its own instance's mutex and
	// sleeps concurrently (≈ 1 × delay). When sessions are shared all goroutines
	// contend on the same instance's mutex and serialise (≈ N × delay).
	p.execMu.Lock()
	defer p.execMu.Unlock()
	select {
	case <-time.After(p.loader.delay):
	case <-ctx.Done():
		return adapter.Result{}, ctx.Err()
	}
	return adapter.Result{Outcome: "success"}, nil
}
func (p *statefulAdapter) Permit(context.Context, string, string, bool, string) error { return nil }
func (p *statefulAdapter) CloseSession(context.Context, string) error                 { return nil }
func (p *statefulAdapter) Kill()                                                      {}

// TestParallelSubworkflow_IsolatedSessions_ConcurrentExecution verifies that
// parallel subworkflow iterations each receive a distinct adapter session (W19
// isolation fix) and execute concurrently.
//
// Acceptance criteria:
//  1. adapterhost.OpenSession is called N times (once per iteration, not once total).
//  2. N iterations complete in ≤ 2×execDelay (concurrent, not serial).
//  3. No data race under -race.
//
// Regression-sensitivity: without the per-iteration SessionManager fix, all
// goroutines share one session → one adapter instance → all N Execute calls
// contend on the same execMu → total wall time ≈ N×execDelay > 2×execDelay,
// failing assertion 2. The perResolveLoader returns a distinct statefulAdapter
// per Resolve call, matching production Loader semantics (one handle per session).
func TestParallelSubworkflow_IsolatedSessions_ConcurrentExecution(t *testing.T) {
	const (
		n         = 3
		execDelay = 60 * time.Millisecond
		maxTotal  = 2 * execDelay // ≈ 120ms; 3 serial executions take ≈ 180ms
	)

	loader := &perResolveLoader{
		n:       n,
		barrier: make(chan struct{}),
		delay:   execDelay,
	}

	// Callee subworkflow: declares adapter "noop" and runs one step with it.
	calleeBody := calleeBodyWithStep("noop")
	swNode := subworkflowNodeFor("callee", calleeBody)

	// Parent graph: parallel subworkflow step over an n-element list.
	parallelExpr := parseExpr(t, `["a", "b", "c"]`)
	parentStep := &workflow.StepNode{
		Name:           "call",
		TargetKind:     workflow.StepTargetSubworkflow,
		SubworkflowRef: "callee",
		Parallel:       parallelExpr,
		ParallelMax:    n,
		Outcomes: map[string]*workflow.CompiledOutcome{
			"all_succeeded": {Next: "done"},
			"any_failed":    {Next: "failed"},
		},
	}
	parentGraph := &workflow.FSMGraph{
		Name:         "parent",
		InitialState: "call",
		TargetState:  "done",
		Policy:       workflow.DefaultPolicy,
		Steps:        map[string]*workflow.StepNode{"call": parentStep},
		States: map[string]*workflow.StateNode{
			"done":   {Name: "done", Terminal: true, Success: true},
			"failed": {Name: "failed", Terminal: true, Success: false},
		},
		Adapters:     map[string]*workflow.AdapterNode{},
		Subworkflows: map[string]*workflow.SubworkflowNode{"callee": swNode},
		Variables:    map[string]*workflow.VariableNode{},
		Environments: map[string]*workflow.EnvironmentNode{},
	}

	sink := &parallelSink{}
	start := time.Now()
	if err := New(parentGraph, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	elapsed := time.Since(start)

	// Assertion 1: each iteration opened its own session (N opens, not 1).
	// With the old shared-session bug, only goroutine 0 calls Resolve/OpenSession;
	// goroutines 1..N-1 hit ErrSessionAlreadyOpen which is swallowed, so opens == 1.
	if got := loader.opens.Load(); got != n {
		t.Errorf("OpenSession call count = %d; want %d (each parallel iteration must open its own session)", got, n)
	}

	// Assertion 2: all iterations completed in ≤ 2×execDelay (concurrent, not serial).
	// Three serial executions would take ≈ 3×execDelay ≈ 180ms, well above the 120ms cap.
	if elapsed > maxTotal {
		t.Errorf("elapsed %v > %v (2×execDelay=%v); iterations appear to have serialized — per-iteration session isolation may be broken", elapsed, maxTotal, execDelay)
	}

	if sink.terminal != "done" || !sink.terminalOK {
		t.Errorf("terminal state: got %q (ok=%v); want \"done\" (true)", sink.terminal, sink.terminalOK)
	}
}

// countingNotSafeAdapter counts Execute calls and does NOT declare "parallel_safe".
// Use in tests that verify the runtime gate fires before any iteration executes.
type countingNotSafeAdapter struct {
	name         string
	outcome      string
	executeCount int32
}

func (p *countingNotSafeAdapter) Info(context.Context) (adapterhost.Info, error) {
	// Deliberately no Capabilities: parallel_safe — this adapter is not safe.
	return adapterhost.Info{Name: p.name, Version: "test"}, nil
}
func (p *countingNotSafeAdapter) OpenSession(context.Context, string, map[string]string) error {
	return nil
}
func (p *countingNotSafeAdapter) Execute(_ context.Context, _ string, _ *workflow.StepNode, _ adapter.EventSink) (adapter.Result, error) {
	atomic.AddInt32(&p.executeCount, 1)
	return adapter.Result{Outcome: p.outcome}, nil
}
func (p *countingNotSafeAdapter) Permit(context.Context, string, string, bool, string) error {
	return nil
}
func (p *countingNotSafeAdapter) CloseSession(context.Context, string) error { return nil }
func (p *countingNotSafeAdapter) Kill()                                      {}

// TestEvaluateParallel_AdapterNotParallelSafe_RuntimeError verifies that when
// an adapter step with parallel = [...] is backed by a session whose adapter
// does NOT declare "parallel_safe", the runtime gate rejects execution with a
// clear error mentioning "parallel_safe" and before any Execute call is made.
func TestEvaluateParallel_AdapterNotParallelSafe_RuntimeError(t *testing.T) {
	g := compile(t, parallelWorkflowHCL(`
step "work" {
  target   = adapter.fake
  parallel = ["a", "b"]
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "failed" }
}`))

	// countingNotSafeAdapter does not declare "parallel_safe" and counts Execute
	// calls so we can assert zero iterations executed when the gate fires.
	p := &countingNotSafeAdapter{name: "fake", outcome: "success"}
	sink := &parallelSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"fake": p}}
	err := New(g, loader, sink).Run(context.Background())
	if err == nil {
		t.Fatal("expected error from adapter missing parallel_safe; got nil")
	}
	if !strings.Contains(err.Error(), "parallel_safe") {
		t.Errorf("error = %q; want mention of 'parallel_safe'", err.Error())
	}
	// The gate must fire before any iteration executes.
	if got := atomic.LoadInt32(&p.executeCount); got != 0 {
		t.Errorf("Execute called %d time(s); want 0 (gate must fire before any iteration)", got)
	}
	// No iteration-entered or iteration-completed events should have fired.
	if len(sink.iterationsStarted) != 0 {
		t.Errorf("iterationsStarted = %d; want 0", len(sink.iterationsStarted))
	}
	if len(sink.iterationsCompleted) != 0 {
		t.Errorf("iterationsCompleted = %d; want 0", len(sink.iterationsCompleted))
	}
}

// TestEvaluateParallel_AdapterParallelSafe_Runs verifies that when an adapter
// step with parallel = [...] is backed by a session whose adapter declares
// "parallel_safe", execution proceeds without error.
func TestEvaluateParallel_AdapterParallelSafe_Runs(t *testing.T) {
	g := compile(t, parallelWorkflowHCL(`
step "work" {
  target   = adapter.fake
  parallel = ["a", "b"]
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "failed" }
}`))

	p := &parallelSafeAdapter{name: "fake", outcome: "success"}
	sink := &parallelSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"fake": p}}
	if err := New(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("expected success; got error: %v", err)
	}
	if sink.terminal != "done" || !sink.terminalOK {
		t.Errorf("terminal: %s ok=%v; want done/true", sink.terminal, sink.terminalOK)
	}
}

// --- fanInEventSink unit tests ---

// fanInCountSink is a shared counting adapter.EventSink for unit tests.
// Its counters must only be written under the shared fanInEventSink mutex;
// the race detector will fire if that guarantee is ever violated.
type fanInCountSink struct {
	logCount        int // non-atomic by design; safe only under the shared mutex
	adapterCount    int
	lastAdapterData any // most recent data argument to Adapter()
}

func (s *fanInCountSink) Log(string, []byte)      { s.logCount++ }
func (s *fanInCountSink) Adapter(_ string, d any) { s.adapterCount++; s.lastAdapterData = d }

// TestFanInEventSink_AllEventsDelivered verifies that all Log and Adapter events
// sent from concurrent goroutines are delivered to the inner sink exactly once,
// with no events dropped or duplicated. The inner sink's non-atomic counters
// expose any concurrency violation to the race detector.
func TestFanInEventSink_AllEventsDelivered(t *testing.T) {
	const (
		numGoroutines        = 8
		logsPerGoroutine     = 100
		adaptersPerGoroutine = 50
	)

	// All fanInEventSinks share one mutex — their drain goroutines serialize
	// writes to the non-atomic counters in fanInCountSink.
	var mu sync.Mutex
	inner := &fanInCountSink{}

	fans := make([]*fanInEventSink, numGoroutines)
	for i := range fans {
		fans[i] = newFanInEventSink(inner, &mu, 64)
	}

	chunk := []byte("hello world")
	var wg sync.WaitGroup
	for _, f := range fans {
		f := f
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < logsPerGoroutine; j++ {
				f.Log("stdout", chunk)
			}
			for j := 0; j < adaptersPerGoroutine; j++ {
				f.Adapter("test.event", j)
			}
		}()
	}
	wg.Wait()

	// Close all sinks — blocks until drain goroutines have flushed every event.
	for _, f := range fans {
		f.close()
	}

	wantLogs := numGoroutines * logsPerGoroutine
	wantAdapters := numGoroutines * adaptersPerGoroutine
	if inner.logCount != wantLogs {
		t.Errorf("Log deliveries: got %d; want %d", inner.logCount, wantLogs)
	}
	if inner.adapterCount != wantAdapters {
		t.Errorf("Adapter deliveries: got %d; want %d", inner.adapterCount, wantAdapters)
	}
}

// TestFanInEventSink_RaceDetector is an integration test that verifies the
// engine's fanInEventSink path passes -race with parallel_max=8 and a
// high-concurrency logging adapter. Complements the unit test by exercising
// the full engine path including lockedSink.closeEventSinks().
func TestFanInEventSink_RaceDetector(t *testing.T) {
	const n = 8
	g := compile(t, parallelWorkflowHCL(`
step "work" {
  target       = adapter.fake
  parallel     = ["a", "b", "c", "d", "e", "f", "g", "h"]
  parallel_max = 8
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "failed" }
}`))

	p := newLoggingBarrierAdapter("fake", n, "success")
	sink := &sharedLogSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"fake": p}}
	if err := New(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if sink.count != n {
		t.Errorf("log count: got %d; want %d (fanInEventSink must deliver all events)", sink.count, n)
	}
}

// TestFanInEventSink_AdapterPayloadSafety verifies that Adapter events are not
// affected by mutations to the map passed to Adapter() after the call returns.
// fanInEventSink.Adapter() must snapshot (shallow-copy) map[string]any data
// before enqueuing it so that post-call mutations by the caller are invisible
// to the drain goroutine.
func TestFanInEventSink_AdapterPayloadSafety(t *testing.T) {
	inner := &fanInCountSink{}
	var mu sync.Mutex
	f := newFanInEventSink(inner, &mu, 64)

	// Build a mutable map and enqueue it via Adapter.
	payload := map[string]any{"key": "original", "num": 42}
	f.Adapter("test.event", payload)

	// Mutate the map immediately — before the drain goroutine has processed it.
	payload["key"] = "mutated"
	payload["num"] = 999

	// Drain completely. After close() returns, all events are delivered.
	f.close()

	got, ok := inner.lastAdapterData.(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any delivered to Adapter; got %T", inner.lastAdapterData)
	}
	if got["key"] != "original" {
		t.Errorf("Adapter payload key: got %q; want %q — copy not taken at enqueue time", got["key"], "original")
	}
	if got["num"] != 42 {
		t.Errorf("Adapter payload num: got %v; want 42 — copy not taken at enqueue time", got["num"])
	}
}

// TestRunParallelIterations_DrainBeforeReturn verifies that the engine does not
// return from a parallel step until all fanInEventSink drain goroutines have
// finished delivering their buffered events. This catches any regression where
// closeEventSinks() is moved outside runParallelIterations: with a slow-writing
// sink, events still in the channel buffer would not yet be counted when the
// caller checks.
func TestRunParallelIterations_DrainBeforeReturn(t *testing.T) {
	const (
		numItems    = 4
		logsPerItem = 10
		// writeDelay simulates gRPC/IO write latency. With parallel_max=4 and
		// 10 log calls per adapter, a 200µs delay ensures drain goroutines are
		// still working when the goroutine phase (wg.Wait) finishes if drain is
		// not awaited inside runParallelIterations.
		writeDelay = 200 * time.Microsecond
	)

	g := compile(t, parallelWorkflowHCL(`
step "work" {
  target       = adapter.fake
  parallel     = ["a", "b", "c", "d"]
  parallel_max = 4
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "failed" }
}`))

	p := &slowLogAdapter{name: "fake", logsPerCall: logsPerItem}
	sink := &slowCountingSink{delay: writeDelay}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"fake": p}}

	if err := New(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	// All events must have been delivered before Run returns; if closeEventSinks
	// is not awaited inside runParallelIterations, count will be < want.
	want := numItems * logsPerItem
	sink.mu.Lock()
	got := sink.count
	sink.mu.Unlock()
	if got != want {
		t.Errorf("event count after Run: got %d; want %d — drain not complete before runParallelIterations returned", got, want)
	}
}

// slowLogAdapter calls sink.Log logsPerCall times per Execute call and succeeds.
// It declares parallel_safe so the engine assigns it a fanInEventSink.
type slowLogAdapter struct {
	name        string
	logsPerCall int
}

func (p *slowLogAdapter) Info(context.Context) (adapterhost.Info, error) {
	return adapterhost.Info{Name: p.name, Version: "test", Capabilities: []string{"parallel_safe"}}, nil
}
func (p *slowLogAdapter) OpenSession(context.Context, string, map[string]string) error { return nil }
func (p *slowLogAdapter) Execute(_ context.Context, _ string, _ *workflow.StepNode, sink adapter.EventSink) (adapter.Result, error) {
	chunk := []byte("x")
	for i := 0; i < p.logsPerCall; i++ {
		sink.Log("stdout", chunk)
	}
	return adapter.Result{Outcome: "success"}, nil
}
func (p *slowLogAdapter) Permit(context.Context, string, string, bool, string) error { return nil }
func (p *slowLogAdapter) CloseSession(context.Context, string) error                 { return nil }
func (p *slowLogAdapter) Kill()                                                      {}

// slowCountingSink is a Sink whose StepEventSink-produced EventSink sleeps
// writeDelay on every Log call. This models gRPC/IO write latency and exposes
// any regression where drain goroutines are not awaited inside
// runParallelIterations: buffered events would be uncounted when Run returns.
type slowCountingSink struct {
	fakeSink
	delay time.Duration
	mu    sync.Mutex
	count int
}

func (s *slowCountingSink) StepEventSink(step string) adapter.EventSink {
	return &slowCountingEventSink{parent: s}
}

type slowCountingEventSink struct {
	parent *slowCountingSink
}

func (e *slowCountingEventSink) Log(_ string, _ []byte) {
	if e.parent.delay > 0 {
		time.Sleep(e.parent.delay)
	}
	e.parent.mu.Lock()
	e.parent.count++
	e.parent.mu.Unlock()
}
func (e *slowCountingEventSink) Adapter(string, any) {}
