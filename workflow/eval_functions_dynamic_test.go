package workflow_test

// eval_functions_dynamic_test.go — tests for uuid() and timestamp() HCL
// functions.

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// ── uuid ──────────────────────────────────────────────────────────────────────

func TestUUID_FormatRFC4122(t *testing.T) {
	fn := funcFromContext(t, "uuid")
	got := callFn(t, fn)
	s := got.AsString()
	if len(s) != 36 {
		t.Errorf("uuid(): expected 36 chars, got %d: %q", len(s), s)
	}
	// Hyphens at positions 8, 13, 18, 23.
	for _, pos := range []int{8, 13, 18, 23} {
		if s[pos] != '-' {
			t.Errorf("uuid(): expected '-' at position %d, got %q in %q", pos, s[pos], s)
		}
	}
	if _, err := uuid.Parse(s); err != nil {
		t.Errorf("uuid(): result %q does not parse as UUID: %v", s, err)
	}
}

func TestUUID_NonDeterministic(t *testing.T) {
	fn := funcFromContext(t, "uuid")
	v1 := callFn(t, fn)
	v2 := callFn(t, fn)
	if v1.AsString() == v2.AsString() {
		t.Errorf("uuid(): two successive calls returned the same value %q", v1.AsString())
	}
}

// ── timestamp ─────────────────────────────────────────────────────────────────

func TestTimestamp_FormatRFC3339(t *testing.T) {
	fn := funcFromContext(t, "timestamp")
	got := callFn(t, fn)
	s := got.AsString()
	if _, err := time.Parse(time.RFC3339, s); err != nil {
		t.Errorf("timestamp(): result %q does not parse as RFC3339: %v", s, err)
	}
}

func TestTimestamp_NonRegressing(t *testing.T) {
	fn := funcFromContext(t, "timestamp")
	v1 := callFn(t, fn)
	time.Sleep(10 * time.Millisecond)
	v2 := callFn(t, fn)

	t1, err := time.Parse(time.RFC3339, v1.AsString())
	if err != nil {
		t.Fatalf("parse t1: %v", err)
	}
	t2, err := time.Parse(time.RFC3339, v2.AsString())
	if err != nil {
		t.Fatalf("parse t2: %v", err)
	}
	// RFC3339 has second-level precision; t2 >= t1 is guaranteed only when
	// the sleep crosses a second boundary.  Accept equality as valid.
	if t2.Before(t1) {
		t.Errorf("timestamp() not monotonic: t1=%s t2=%s", t1, t2)
	}
}

// TestTimestamp_IsUTC verifies the returned timestamp uses the UTC offset.
func TestTimestamp_IsUTC(t *testing.T) {
	fn := funcFromContext(t, "timestamp")
	got := callFn(t, fn)
	s := got.AsString()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	_, offset := ts.Zone()
	if offset != 0 {
		t.Errorf("timestamp(): expected UTC (offset 0), got offset %d in %q", offset, s)
	}
}

// callFn with no args is needed here; the helpers file supports variadic args.
