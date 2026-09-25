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
