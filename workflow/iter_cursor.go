package workflow

import (
	"encoding/json"

	"github.com/zclconf/go-cty/cty"
)

// IterCursor tracks the active state of a for_each node's iteration loop.
//
// The server stores the cursor opaquely as the "iter" field inside the
// runs.variable_scope JSON blob; only the agent interprets its contents.
// Field documentation is authoritative for the Phase 1.6 SDK extraction.
//
// Items is populated at runtime when the for_each node is first entered (or
// re-entered after crash recovery). It is intentionally NOT persisted in the
// scope JSON — on reattach the for_each node re-evaluates the items expression
// with the restored var scope, ensuring correctness even if variable inputs
// changed between runs.
type IterCursor struct {
	// NodeName is the name of the for_each node owning this cursor.
	NodeName string
	// Items holds the evaluated list elements for the current iteration run.
	// Nil when restored from crash-recovery scope (re-evaluated on first entry).
	// An empty slice means the list evaluated to zero items (distinct from nil).
	Items []cty.Value
	// Index is the zero-based index of the next iteration to dispatch.
	// Incremented by the engine loop after each _continue interception.
	Index int
	// AnyFailed is true if at least one prior iteration produced a non-success
	// outcome that transitioned back via _continue.
	AnyFailed bool
	// InProgress is true while the per-iteration step is executing.
	// On crash recovery, a true value means the step needs to be replayed.
	InProgress bool
}

// SerializeIterCursor encodes the cursor to a JSON string suitable for
// transmission via ScopeIterCursorSet. A nil cursor returns an empty string
// (signals "clear the cursor"). The server stores this verbatim without
// interpreting the field names, preserving 1.6 split independence.
func SerializeIterCursor(cursor *IterCursor) (string, error) {
	if cursor == nil {
		return "", nil
	}
	m := map[string]interface{}{
		"node":        cursor.NodeName,
		"index":       cursor.Index,
		"any_failed":  cursor.AnyFailed,
		"in_progress": cursor.InProgress,
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
