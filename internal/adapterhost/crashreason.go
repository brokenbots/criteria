package adapterhost

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
)

// SupervisedHandle is the optional Handle capability a peer-supervised
// handle implements (ADR-0007, T-07): the peer's supervision journal
// delivered a terminal classification for the adapter child, and the host
// consumes that classification verbatim instead of guessing.
//
// The reason value is a CrashClassified.reason wire fact — one of the
// CrashReason* constants above, the same taxonomy the host's own classifier
// uses, emitted by the peer's child-side journal (T-05). Because the peer
// watched the child directly, its classification outranks every host-side
// heuristic: classifySessionCrash consults this seam before the
// ProcessExited branch and before the legacy string matching, which remain
// the fallbacks for legacy-runner connections and for peers whose
// Supervise stream is unavailable.
type SupervisedHandle interface {
	// SupervisionCrashReason returns the journal-delivered crash reason and
	// ok=true. ok=false means no terminal classification was delivered (the
	// child is alive, exited cleanly, or only the plain Exited journal
	// record arrived) — classification then falls through to the local
	// evidence path.
	SupervisionCrashReason() (reason string, ok bool)
}

// SupervisionCrashReason returns the crash classification the peer
// supervision journal delivered for h's adapter child (verbatim wire fact),
// or ok=false when the handle carries no supervision classification —
// including when the delivered reason is empty or outside the CrashReason*
// taxonomy (wire garbage or host/peer version skew): the classification
// path is documented to yield taxonomy constants only, so anything else
// falls through to the local evidence path.
func SupervisionCrashReason(h Handle) (string, bool) {
	sh, ok := h.(SupervisedHandle)
	if !ok || sh == nil {
		return "", false
	}
	reason, ok := sh.SupervisionCrashReason()
	if !ok || !isTaxonomyReason(reason) {
		return "", false
	}
	return reason, true
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
		CrashReasonUnknownAdapterError:
		return true
	}
	return false
}
