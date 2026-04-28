package criteria

import "github.com/brokenbots/criteria/events"

// SchemaVersion is the current event protocol version. It matches the proto
// package major version (criteria.v1). A bump requires a new proto package
// (criteria.v2) and a coordinated SDK minor release.
const SchemaVersion = events.SchemaVersion

// NewEnvelope builds an [Envelope] for runID stamped with [SchemaVersion] and
// the current UTC time. The seq field is left at zero; the orchestrator assigns
// the real value on ingest.
//
// payload must be one of the concrete payload pointer types exported by this
// package (e.g. *[RunStarted], *[StepLog]). Passing nil leaves Payload unset.
// Passing a non-nil value of an unknown type panics — this surfaces caller bugs
// at construction time rather than silently producing an empty envelope.
func NewEnvelope(runID string, payload any) *Envelope {
	return events.NewEnvelope(runID, payload)
}

// TypeString returns a stable discriminator string for env's payload
// (e.g. "step.log"). It is used as the event-type column in the orchestrator's
// event store and by tests that inspect events without reaching into the oneof.
// Returns the empty string for nil or payload-less envelopes.
func TypeString(env *Envelope) string {
	return events.TypeString(env)
}

// IsTerminal reports whether env is a terminal run event (run.completed or
// run.failed). Orchestrators use this to close WatchRun streams after the
// final event for a run.
func IsTerminal(env *Envelope) bool {
	return events.IsTerminal(env)
}
