package engine

// prompts.go — the engine-side agent prompt delivery path (ADR-0006 D1/D2/D4/D5/D9).
// A PromptRouter consumes one run's AgentPrompt channel (fed from the
// orchestrator's Control stream) and delivers prompts into the addressed
// step's live adapter session while that step's Execute call is in flight.
// Delivery is queued-to-next-turn, never an interrupt: a prompt that arrives
// while the step is between attempts (retried after a transient failure) is
// held and delivered to the next attempt's session; a prompt addressed to a
// step with no live execution window fails as NO_ACTIVE_SESSION. Every
// non-delivery outcome is recorded as a structured log with a typed class;
// AgentPromptInjected is emitted exactly once, at successful delivery.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/brokenbots/criteria/internal/adapterhost"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
	"github.com/brokenbots/criteria/workflow"
)

// Prompt failure classes (ADR-0006 D9): deterministic, distinguishable, and
// never collapsed into silence.
const (
	PromptFailNotFound           = "NOT_FOUND"
	PromptFailAuthorization      = "AUTHORIZATION"
	PromptFailNoActiveSession    = "NO_ACTIVE_SESSION"
	PromptFailUnsupportedAdapter = "UNSUPPORTED_ADAPTER"
	PromptFailSessionMismatch    = "SESSION_MISMATCH"
	PromptFailAdapterRejected    = "ADAPTER_REJECTED"
	PromptFailDeliveryError      = "DELIVERY_ERROR"
)

// promptAction is the pump's routing decision for one message.
type promptAction int

const (
	promptDeliverNow promptAction = iota
	promptHold
	promptNoSession
)

// consoleCallerSentinel is the caller identity the orchestrator stamps on
// SendPrompt calls it authorizes itself (console-initiated). Delivery-side
// the agent trusts the orchestrator's acceptance-time gate for this identity
// (ADR-0006 D4: the primary gate is castle-side).
const consoleCallerSentinel = "console"

// PromptRouter consumes the run's AgentPrompt channel. Its lifetime is the
// runLoop: created after the SessionManager exists, stopped when the loop
// returns. All window bookkeeping (beginExecute/endExecute/endStep) is
// nil-safe so engines built without a prompt channel are untouched.
type PromptRouter struct {
	ch       <-chan *pb.AgentPrompt
	runID    string
	ownerID  string
	graph    *workflow.FSMGraph
	sessions *adapterhost.SessionManager
	sink     Sink
	log      *slog.Logger
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}

	stopOnce sync.Once

	mu        sync.Mutex
	inFlight  map[string]int // step → live attempt loops
	executing map[string]int // step → open delivery windows (adapter call in flight)
	held      map[string][]*pb.AgentPrompt
}

// NewPromptRouter starts the pump goroutine for ch. The caller must Stop it
// (or close ch) when the run loop returns.
func NewPromptRouter(ctx context.Context, ch <-chan *pb.AgentPrompt, runID, ownerID string, graph *workflow.FSMGraph, sessions *adapterhost.SessionManager, sink Sink, log *slog.Logger) *PromptRouter {
	if ch == nil || sink == nil || sessions == nil {
		return nil
	}
	if log == nil {
		log = slog.Default()
	}
	rctx, cancel := context.WithCancel(ctx)
	r := &PromptRouter{
		ch:        ch,
		runID:     runID,
		ownerID:   ownerID,
		graph:     graph,
		sessions:  sessions,
		sink:      sink,
		log:       log,
		ctx:       rctx,
		cancel:    cancel,
		done:      make(chan struct{}),
		inFlight:  make(map[string]int),
		executing: make(map[string]int),
		held:      make(map[string][]*pb.AgentPrompt),
	}
	go r.pump()
	return r
}

// Stop cancels the pump, records any prompts still held as failed, and waits
// for the pump goroutine to exit. Idempotent.
func (r *PromptRouter) Stop() {
	if r == nil {
		return
	}
	r.stopOnce.Do(func() {
		r.cancel()
		<-r.done
	})
}

func (r *PromptRouter) pump() {
	defer close(r.done)
	for {
		select {
		case <-r.ctx.Done():
			r.flushHeld("run no longer active")
			return
		case msg, ok := <-r.ch:
			if !ok {
				r.flushHeld("run no longer active")
				return
			}
			r.route(msg)
		}
	}
}

func (r *PromptRouter) route(msg *pb.AgentPrompt) {
	if msg.GetRunId() != r.runID {
		r.fail(msg, PromptFailNotFound, fmt.Sprintf("prompt addressed to run %q which is not this run", msg.GetRunId()))
		return
	}
	stepName := msg.GetStep()
	if stepName == "" {
		r.fail(msg, PromptFailNotFound, "prompt has no addressed step")
		return
	}
	if reason := promptCallerRejection(msg.GetCallerCriteriaId(), r.ownerID); reason != "" {
		r.fail(msg, PromptFailAuthorization, reason)
		return
	}

	r.mu.Lock()
	var action promptAction
	switch {
	case r.executing[stepName] > 0:
		action = promptDeliverNow
	case r.inFlight[stepName] > 0:
		action = promptHold
		r.held[stepName] = append(r.held[stepName], msg)
	default:
		action = promptNoSession
	}
	r.mu.Unlock()

	switch action {
	case promptDeliverNow:
		r.deliver(msg)
	case promptNoSession:
		r.fail(msg, PromptFailNoActiveSession, fmt.Sprintf("step %q has no live adapter session", stepName))
	}
}

