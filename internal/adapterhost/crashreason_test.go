package adapterhost

import (
	"errors"
	"sync/atomic"
	"testing"
)

// crashReasonConsts enumerates every exported CrashReason* constant. The
// compile-time references fail the build if a constant is renamed or
// removed; TestCrashReasonTaxonomy's cardinality check fails if a new
// constant is added without a pin entry in both tables.
func crashReasonConsts() map[string]string {
	return map[string]string{
		"CrashReasonProcessExitedEarly":  CrashReasonProcessExitedEarly,
		"CrashReasonUnknown":             CrashReasonUnknown,
		"CrashReasonHeartbeatStall":      CrashReasonHeartbeatStall,
		"CrashReasonTransportClosed":     CrashReasonTransportClosed,
		"CrashReasonEndpointUnavailable": CrashReasonEndpointUnavailable,
		"CrashReasonStdioPipeBroken":     CrashReasonStdioPipeBroken,
		"CrashReasonStdioEOF":            CrashReasonStdioEOF,
		"CrashReasonProcessTerminated":   CrashReasonProcessTerminated,
		"CrashReasonUnknownAdapterError": CrashReasonUnknownAdapterError,
	}
}

// crashReasonLiterals pins each constant to the exact historical value
// classifySessionCrash returned before the taxonomy extraction (CRI-271).
// These strings are stable wire/log vocabulary: session.crash sink events,
// inspect payloads, and peer supervision journal CrashClassified.reason
// events (peer.proto, ADR-0007 Stage A) all carry them verbatim, so a
// reword of any constant must fail this test.
func crashReasonLiterals() map[string]string {
	return map[string]string{
		"CrashReasonProcessExitedEarly":  "adapter process exited before the call completed",
		"CrashReasonUnknown":             "unknown",
		"CrashReasonHeartbeatStall":      "log-stream heartbeat stall (adapter stopped streaming)",
		"CrashReasonTransportClosed":     "gRPC client transport closed (adapter or shim closed the connection)",
		"CrashReasonEndpointUnavailable": "gRPC endpoint unavailable (adapter process gone)",
		"CrashReasonStdioPipeBroken":     "plugin stdio pipe broken (adapter process died)",
		"CrashReasonStdioEOF":            "plugin stdio EOF (adapter process exited or closed its stream)",
		"CrashReasonProcessTerminated":   "adapter process terminated",
		"CrashReasonUnknownAdapterError": "unknown adapter error",
	}
}

// TestCrashReasonTaxonomy pins the exported const set verbatim: every
// constant matches its historical literal, both tables cover the same set
// (cardinality parity), no value is empty, and no two constants collide —
// neither a reword nor a duplicate can slip onto the wire or into logs.
func TestCrashReasonTaxonomy(t *testing.T) {
	consts := crashReasonConsts()
	literals := crashReasonLiterals()
	if len(consts) != len(literals) {
		t.Fatalf("crash-reason taxonomy cardinality drifted: %d constants vs %d pinned literals — add an entry to both tables", len(consts), len(literals))
	}
	for name, want := range literals {
		got := consts[name]
		if got != want {
			t.Errorf("%s = %q, want pinned verbatim %q", name, got, want)
		}
	}
	seen := make(map[string]string, len(consts))
	for name, value := range consts {
		if value == "" {
			t.Errorf("%s is empty", name)
		}
		if dup, ok := seen[value]; ok {
			t.Errorf("%s duplicates %s (%q)", name, dup, value)
		}
		seen[value] = name
	}
}

// exitedHandle is a minimal Handle that reports a process exit, exercising
// the ProcessExited branch of classifySessionCrash (which the message-
// heuristic cases cannot reach without a real subprocess).
type exitedHandle struct {
	Handle
	exited atomic.Bool
}

func (h *exitedHandle) ProcessExited() bool { return h.exited.Load() }

// TestClassifySessionCrashUsesTaxonomy proves every classifySessionCrash
// return path yields a member of the exported taxonomy (single source of
// truth: the classifier cannot drift off the const set).
func TestClassifySessionCrashUsesTaxonomy(t *testing.T) {
	members := make(map[string]bool, len(crashReasonConsts()))
	for _, reason := range crashReasonConsts() {
		members[reason] = true
	}

	exited := &exitedHandle{}
	exited.exited.Store(true)

	cases := []struct {
		name    string
		sess    *Session
		execErr error
		want    string
	}{
		{"nil session nil error", nil, nil, CrashReasonUnknown},
		{"message heuristics", nil, errors.New("EOF"), CrashReasonStdioEOF},
		{"transport close", nil, errors.New("rpc error: transport is closing"), CrashReasonTransportClosed},
		{"fallback", nil, errors.New("bizarre failure"), CrashReasonUnknownAdapterError},
		{"process exited", &Session{handle: exited}, errors.New("anything"), CrashReasonProcessExitedEarly},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifySessionCrash(tc.sess, tc.execErr)
			if got != tc.want {
				t.Errorf("classifySessionCrash() = %q, want %q", got, tc.want)
			}
			if !members[got] {
				t.Errorf("classifySessionCrash() returned %q, which is not a member of the crash-reason taxonomy", got)
			}
		})
	}
}
