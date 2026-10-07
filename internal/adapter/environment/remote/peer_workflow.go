package remote

// peer_workflow.go — parent-side mapping of a criteria-adapter session to
// its child run (KB-95, ADR-0008 D2). The compile side treats the
// criteria-child adapter like any other adapter (its handshake declares
// step input/output), so all runtime behavior lives here:
//
//   - the supervision journal's ChildRunStarted / ChildRunTerminal arms are
//     consumed into a per-session run tracker (the parent's only child-run
//     truth: the journal replays it after a parent crash + restart),
//   - Execute wraps the v2 call with the one-shot semantics: a FailedPrecondition
//     guard for a run this parent did not start adopts the surviving run from
//     its journal record instead of spawning fresh, and a transport loss while
//     a run is in flight is reported as child-run evidence,
//   - teardown cancels the in-flight child run over ControlRequest.cancel_child_run
//     then waits the bounded settle budget for the terminal journal evidence
//     before the close proceeds — a run that fails to settle is force-settled
//     with ControlRequest.kill_child plus a structured partial-teardown warn
//     (KB-96, ADR-0008 D2 stop semantics; there is no detach in v0.6.0).

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"

	criteriav1 "github.com/brokenbots/criteria/sdk/pb/criteria/v1"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow"
)

// peerWorkflowV1Capability is the role capability the peer identity frame
// carries for ADR-0008 criteria-as-adapter peers (internal/peer). The parent
// checks it before consuming the child-run mapping: legacy peers without the
// capability keep the plain adapter behavior untouched.
const peerWorkflowV1Capability = "workflow.v1"

// peerCancelChildRunTimeout bounds the cancel control round-trip on teardown
// paths. Cancellation is cooperative: the child ACKs the control without
// blocking on the run's settle, so a bounded single control is enough.
const peerCancelChildRunTimeout = 5 * time.Second

// peerCancelChildRunGraceMs is the grace period the child applies between
// signalling the run and force-settling it.
const peerCancelChildRunGraceMs = 15000

// peerChildRunTeardownWait is the bounded budget the parent waits for the
// cancelled child run's terminal journal evidence before force-settling
// (KB-96 ADR-0008 D2). It matches the child's own cancel grace
// (peerCancelChildRunGraceMs): a healthy child surfaces its terminal arm
// well inside the budget, and a wedged one is force-settled exactly at it.
const peerChildRunTeardownWait = time.Duration(peerCancelChildRunGraceMs) * time.Millisecond

// peerChildRunTeardownPoll paces the settle poll for a child run the
// tracker has not observed yet (no known run id to register a watch on).
const peerChildRunTeardownPoll = 100 * time.Millisecond

// errChildRunUnsettled reports that no terminal evidence arrived for a child
// run before the wait ended.
var errChildRunUnsettled = errors.New("child run had no terminal evidence")

// childRunRecord is the parent-side truth for one child run, lifted from the
// supervision journal arms. An in-flight record is one whose journal has
// delivered ChildRunStarted but no ChildRunTerminal.
type childRunRecord struct {
	RunID           string
	WorkflowDigest  string
	Version         string
	TerminalOutcome string
	OutputsDigest   string
	// ForcedTeardown marks a record settled through the child's
	// ChildRunTeardownPartial arm (a parent force kill on an in-flight,
	// never-terminal run): the mark is provisional until the child's real
	// terminal arm arrives late, which outranks it.
	ForcedTeardown bool
}

// childRunOutcomeForceKilled is the parent-side outcome vocabulary for a
// run settled through the ChildRunTeardownPartial journal arm (ADR-0008
// D2, KB-96): the parent's settle grace expired and the kill_child landed
// before the run's own terminal ever reached the journal.
const childRunOutcomeForceKilled = "force_killed"

// inFlight reports whether the run started without a terminal arm yet.
func (r *childRunRecord) inFlight() bool { return r != nil && r.TerminalOutcome == "" }

// hasPeerCapability reports whether the phone-home identity frame negotiated
// the capability. A peer without a Peer identity block (legacy dial shape)
// negotiates nothing.
func (ps *peerSession) hasPeerCapability(capability string) bool {
	if ps == nil || ps.dial.Peer == nil {
		return false
	}
	for _, c := range ps.dial.Peer.Capabilities {
		if c == capability {
			return true
		}
	}
	return false
}