// deliver performs the host-side delivery for one prompt: resolve the step's
// adapter ref, gate on adapter capability, verify the caller-supplied
// session id, then issue the Prompt RPC into the live session.
func (r *PromptRouter) deliver(msg *pb.AgentPrompt) {
	stepName := msg.GetStep()
	step, ok := r.graph.Steps[stepName]
	if !ok {
		r.fail(msg, PromptFailNotFound, fmt.Sprintf("step %q does not exist in this workflow", stepName))
		return
	}
	if step.TargetKind != workflow.StepTargetAdapter || step.AdapterRef == "" {
		r.fail(msg, PromptFailNotFound, fmt.Sprintf("step %q does not execute on an adapter", stepName))
		return
	}
	sessionID, err := r.sessions.Prompt(r.ctx, step.AdapterRef, step, msg.GetSessionId(), msg.GetPrompt())
	if err != nil {
		r.fail(msg, promptFailureClass(err), err.Error())
		return
	}
	// ADR-0006 D5: emitted exactly once, at the moment of delivery into the
	// adapter session — never at receipt, never on failure.
	r.sink.OnAgentPromptInjected(stepName, sessionID, msg.GetPrompt(), msg.GetCallerCriteriaId(), time.Now().UTC())
}

// beginExecute opens the per-attempt delivery window: a prompt arriving from
// now until endExecute is delivered mid-adapter-call. Prompts held from
// between attempts flush into the new attempt's session.
func (r *PromptRouter) beginExecute(step string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.inFlight[step]++
	r.executing[step]++
	held := r.held[step]
	delete(r.held, step)
	r.mu.Unlock()
	for _, msg := range held {
		r.deliver(msg)
	}
}

// endExecute closes the delivery window opened by beginExecute. Held prompts
// survive between attempts (retry backoff) and are delivered to the next
// attempt's session.
func (r *PromptRouter) endExecute(step string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.executing[step]--
	if r.executing[step] <= 0 {
		delete(r.executing, step)
	}
	r.mu.Unlock()
}

// endStep closes the attempt loop for a step, draining the inFlight balance
// owned by its attempt loop: attempts is the number of attempts the loop
// actually consumed, so inFlight returns to its pre-loop balance (0 for a
// plain visit) for any attempt count. Any prompt still held for the step has
// missed its delivery window: the step is complete, so it becomes
// NO_ACTIVE_SESSION. Also drops a stale executing window left by an
// early-returned execute path.
func (r *PromptRouter) endStep(step string, attempts int) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.inFlight[step] -= attempts
	if r.inFlight[step] <= 0 {
		delete(r.inFlight, step)
		delete(r.executing, step)
	}
	held := r.held[step]
	delete(r.held, step)
	r.mu.Unlock()
	r.recordNoSession(held, fmt.Sprintf("step %q completed before delivery", step))
}

// flushHeld records every held prompt as NO_ACTIVE_SESSION (stop or channel
// close while prompts are queued).
func (r *PromptRouter) flushHeld(reason string) {
	r.mu.Lock()
	held := r.held
	r.held = make(map[string][]*pb.AgentPrompt)
	r.mu.Unlock()
	for _, msgs := range held {
		r.recordNoSession(msgs, reason)
	}
}

func (r *PromptRouter) recordNoSession(msgs []*pb.AgentPrompt, reason string) {
	for _, msg := range msgs {
		r.fail(msg, PromptFailNoActiveSession, reason)
	}
}

func (r *PromptRouter) fail(msg *pb.AgentPrompt, class, reason string) {
	r.log.Error("agent prompt delivery failed",
		"class", class,
		"run_id", msg.GetRunId(),
		"step", msg.GetStep(),
		"caller_criteria_id", msg.GetCallerCriteriaId(),
		"reason", reason,
	)
}

func promptFailureClass(err error) string {
	switch {
	case errors.Is(err, adapterhost.ErrPromptUnsupportedAdapter):
		return PromptFailUnsupportedAdapter
	case errors.Is(err, adapterhost.ErrPromptNoActiveSession):
		return PromptFailNoActiveSession
	case errors.Is(err, adapterhost.ErrPromptSessionMismatch):
		return PromptFailSessionMismatch
	case errors.Is(err, adapterhost.ErrPromptRejected):
		return PromptFailAdapterRejected
	default:
		return PromptFailDeliveryError
	}
}

// promptCallerRejection implements the delivery-side caller re-check
// (ADR-0006 D4, defense in depth). The Control stream is point-to-point to
// the owning agent, so the primary gate is the orchestrator's acceptance-time
// authorization; this re-check fails closed on anything but the run's owner
// identity or the orchestrator's console sentinel. A non-empty return value
// is the typed rejection reason.
func promptCallerRejection(caller, ownerID string) string {
	switch {
	case caller == ownerID && ownerID != "":
		return ""
	case caller == consoleCallerSentinel:
		return ""
	case strings.TrimSpace(caller) == "":
		return "prompt carries no caller_criteria_id"
	default:
		return fmt.Sprintf("caller %q is not the run owner", caller)
	}
}
