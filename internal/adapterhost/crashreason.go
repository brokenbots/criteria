package adapterhost

import (
	"context"
	"fmt"
	"time"
)

// Crash-reason taxonomy for adapter session crash classification (T-02).
//
// This set is the single source of truth for the classification vocabulary:
// classifySessionCrash returns exactly one of these constants, and both the
// host-side crash mapping (session.crash events, inspect payloads) and the
// peer supervision journal emission (peer.proto CrashClassified.reason,
// ADR-0007 Stage A) consume it. The values are stable wire/log vocabulary —
// do not reword them; extend the taxonomy only by appending new constants.
const (
	// CrashReasonProcessExitedEarly is the most precise diagnosis: the
	// adapter process died before the in-flight call completed.
	CrashReasonProcessExitedEarly = "adapter process exited before the call completed"

	// CrashReasonUnknown is returned when neither process state nor an error
	// is available to classify (nil session, nil error).
	CrashReasonUnknown = "unknown"

	// CrashReasonHeartbeatStall: the adapter stopped streaming its log
	// stream and the stall watchdog tripped.
	CrashReasonHeartbeatStall = "log-stream heartbeat stall (adapter stopped streaming)"

	// CrashReasonTransportClosed: the go-plugin shim's gRPC transport closed
	// underneath a live session (adapter or shim closed the connection).
	CrashReasonTransportClosed = "gRPC client transport closed (adapter or shim closed the connection)"

	// CrashReasonEndpointUnavailable: the adapter's gRPC endpoint is gone,
	// i.e. the adapter process died.
	CrashReasonEndpointUnavailable = "gRPC endpoint unavailable (adapter process gone)"

	// CrashReasonStdioPipeBroken: the plugin stdio pipe broke because the
	// adapter process died.
	CrashReasonStdioPipeBroken = "plugin stdio pipe broken (adapter process died)"

	// CrashReasonStdioEOF: the plugin stdio stream hit EOF because the
	// adapter process exited or closed its stream.
	CrashReasonStdioEOF = "plugin stdio EOF (adapter process exited or closed its stream)"

	// CrashReasonProcessTerminated: the adapter process was terminated.
	CrashReasonProcessTerminated = "adapter process terminated"

	// CrashReasonUnknownAdapterError: an error that matches no known
	// transport or process signature.
	CrashReasonUnknownAdapterError = "unknown adapter error"

	// CrashReasonChildRunLost: the phone-home transport to a workflow.v1
	// peer died while a child run was tracked in flight and the journal
	// delivered no terminal evidence (ADR-0008). The child's verdict
	// is unknowable at classification time even though the child may still
	// be alive — the run anchors to the child's base context, so the loss
	// itself is the crash fact for the parent's step; the surviving run is
	// the adoption path's subject, not a fresh spawn.
	CrashReasonChildRunLost = "workflow.v1 child run was in flight when the peer connection was lost"
)

// SupervisedHandle is the optional Handle capability a peer-supervised
// handle implements (ADR-0007, T-07): the peer's supervision journal
// delivered a terminal classification for the adapter child, and the host
// consumes that classification verbatim instead of guessing.
//
// The reason value is a CrashClassified.reason wire fact — one of the
// CrashReason* constants above, the same taxonomy the host's own classifier
// uses, emitted by the peer's child-side journal (T-05). Classification is
// journal-only (ADR-0014 M5): a delivered classification is consumed
// verbatim ahead of the structural checks, and error text is never parsed —
// a session whose journal delivered nothing and whose process is still
// alive is not a crash, whatever the transport error says.
type SupervisedHandle interface {
	// SupervisionCrashReason returns the crash reason the handle's
	// supervision story carries and ok=true. The value is a single
	// CrashReason* wire fact — most often the journal's CrashClassified
	// record verbatim, but a supervised handle may also synthesize the
	// fact structurally when the journal holds no terminal and the conn
	// knowledge is itself the missing-terminal evidence (e.g. the peer's
	// lost-conn arm: ChildRunLost for a run in flight, else
	// TransportClosed). ok=false means no terminal classification is
	// available (the child is alive, exited cleanly, or only the plain
	// Exited journal record arrived) — classification then falls through
	// to the local structural evidence path.
	SupervisionCrashReason() (reason string, ok bool)
}