// applyChildRunArmLocked routes the workflow.v1 journal arms into the
// tracker. Caller holds ps.mu. Returns the id of the run whose terminal just
// settled ("" otherwise) so the watch broadcast can happen after the unlock.
func (ps *peerSession) applyChildRunArmLocked(ev *criteriav1.SupervisionEvent) string {
	switch kind := ev.GetKind().(type) {
	case *criteriav1.SupervisionEvent_ChildRunStarted:
		started := kind.ChildRunStarted
		id := started.GetRunId()
		if id == "" {
			return ""
		}
		if _, ok := ps.childRuns[id]; ok {
			// Re-arm for a known id (journal replay skew): never erase the
			// settled truth of an already-terminal run.
			return ""
		}
		ps.childRuns[id] = &childRunRecord{
			RunID:          id,
			WorkflowDigest: started.GetWorkflowDigest(),
			Version:        started.GetVersion(),
		}
		slog.Debug("peer child run started", "adapter", ps.dial.AdapterType, "scope", ps.dial.Scope,
			"run_id", id, "workflow_digest", started.GetWorkflowDigest())
	case *criteriav1.SupervisionEvent_ChildRunTerminal:
		term := kind.ChildRunTerminal
		id := term.GetRunId()
		if id == "" {
			return ""
		}
		rec := ps.childRuns[id]
		if rec == nil {
			// Terminal without a seen Started (replay boundary in between):
			// record the settled truth so the run is never treated as in
			// flight on this session.
			rec = &childRunRecord{RunID: id}
			ps.childRuns[id] = rec
		}
		if !rec.inFlight() {
			if rec.ForcedTeardown && term.GetOutcome() != "" {
				// The forced cancel still reached a terminal state and the
				// child journaled it late: the real terminal outranks the
				// partial-teardown mark.
				rec.TerminalOutcome = term.GetOutcome()
				rec.OutputsDigest = term.GetOutputsDigest()
				rec.ForcedTeardown = false
			}
			return ""
		}
		rec.TerminalOutcome = term.GetOutcome()
		rec.OutputsDigest = term.GetOutputsDigest()
		slog.Debug("peer child run terminal", "adapter", ps.dial.AdapterType, "scope", ps.dial.Scope,
			"run_id", id, "outcome", rec.TerminalOutcome)
		return id
	case *criteriav1.SupervisionEvent_ChildRunTeardownPartial:
		// KB-96 (ADR-0008 D2): the parent's force kill landed on an
		// in-flight run whose terminal never journaled — the partial arm is
		// the child's last run truth, so it settles the run (deterministic
		// teardown: no waiter ever hangs on a killed run). A run the
		// tracker never observed carries no in-flight truth to correct.
		partial := kind.ChildRunTeardownPartial
		id := partial.GetRunId()
		if id == "" {
			return ""
		}
		rec := ps.childRuns[id]
		if rec == nil || !rec.inFlight() {
			return ""
		}
		rec.TerminalOutcome = childRunOutcomeForceKilled
		rec.ForcedTeardown = true
		slog.Warn("peer child run force killed on parent teardown", "adapter", ps.dial.AdapterType,
			"scope", ps.dial.Scope, "run_id", id, "detail", partial.GetDetail())
		return id
	}
	return ""
}

// noteChildRunTerminal wakes every waiter registered on the settled run.
func (ps *peerSession) noteChildRunTerminal(runID string) {
	ps.mu.Lock()
	watches := ps.childWatches[runID]
	delete(ps.childWatches, runID)
	ps.mu.Unlock()
	for _, wake := range watches {
		close(wake)
	}
}

// childRunInFlightLocked returns the tracked in-flight run, if any. Caller
// holds ps.mu.
func (ps *peerSession) childRunInFlightLocked() *childRunRecord {
	for _, rec := range ps.childRuns {
		if rec.inFlight() {
			return rec
		}
	}
	return nil
}

// childRunInFlight returns the tracked in-flight run (its journal record),
// if any.
func (ps *peerSession) childRunInFlight() (*childRunRecord, bool) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	rec := ps.childRunInFlightLocked()
	return rec, rec != nil
}

