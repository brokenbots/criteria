package engine

// KB-25: a step blocked with no observable adapter progress for the
// configured stall window must fail the run with a typed stall error instead
// of wedging until an operator kills it. The CRI-275 step-timeout ceiling and
// the CRI-287 teardown-window semantics stay unchanged for steps that
// declare their own timeout, and a parent-initiated cancellation always wins
// over a stall.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	criteriav2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow"
)

// blockingAdapter blocks in Execute until its context is canceled, standing
// in for a wedged adapter call (KB-25).
type blockingAdapter struct {
	name        string
	entered     chan struct{}
	enteredOnce sync.Once
}

func newBlockingAdapter(name string) *blockingAdapter {
	return &blockingAdapter{name: name, entered: make(chan struct{})}
}

func (b *blockingAdapter) Info(context.Context) (adapterhost.Info, error) {
	return adapterhost.Info{Name: b.name, Version: "test"}, nil
}

func (b *blockingAdapter) OpenSession(context.Context, string, map[string]string, map[string]string) error {
	return nil
}

func (b *blockingAdapter) Execute(ctx context.Context, _ string, _ *workflow.StepNode, _ adapter.EventSink) (adapter.Result, error) {
	b.enteredOnce.Do(func() { close(b.entered) })
	<-ctx.Done()
	return adapter.Result{}, ctx.Err()
}

func (b *blockingAdapter) Permit(context.Context, string, string, bool, string) error { return nil }
func (b *blockingAdapter) CloseSession(context.Context, string) error                 { return nil }
func (b *blockingAdapter) Kill()                                                      {}
func (b *blockingAdapter) Pause(context.Context, string) error                        { return nil }
func (b *blockingAdapter) Resume(context.Context, string) error                       { return nil }
func (b *blockingAdapter) Inspect(context.Context, string) (*criteriav2.InspectResponse, error) {
	return &criteriav2.InspectResponse{}, nil
}
func (b *blockingAdapter) Snapshot(context.Context, string) (*criteriav2.SnapshotResponse, error) {
	return &criteriav2.SnapshotResponse{}, nil
}
func (b *blockingAdapter) Restore(context.Context, string, []byte, uint32) error { return nil }

// heartbeatLogStream implements LogStreamStarter by emitting heartbeats on
// the session's log stream while the step runs: real CRI-271 adapter
// activity that must keep the stall watchdog from firing.
type heartbeatLogStream struct {
	every time.Duration
}

func (h *heartbeatLogStream) StartLogStream(ctx context.Context, sessionID string, sink adapterhost.LogEventSink) (func(), <-chan error, error) {
	done := make(chan error, 1)
	go func() {
		defer close(done)
		t := time.NewTicker(h.every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := sink.Emit(&criteriav2.LogEvent{Heartbeat: &criteriav2.Heartbeat{}}); err != nil {
					return
				}
			}
		}
	}()
	return func() {}, done, nil
}

// heartbeatHandle pairs a blocking adapter Execute with a heartbeat-emitting
// log stream, standing in for a healthy but slow adapter (KB-25).
type heartbeatHandle struct {
	*blockingAdapter
	logs *heartbeatLogStream
}

func (h *heartbeatHandle) StartLogStream(ctx context.Context, sessionID string, sink adapterhost.LogEventSink) (func(), <-chan error, error) {
	return h.logs.StartLogStream(ctx, sessionID, sink)
}

// newStallTestDeps builds a graph whose boot step targets the wedge adapter,
// and a Deps with a SessionManager verified against it.
func newStallTestDeps(t *testing.T, handle adapterhost.Handle) (Deps, *workflow.FSMGraph, *workflow.StepNode) {
	t.Helper()
	g := compile(t, `
workflow {
  name = "stall"
  version = "0.1"
  initial_state = "boot"
  target_state  = "done"
}
adapter "wedge" "default" {}
step "boot" {
  target = adapter.wedge.default
  outcome "success" { next = step.finish }
  outcome "failure" { next = step.finish }
}
step "finish" {
  target = adapter.wedge.default
  outcome "success" { next = state.done }
}
state "done" {
  terminal = true
  success  = true
}
`)
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"wedge": handle}}
	sessions := adapterhost.NewSessionManager(loader)
	sessions.SetGraph(g)
	t.Cleanup(func() { _ = sessions.Shutdown(context.Background()) })
	if err := sessions.OpenWithOriginRefs(context.Background(), "wedge.default", "wedge", "", nil, nil, nil, t.TempDir()); err != nil {
		t.Fatalf("open wedge session: %v", err)
	}
	deps := Deps{Sessions: sessions, Sink: &fakeSink{}}
	return deps, g, g.Steps["boot"]
}