// SupervisionCrashReason returns the crash classification the handle's
// supervision story delivers for its adapter child (verbatim wire fact), or
// ok=false when the handle carries no supervision classification —
// including when the delivered reason is empty or outside the CrashReason*
// taxonomy (wire garbage or host/peer version skew): the classification
// path is documented to yield taxonomy constants only, so anything else
// falls through to the local evidence path.
func SupervisionCrashReason(h Handle) (string, bool) {
	sh, ok := h.(SupervisedHandle)
	if !ok || sh == nil {
		return "", false
	}
	return filterTaxonomyReason(sh.SupervisionCrashReason())
}

// filterTaxonomyReason keeps the classification vocabulary guarantee: the
// journal path yields taxonomy constants only, so anything else falls
// through to the local evidence path.
func filterTaxonomyReason(reason string, ok bool) (string, bool) {
	if !ok || !isTaxonomyReason(reason) {
		return "", false
	}
	return reason, true
}

// watchdogStallError is the typed carrier for the heartbeat watchdog's
// verdict (ADR-0014 M5): the watchdog observed the log-stream stall
// directly, so the classification is structural evidence and classifySessionCrash
// reads it through errors.As instead of matching the message text. The
// message keeps the previous log rendering verbatim.
type watchdogStallError struct {
	threshold time.Duration
}

func (e *watchdogStallError) Error() string {
	return fmt.Sprintf("heartbeat stall (>%s)", e.threshold)
}

// JournalCrashSettler is the optional SupervisedHandle capability for
// in-process supervision: a transport death precedes the supervision
// journal's exit fact (the child client flips its process-exited flag
// strictly after the transport fails), so a kill that races
// classification leaves the plain journal query empty for a few
// milliseconds. A settled query bridges that flip window inside a caller
// supplied budget before concluding "no terminal classification".
type JournalCrashSettler interface {
	SupervisedHandle
	// SettledSupervisionCrashReason settles the journal's exit fact within
	// the ctx deadline and classifies exactly like SupervisionCrashReason:
	// the journal's current-generation crash fact and ok=true, or ok=false
	// when no terminal classification was delivered.
	SettledSupervisionCrashReason(ctx context.Context) (reason string, ok bool)
}

// SettledSupervisionCrashReason returns the journal-delivered crash
// classification for h after settling the journal's exit fact, bounded by
// budget. Handles without the settler capability answer immediately from
// their existing journal flow (no settle — remote peers deliver their
// journal facts through the Supervise stream, which carries no flip
// window). The budget must come from tunables, not be declared at call
// sites.
func SettledSupervisionCrashReason(ctx context.Context, h Handle, budget time.Duration) (string, bool) {
	if settler, isSettler := h.(JournalCrashSettler); isSettler {
		// WithoutCancel: the settle stays bounded by budget and must run
		// even when the caller's ctx is already canceled — a run that
		// aborts its step ctx on transport death still needs the journal
		// classification for its crash decision.
		settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), budget)
		defer cancel()
		reason, ok := settler.SettledSupervisionCrashReason(settleCtx)
		if !ok {
			return "", false
		}
		if !isTaxonomyReason(reason) {
			return "", false
		}
		return reason, true
	}
	return SupervisionCrashReason(h)
}

// isTaxonomyReason reports whether reason is one of the CrashReason*
// constants — the exact vocabulary classifySessionCrash documents as its
// return values.
func isTaxonomyReason(reason string) bool {
	switch reason {
	case CrashReasonProcessExitedEarly,
		CrashReasonUnknown,
		CrashReasonHeartbeatStall,
		CrashReasonTransportClosed,
		CrashReasonEndpointUnavailable,
		CrashReasonStdioPipeBroken,
		CrashReasonStdioEOF,
		CrashReasonProcessTerminated,
		CrashReasonUnknownAdapterError,
		CrashReasonChildRunLost:
		return true
	}
	return false
}