// waitChildRunTerminal blocks until the named child run settles on the
// supervision journal, the session's transport closes (wake via ps.done), or
// the context is cancelled. It returns the settled record.
func (ps *peerSession) waitChildRunTerminal(ctx context.Context, runID string) (*childRunRecord, error) {
	ps.mu.Lock()
	if rec := ps.childRuns[runID]; rec != nil && !rec.inFlight() {
		ps.mu.Unlock()
		return rec, nil
	}
	wake := make(chan struct{})
	ps.childWatches[runID] = append(ps.childWatches[runID], wake)
	done := ps.done
	ps.mu.Unlock()

	select {
	case <-wake:
	case <-done:
	case <-ctx.Done():
		ps.removeChildWatch(runID, wake)
		return nil, ctx.Err()
	}

	ps.mu.Lock()
	defer ps.mu.Unlock()
	rec := ps.childRuns[runID]
	if rec == nil || rec.inFlight() {
		// Woken by the transport closing or a removed watch: no terminal.
		return nil, errChildRunUnsettled
	}
	return rec, nil
}

// removeChildWatch drops one waiter whose select already resolved. No close:
// noteChildRunTerminal is the single closer of every registered watch — if
// the terminal raced this removal it already consumed the watch from the
// map (and closed it); the waiter itself reads nothing past its select.
func (ps *peerSession) removeChildWatch(runID string, watch chan struct{}) {
	ps.mu.Lock()
	watches := ps.childWatches[runID]
	kept := make([]chan struct{}, 0, len(watches))
	for _, w := range watches {
		if w != watch {
			kept = append(kept, w)
		}
	}
	if len(kept) == 0 {
		delete(ps.childWatches, runID)
	} else {
		ps.childWatches[runID] = kept
	}
	ps.mu.Unlock()
}

// teardownInFlightChildRun tears down a live child run ahead of a session
// close (KB-96, ADR-0008 D2 stop semantics). The sequence is:
//
//  1. the cooperative cancel control (an empty run id means "the current
//     one" on the child's control surface, covering a run the tracker has
//     not yet observed),
//  2. a bounded wait for the terminal evidence on the supervision journal
//     — the close that follows must not strand or race an unsettled run,
//     and the journal terminal is the cross-feed proof the parent step can
//     attribute its own outcome to,
//  3. on budget expiry, the KillChild control plus a structured
//     partial-teardown warn (the child journals its matching arm) — the
//     parent step's outcome then stays deterministic through the transport
//     teardown instead of hanging on a wedged child.
//
// The wait is detached from the caller's context (teardown paths race
// context cancellation by design) and stays best-effort: every failure is
// logged and the teardown proceeds.
func (ps *peerSession) teardownInFlightChildRun(ctx context.Context) {
	if !ps.hasPeerCapability(peerWorkflowV1Capability) {
		return
	}
	// KB-95 contract preserved: the cancel is issued even when the tracker
	// shows nothing (the empty run id means "the current one" child-side,
	// covering a run the parent has not observed yet).
	rec, inFlight := ps.childRunInFlight()
	runID := ""
	if inFlight {
		runID = rec.RunID
	}

	cctx, cancel := context.WithTimeout(ctx, peerCancelChildRunTimeout)
	defer cancel()
	resp, err := ps.control(cctx, &criteriav1.ControlRequest{
		AdapterType: ps.dial.AdapterType,
		Scope:       ps.dial.Scope,
		GraceMs:     peerCancelChildRunGraceMs,
		Kind: &criteriav1.ControlRequest_CancelChildRun{
			CancelChildRun: &criteriav1.CancelChildRun{RunId: runID, GraceMs: peerCancelChildRunGraceMs},
		},
	})
	if err != nil {
		slog.Warn("peer child run cancel control failed", "adapter", ps.dial.AdapterType, "run_id", runID, "error", err)
	} else if !resp.GetAccepted() {
		// Rejected means the child holds no live run (e.g. it settled in the
		// race window): nothing to wait for, nothing to force.
		slog.Info("peer accepted no child run cancel", "adapter", ps.dial.AdapterType, "run_id", runID, "detail", resp.GetDetail())
		return
	}

	wctx, wcancel := context.WithTimeout(context.WithoutCancel(ctx), ps.teardownSettleBudget())
	defer wcancel()
	rec, settled := ps.waitChildRunSettle(wctx, runID)
	if settled {
		outcome := ""
		if rec != nil {
			outcome = rec.TerminalOutcome
		}
		slog.Debug("peer child run settled on teardown", "adapter", ps.dial.AdapterType, "run_id", runID,
			"outcome", outcome)
		return
	}

	slog.Warn("peer child run teardown incomplete",
		"adapter", ps.dial.AdapterType, "scope", ps.dial.Scope, "run_id", runID,
		"action", "kill_child issued after the settle grace expired")

	kctx, kcancel := context.WithTimeout(context.WithoutCancel(ctx), peerCancelChildRunTimeout)
	defer kcancel()
	killResp, killErr := ps.control(kctx, &criteriav1.ControlRequest{
		AdapterType: ps.dial.AdapterType,
		Scope:       ps.dial.Scope,
		GraceMs:     peerKillGraceMs,
		Kind:        &criteriav1.ControlRequest_KillChild{KillChild: &criteriav1.KillChild{}},
	})
	switch {
	case killErr != nil:
		slog.Warn("peer child run force kill control failed", "adapter", ps.dial.AdapterType, "run_id", runID, "error", killErr)
	case !killResp.GetAccepted():
		slog.Warn("peer rejected child run force kill", "adapter", ps.dial.AdapterType, "run_id", runID, "detail", killResp.GetDetail())
	default:
		// KB-96 (ADR-0008 D2): the kill is the parent's own last-knowable
		// truth about the run it tracked in flight — settle the record
		// immediately. The child's ChildRunTeardownPartial journal arm
		// stays the child-feed evidence (and replays into a restarted
		// parent); here it only confirms an already-settled record or is
		// outranked by a late real terminal.
		ps.noteChildRunForceKilled(runID)
	}
}

