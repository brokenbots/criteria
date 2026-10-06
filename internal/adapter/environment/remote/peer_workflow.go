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
//     BEFORE the session closes (orphan-by-default is not an option in v0.6.0;
//     there is no detach).

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
}

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
		if rec, ok := ps.childRuns[id]; ok {
			// Re-arm for a known id (journal replay skew): never erase the
			// settled truth of an already-terminal run.
			if !rec.inFlight() {
				return ""
			}
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
			return ""
		}
		rec.TerminalOutcome = term.GetOutcome()
		rec.OutputsDigest = term.GetOutputsDigest()
		slog.Debug("peer child run terminal", "adapter", ps.dial.AdapterType, "scope", ps.dial.Scope,
			"run_id", id, "outcome", rec.TerminalOutcome)
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

// removeChildWatch drops one waiter and closes it (the caller's select has
// already resolved; the closed channel simply releases any concurrent
// closer without blocking).
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
	close(watch)
}

// cancelInFlightChildRun issues the ControlRequest.cancel_child_run control
// so the child run does not outlive the session teardown (ADR-0008 ordering:
// cancel, then close, then process cleanup by the child host). An empty run
// id means "the current one" on the child's control surface, which covers a
// run the tracker has not yet observed. Best-effort: failures are logged and
// the teardown proceeds.
func (ps *peerSession) cancelInFlightChildRun(ctx context.Context) {
	if !ps.hasPeerCapability(peerWorkflowV1Capability) {
		return
	}
	ps.mu.Lock()
	rec := ps.childRunInFlightLocked()
	runID := ""
	if rec != nil {
		runID = rec.RunID
	}
	ps.mu.Unlock()

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
		return
	}
	if !resp.GetAccepted() {
		slog.Info("peer accepted no child run cancel", "adapter", ps.dial.AdapterType, "run_id", runID, "detail", resp.GetDetail())
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
		return adapter.Result{}, fmt.Errorf("workflow.v1 child run %q adoption never observed a terminal outcome: %w (guard: %v)", runID, err, guardErr)
	}
	slog.Info("adopted surviving workflow.v1 child run",
		"adapter", h.ps.dial.AdapterType, "run_id", rec.RunID,
		"outcome", rec.TerminalOutcome, "outputs_digest", rec.OutputsDigest)
	return adapter.Result{
		Outcome: rec.TerminalOutcome,
		Comment: fmt.Sprintf("adopted surviving child run %q after parent restart", rec.RunID),
	}, nil
}