func TestStepStall_FailsRunWithTypedError(t *testing.T) {
	t.Setenv("CRITERIA_STEP_STALL_WINDOW", "40ms")
	deps, g, step := newStallTestDeps(t, newBlockingAdapter("wedge"))
	n := &stepNode{graph: g, step: step}

	// The watchdog cancels the wedged Execute on its own; the wall-clock
	// guard only catches a full regression to the wedged behavior.
	result, _, err := n.executeStepTimed(context.Background(), deps, step)

	if err == nil {
		t.Fatal("a wedged step must fail the run")
	}
	if !errors.Is(err, errStepStalled) {
		t.Fatalf("error must unwrap to the stall sentinel, got: %v", err)
	}
	var stall *StepStallError
	if !errors.As(err, &stall) {
		t.Fatalf("error must be a typed StepStallError, got: %v", err)
	}
	if stall.Step != "boot" {
		t.Errorf("stall step = %q, want %q", stall.Step, "boot")
	}
	if stall.Window != 40*time.Millisecond {
		t.Errorf("stall window = %s, want 40ms", stall.Window)
	}
	if stall.Idle < 40*time.Millisecond {
		t.Errorf("stall idle = %s, want >= 40ms", stall.Idle)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("stall must not be misreported as a timeout: %v", err)
	}
	var fatal *adapterhost.FatalRunError
	if !errors.As(err, &fatal) {
		t.Fatalf("stall must be run-fatal so the workflow fails fast, got: %v", err)
	}
	if result.Outcome != "" {
		t.Errorf("stalled step must not fabricate an outcome, got %q", result.Outcome)
	}
}

// TestStepStall_ProgressPreventsStall runs a wedged Execute whose session
// stays observable (heartbeat log stream): the watchdog must never fire, so
// the parent's own cancellation keeps its plain semantics.
func TestStepStall_ProgressPreventsStall(t *testing.T) {
	t.Setenv("CRITERIA_STEP_STALL_WINDOW", "2s")
	handle := &heartbeatHandle{
		blockingAdapter: newBlockingAdapter("wedge"),
		logs:            &heartbeatLogStream{every: 100 * time.Millisecond},
	}
	deps, g, step := newStallTestDeps(t, handle)
	n := &stepNode{graph: g, step: step}

	ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()
	_, _, err := n.executeStepTimed(ctx, deps, step)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("parent cancellation must surface as its own error, got: %v", err)
	}
	if errors.Is(err, errStepStalled) {
		t.Fatalf("a session showing heartbeat activity must never be reported stalled: %v", err)
	}
}

// TestStepStall_ParentCancelWins runs a wedged step under a parent deadline
// shorter than the stall window: the watchdog must exit without firing so
// the run teardown keeps its existing routing.
func TestStepStall_ParentCancelWins(t *testing.T) {
	t.Setenv("CRITERIA_STEP_STALL_WINDOW", "200ms")
	deps, g, step := newStallTestDeps(t, newBlockingAdapter("wedge"))
	n := &stepNode{graph: g, step: step}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, _, err := n.executeStepTimed(ctx, deps, step)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("parent cancellation must surface as its own error, got: %v", err)
	}
	if errors.Is(err, errStepStalled) {
		t.Fatalf("a parent-initiated cancellation must not be reported as a stall: %v", err)
	}
}