// noteChildRunForceKilled settles a tracked in-flight run as force_killed
// and wakes its watchers (the parent-side mirror of the child's
// ChildRunTeardownPartial arm). Runs the tracker did not observe — and
// records already settled — are left untouched.
func (ps *peerSession) noteChildRunForceKilled(runID string) {
	if runID == "" {
		return
	}
	ps.mu.Lock()
	rec := ps.childRuns[runID]
	if rec == nil || !rec.inFlight() {
		ps.mu.Unlock()
		return
	}
	rec.TerminalOutcome = childRunOutcomeForceKilled
	rec.ForcedTeardown = true
	ps.mu.Unlock()
	ps.noteChildRunTerminal(runID)
}

// waitChildRunSettle waits, bounded by ctx, for the child run's terminal
// journal evidence. A known run id registers a journal watch (settles
// promptly on the terminal arm or the transport closing); an unknown id —
// a run the tracker has not observed yet — polls the tracker instead: the
// terminal arm lands on the journal at any point mid-settle and the poll
// observes its absence. Returns the settled record (nil, true) meaning
// "no run in flight anymore" and the settled record; (nil, false) when the
// budget expired without evidence.
func (ps *peerSession) waitChildRunSettle(ctx context.Context, runID string) (*childRunRecord, bool) {
	if runID != "" {
		rec, err := ps.waitChildRunTerminal(ctx, runID)
		return rec, err == nil
	}
	for {
		if _, ok := ps.childRunInFlight(); !ok {
			return nil, true
		}
		select {
		case <-ctx.Done():
			return nil, false
		case <-time.After(peerChildRunTeardownPoll):
		}
	}
}
// childRunGuardPattern extracts the run id from the child's one-run
// re-execute guard. The KB-94 wire shape is a message-only FailedPrecondition
// status (`child run %q is still in flight for workflow %q; ...`), so the id
// can only travel in the message; the parse is bounded to the quoted id and
// no other message content is trusted.
var childRunGuardPattern = regexp.MustCompile(`child run "([^"]+)"`)

// childRunIDFromGuardError extracts the in-flight run id from a
// FailedPrecondition child-run guard, or "" when the message does not carry
// a parseable id.
func childRunIDFromGuardError(err error) string {
	matches := childRunGuardPattern.FindStringSubmatch(err.Error())
	if len(matches) < 2 {
		return ""
	}
	return matches[1]
}

