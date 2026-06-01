package adapter

// FailureWithContext is implemented by structured error values an adapter
// returns when a partial-failure scenario occurs mid-execution. The host uses
// errors.As to extract phase + event index for routing.
type FailureWithContext interface {
	error
	// EventIndex is the zero-based index of the last successfully delivered
	// event before the failure. Returns -1 when no events were delivered.
	EventIndex() int
	// Phase is a short identifier for the lifecycle phase in which the
	// failure occurred: "open", "execute", "close".
	Phase() string
}