// TestStepStall_StepTimeoutCeilingUnchanged pins the CRI-275/CRI-287
// semantics for steps that declare their own timeout: the stall watchdog
// stays disarmed, the ceiling error is kept, and the CRI-287 teardown window
// still opens from the step-declared timeout.
func TestStepStall_StepTimeoutCeilingUnchanged(t *testing.T) {
	t.Setenv("CRITERIA_STEP_STALL_WINDOW", "40ms")
	deps, g, step := newStallTestDeps(t, newBlockingAdapter("wedge"))
	timedStep := *step
	timedStep.Timeout = 50 * time.Millisecond
	n := &stepNode{graph: g, step: &timedStep}

	_, _, err := n.executeStepTimed(context.Background(), deps, &timedStep)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a step-declared timeout must surface as the CRI-275 ceiling error, got: %v", err)
	}
	if errors.Is(err, errStepStalled) {
		t.Fatalf("a step-declared timeout must never be reported as a stall: %v", err)
	}
	if !deps.Sessions.StepTimeoutTeardownWindowOpen() {
		t.Fatal("the CRI-287 teardown window must open for the step-declared timeout")
	}
}

func TestStepStallWindowFromEnv(t *testing.T) {
	cases := []struct {
		name  string
		value string
		set   bool
		want  time.Duration
	}{
		{"unset keeps default", "", false, 30 * time.Minute},
		{"configured", "45m", true, 45 * time.Minute},
		{"zero disables", "0", true, 0},
		{"negative disables", "-5m", true, 0},
		{"malformed keeps default", "garbage", true, 30 * time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv("CRITERIA_STEP_STALL_WINDOW", tc.value)
			}
			if got := stepStallWindowFromEnv(); got != tc.want {
				t.Errorf("stepStallWindowFromEnv() = %s, want %s", got, tc.want)
			}
		})
	}
}

// TestStepStall_DisabledWindow pins that CRITERIA_STEP_STALL_WINDOW=0 fully
// disarms stall detection: a wedged step is torn down only by its parent.
func TestStepStall_DisabledWindow(t *testing.T) {
	t.Setenv("CRITERIA_STEP_STALL_WINDOW", "0")
	deps, g, step := newStallTestDeps(t, newBlockingAdapter("wedge"))
	n := &stepNode{graph: g, step: step}

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	_, _, err := n.executeStepTimed(ctx, deps, step)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("disabled stall window must keep plain parent-cancellation semantics, got: %v", err)
	}
	if errors.Is(err, errStepStalled) {
		t.Fatalf("stall detection must be disabled with a zero window: %v", err)
	}
}

// TestStepStallIdle pins the idle computation: fresh session activity resets
// the idle clock, activity predating the attempt does not, and an unknown
// session measures silence from the attempt start.
func TestStepStallIdle(t *testing.T) {
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"noop": &fakeAdapter{name: "noop", outcome: "success"},
	}}
	sessions := adapterhost.NewSessionManager(loader)
	g := &workflow.FSMGraph{
		Adapters:     map[string]*workflow.AdapterNode{"noop.default": {Type: "noop", Name: "default"}},
		AdapterOrder: []string{"noop.default"},
	}
	sessions.SetGraph(g)
	ctx := context.Background()
	t.Cleanup(func() { _ = sessions.Shutdown(ctx) })
	if err := sessions.OpenWithOriginRefs(ctx, "noop.default", "noop", "", nil, nil, nil, t.TempDir()); err != nil {
		t.Fatalf("open noop session: %v", err)
	}

	before := time.Now().Add(-time.Hour)
	if _, err := sessions.Execute(ctx, "noop.default", &workflow.StepNode{
		Name: "s", TargetKind: workflow.StepTargetAdapter, AdapterRef: "noop.default",
	}, noopSink{}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	idle, stalled := stepStallIdle(sessions, "noop.default", before, 30*time.Minute)
	if stalled {
		t.Fatalf("a completed Execute counts as progress; idle %s", idle)
	}
	if idle > time.Minute {
		t.Fatalf("fresh activity must yield a small idle, got %s", idle)
	}

	// stepStart after the last activity: the pre-attempt activity must not
	// count as progress. The idle clock starts at stepStart (still in the
	// future here, so idle is negative); if the stale activity were counted,
	// the tiny window would report a stall.
	futureStart := time.Now().Add(time.Hour)
	if idle, stalled = stepStallIdle(sessions, "noop.default", futureStart, time.Nanosecond); stalled {
		t.Error("activity predating the attempt start must not reset the idle clock")
	} else if idle >= 0 {
		t.Errorf("idle must be measured from the attempt start, got %s", idle)
	}

	if _, stalled = stepStallIdle(sessions, "missing.default", before, time.Minute); !stalled {
		t.Error("an unknown session with idle beyond the window must report stalled")
	}
}
