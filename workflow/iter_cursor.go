package workflow

import (
	"encoding/json"

	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"
)

// IterCursor tracks the active state of a step-level iteration loop
// (for_each or count).
//
// The server stores cursors opaquely inside the runs.variable_scope JSON blob;
// only the agent interprets their contents. Field documentation is authoritative
// for the Phase 1.6 SDK extraction.
//
// Items and Keys are NOT persisted — on reattach the step re-evaluates the
// for_each/count expression with the restored var scope.
type IterCursor struct {
	// StepName is the name of the iterating step owning this cursor.
	StepName string
	// Items holds the evaluated list or map values for the current iteration
	// run. Nil when restored from crash-recovery scope (re-evaluated on first
	// step entry). An empty slice means the evaluated collection was empty.
	Items []cty.Value
	// Keys holds the map keys when iterating over an HCL object/map. Its
	// length matches Items. For list/count iteration, Keys is nil and the
	// numeric index is used as each.key instead.
	Keys []cty.Value
	// Index is the zero-based index of the NEXT iteration to dispatch.
	// Incremented by the engine loop after each iteration completes.
	Index int
	// Total is the total number of items to iterate. Populated when Items is
	// set; used for each._total and each._last bindings.
	Total int
	// Key is the map key for the current iteration (for map for_each). Its
	// type is cty.String. For list/count iteration, Key equals
	// cty.StringVal(strconv.Itoa(Index)).
	Key cty.Value
	// AnyFailed is true if at least one prior iteration produced a non-success
	// outcome. Cleared when on_failure == "ignore".
	AnyFailed bool
	// InProgress is true while the per-iteration step body is executing.
	// On crash recovery, a true value means the step needs to be replayed.
	InProgress bool
	// OnFailure is the on_failure value from the step spec. Stored here so the
	// engine does not need to re-read the step node on every advance.
	// Values: "" or "continue" (default), "abort", "ignore".
	OnFailure string
	// Prev holds the output object of the most recently completed iteration
	// (each._prev). Set to the adapter response map (for adapter steps) or the
	// evaluated output{} block values (for workflow-type steps). cty.NilVal
	// before the first iteration completes.
	Prev cty.Value
}

// SerializeIterCursor encodes the cursor to a JSON string suitable for
// transmission via ScopeIterCursorSet. A nil cursor returns "" (signals
// "clear the cursor").
func SerializeIterCursor(cursor *IterCursor) (string, error) {
	if cursor == nil || cursor.StepName == "" {
		return "", nil
	}
	m := map[string]interface{}{
		"step":        cursor.StepName,
		"index":       cursor.Index,
		"total":       cursor.Total,
		"any_failed":  cursor.AnyFailed,
		"in_progress": cursor.InProgress,
	}
	if cursor.OnFailure != "" {
		m["on_failure"] = cursor.OnFailure
	}
	if cursor.Key != cty.NilVal {
		m["key"] = CtyValueToString(cursor.Key)
	}
	if len(cursor.Keys) > 0 {
		keys := make([]string, len(cursor.Keys))
		for i, k := range cursor.Keys {
			keys[i] = CtyValueToString(k)
		}
		m["keys"] = keys
	}
	if cursor.Prev != cty.NilVal {
		b, err := ctyjson.Marshal(cursor.Prev, cursor.Prev.Type())
		if err == nil {
			var v interface{}
			if err2 := json.Unmarshal(b, &v); err2 == nil {
				m["prev"] = v
			}
		}
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// DeserializeIterCursor parses a JSON cursor string produced by
// SerializeIterCursor and returns the reconstructed IterCursor. An empty
// string returns a zero-value cursor without error (caller should treat it
// as "no cursor"). Use this when you need to inspect the cursor outside the
// workflow package (e.g. in tests).
func DeserializeIterCursor(data string) (*IterCursor, error) {
	if data == "" {
		return &IterCursor{}, nil
	}
	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(data), &raw); err != nil {
		return nil, err
	}
	cur := deserializeIterCursor(raw)
	return &cur, nil
}

// deserializeIterCursor reconstructs an IterCursor from the wire JSON map
// produced by SerializeIterCursor (or legacy W07 format with "node" key).
func deserializeIterCursor(raw map[string]interface{}) IterCursor {
	cur := IterCursor{}
	// Support both W10 "step" key and legacy W07 "node" key.
	if v, ok := raw["step"].(string); ok && v != "" {
		cur.StepName = v
	} else if v, ok := raw["node"].(string); ok {
		cur.StepName = v
	}
	if v, ok := raw["index"].(float64); ok {
		cur.Index = int(v)
	}
	if v, ok := raw["total"].(float64); ok {
		cur.Total = int(v)
	}
	if v, ok := raw["key"].(string); ok && v != "" {
		cur.Key = cty.StringVal(v)
	}
	if rawKeys, ok := raw["keys"].([]interface{}); ok {
		cur.Keys = make([]cty.Value, 0, len(rawKeys))
		for _, rk := range rawKeys {
			if s, ok := rk.(string); ok {
				cur.Keys = append(cur.Keys, cty.StringVal(s))
			}
		}
	}
	if v, ok := raw["any_failed"].(bool); ok {
		cur.AnyFailed = v
	}
	if v, ok := raw["in_progress"].(bool); ok {
		cur.InProgress = v
	}
	if v, ok := raw["on_failure"].(string); ok {
		cur.OnFailure = v
	}
	cur.Prev = deserializePrev(raw["prev"])
	return cur
}

// deserializePrev rebuilds the each._prev cty object from the JSON "prev" map.
// The value is always a flat map of string attributes (adapter outputs or
// output{} block values), so we reconstruct a cty object with string attrs.
// Returns cty.NilVal when prev is absent or empty.
func deserializePrev(raw interface{}) cty.Value {
	prevRaw, ok := raw.(map[string]interface{})
	if !ok || len(prevRaw) == 0 {
		return cty.NilVal
	}
	attrs := make(map[string]cty.Value, len(prevRaw))
	for k, v := range prevRaw {
		if s, ok := v.(string); ok {
			attrs[k] = cty.StringVal(s)
		}
	}
	if len(attrs) == 0 {
		return cty.NilVal
	}
	return cty.ObjectVal(attrs)
}