// isPeerTransportLoss reports whether err is the phone-home transport dying
// under this Execute: status-coded transport failures are the loss shapes a
// lost connection produces. Context cancellation and clean stream closes
// are the caller's own teardown, not evidence about the child.
func isPeerTransportLoss(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	switch status.Code(err) {
	case codes.Unavailable, codes.Internal, codes.DataLoss, codes.Unknown:
		return true
	}
	return false
}

// executeChildRunAware is peerHandle.Execute with the KB-95 child-run
// mapping (ADR-0008): one-shot execute semantics and crash adoption.
//
//   - the happy path is unchanged: ExecuteViaClient streams the step through
//     the child; workflow.v1-prefixed adapter events land in the event feed
//     and typed outputs decode from outputs_json as for any adapter,
//   - a FailedPrecondition guard for a run this parent did not start is the
//     reattach shape (parent crashed + restarted, the fresh session replays
//     the child journal, then re-executes): adopt the surviving run from its
//     record — wait for the journal's terminal arm and project the run's
//     verdict as the step's outcome. No fresh child run is ever spawned,
//   - a transport loss while a child run is tracked in flight is wrapped as
//     child-run evidence so the step fails with the child truth (the crash
//     classifier consumes the same state) instead of a raw transport string.
//
// A peer without the workflow.v1 capability takes the unchanged path.
func (h *peerHandle) executeChildRunAware(ctx context.Context, sessionID string, hasPermStream bool, step *workflow.StepNode, sink adapter.EventSink, rejection *v2.ExecutionRejection) (adapter.Result, error) {
	result, err := adapterhost.ExecuteViaClient(ctx, h.ps.client, h.ps.dial.AdapterType, sessionID, hasPermStream, step, sink, rejection)
	if err == nil {
		return result, nil
	}
	if !h.ps.hasPeerCapability(peerWorkflowV1Capability) || ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return result, err
	}
	if status.Code(err) == codes.FailedPrecondition {
		return h.adoptSurvivingChildRun(ctx, err)
	}
	if rec, inFlight := h.ps.childRunInFlight(); inFlight && isPeerTransportLoss(err) {
		// The loss is conclusive for classification; the supervise consumer
		// races this observation, so mark it here for the classifier that
		// runs right after this error is returned.
		h.ps.lost.Store(true)
		return adapter.Result{}, fmt.Errorf("workflow.v1 child run %q was in flight when the peer connection was lost: %w", rec.RunID, err)
	}
	return result, err
}

// adoptSurvivingChildRun consumes the child's re-execute guard for a run
// this parent did not start. The child's one-run guard means a run is live;
// the parent waits for its ChildRunTerminal arm on the already-running
// supervision stream (bounded by the step context) and maps the terminal to
// the step verdicts via the journal record. Outputs are not available on
// this path — the journal carries the outputs digest as a wire-truth
// pointer, not the map — so the adopted result carries the verdict and an
// adoption comment only.
func (h *peerHandle) adoptSurvivingChildRun(ctx context.Context, guardErr error) (adapter.Result, error) {
	runID := childRunIDFromGuardError(guardErr)
	if runID == "" {
		if rec, ok := h.ps.childRunInFlight(); ok {
			runID = rec.RunID
		}
	}
	if runID == "" {
		return adapter.Result{}, fmt.Errorf("workflow.v1 re-execute guard fired but did not identify its child run: %w", guardErr)
	}
	rec, err := h.ps.waitChildRunTerminal(ctx, runID)
	if err != nil {
		if errors.Is(err, errChildRunUnsettled) {
			return adapter.Result{}, fmt.Errorf("workflow.v1 child run %q adopted from the peer journal but the connection lost it before a terminal outcome: %w", runID, guardErr)
		}
		return adapter.Result{}, fmt.Errorf("workflow.v1 child run %q adoption never observed a terminal outcome: %w (guard: %w)", runID, err, guardErr)
	}
	slog.Info("adopted surviving workflow.v1 child run",
		"adapter", h.ps.dial.AdapterType, "run_id", rec.RunID,
		"outcome", rec.TerminalOutcome, "outputs_digest", rec.OutputsDigest)
	return adapter.Result{
		Outcome: rec.TerminalOutcome,
		Comment: fmt.Sprintf("adopted surviving child run %q after parent restart", rec.RunID),
	}, nil
}
