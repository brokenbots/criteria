package engine

// KB-25: a step blocked with no observable adapter progress for a bounded
// window (env-tunable via the tunables registry, default 30m) fails the
// workflow with a typed stall error instead of wedging the run until an
// operator kills it. The CRI-287
// step-timeout teardown window semantics are preserved: the watchdog opens
// the same engine-initiated teardown mark before cancelling, so transport
// closes during the Execute unwind are routed as teardown consequences and
// not as session crashes. Steps that declare their own timeout keep the
// CRI-275 ceiling unchanged — the watchdog only arms for unbounded steps.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/internal/tunables"
	"github.com/brokenbots/criteria/workflow"
)

// errStepStalled is the sentinel every StepStallError unwraps to.
var errStepStalled = errors.New("step stalled with no adapter progress")

// StepStallError fails a run whose step made no observable adapter progress
// for the configured stall window (KB-25).
type StepStallError struct {
	Step   string
	Window time.Duration
	Idle   time.Duration
}

func (e *StepStallError) Error() string {
	return fmt.Sprintf("step %q stalled: no adapter activity for %s (stall window %s)", e.Step, e.Idle, e.Window)
}

func (e *StepStallError) Unwrap() error { return errStepStalled }

// stepStallWatchdog watches one running step attempt for adapter activity.
// It fires when the step's session shows no activity for the stall window:
// firing opens the CRI-287 teardown window and cancels the step's execution
// context so Execute unblocks. The watchdog never fires on a parent- or
// step-context cancellation of its own — those keep their existing routing.
type stepStallWatchdog struct {
	// cancel cancels the watchdog's execution context. It is the context
	// the step's Execute runs under; cancelling is how a fired watchdog
	// tears the wedged step down.
	cancel context.CancelFunc
	// done closes when the watchdog goroutine has settled.
	done chan struct{}
	// fired and idle are written only by the watchdog goroutine before done
	// closes; stop's caller reads them after joining.
	fired  bool
	idle   time.Duration
	window time.Duration
}

// startStepStallWatchdog arms the KB-25 stall watchdog for a step attempt.
// The second return is the context the attempt must execute under: a
// watchdog-derived cancellation layer when armed, parent otherwise.
//
// A nil SessionManager, a step without an adapter target, a step-declared
// timeout (the CRI-275 ceiling already bounds it), or a disabled window
// (window <= 0) all disarm the watchdog. stepStart is the attempt's start
// time: activity observed before it belongs to earlier attempts or other
// steps and never counts as progress for this one.
func startStepStallWatchdog(parent context.Context, deps Deps, step *workflow.StepNode, stepStart time.Time) (*stepStallWatchdog, context.Context) {
	// KB-25: the stall window (and its disabled posture, window == 0) come
	// from the tunables registry (CRITERIA_STEP_STALL_WINDOW).
	window := tunables.FromEnv().StepStallWindow
	if deps.Sessions == nil || step == nil || step.AdapterRef == "" || step.Timeout > 0 || window <= 0 {
		return &stepStallWatchdog{}, parent
	}
	watchCtx, cancel := context.WithCancel(parent)
	w := &stepStallWatchdog{cancel: cancel, done: make(chan struct{}), window: window}
	go func() {
		defer close(w.done)
		ticker := time.NewTicker(tunables.StepStallPollInterval(window))
		defer ticker.Stop()
		for {
			select {
			case <-parent.Done():
				return
			case <-watchCtx.Done():
				return
			case <-ticker.C:
			}
			idle, stalled := stepStallIdle(deps.Sessions, step.AdapterRef, stepStart, window)
			if !stalled {
				continue
			}
			w.idle = idle
			w.fired = true
			// Engine-initiated teardown of a wedged step: open the CRI-287
			// reclassification window BEFORE cancelling, so transport closes
			// during the Execute unwind are routed as teardown consequences
			// instead of session crashes.
			deps.Sessions.MarkEngineStepTimeoutTeardown()
			cancel()
			return
		}
	}()
	return w, watchCtx
}

// stepStallIdle reports the step's current idle time and whether it already
// exceeds the stall window. Progress is the session's last observable
// activity after stepStart; with no such activity the idle clock starts at
// stepStart, so the window measures this step's own silence.
func stepStallIdle(sessions *adapterhost.SessionManager, adapterRef string, stepStart time.Time, window time.Duration) (time.Duration, bool) {
	idle := time.Since(stepStart)
	if last, ok := sessions.LastSessionActivity(adapterRef); ok && last.After(stepStart) {
		idle = time.Since(last)
	}
	return idle, idle >= window
}

// stop stops and joins the watchdog. It reports whether the watchdog fired
// during the attempt, with the idle time observed at fire.
func (w *stepStallWatchdog) stop() (bool, time.Duration) {
	if w.cancel != nil {
		w.cancel()
	}
	if w.done != nil {
		<-w.done
	}
	return w.fired, w.idle
}

// stallFailure builds the run-fatal typed stall error for a step whose
// watchdog fired (KB-25). Wrapping in FatalRunError aborts the run: a wedged
// step produces no outcome, so neither its declared outcomes nor the
// failure/default routing can make progress either.
func stallFailure(step *workflow.StepNode, window, idle time.Duration) error {
	return &adapterhost.FatalRunError{Err: &StepStallError{Step: step.Name, Window: window, Idle: idle}}
}
