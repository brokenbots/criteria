package adapterhost

import (
	"errors"
	"sync/atomic"
	"testing"
)

// crashReasonTaxonomy enumerates every exported crash-reason constant. The
// test pins the set, not just individual values: classifySessionCrash must
// never return a string outside this taxonomy, and peer supervision journal
// emission (peer.proto CrashClassified.reason, ADR-0007 Stage A) consumes the
// same set.
func crashReasonTaxonomy() map[string]string {
	return map[string]string{
		"ProcessExitedEarly":  CrashReasonProcessExitedEarly,
		"Unknown":             CrashReasonUnknown,
		"HeartbeatStall":      CrashReasonHeartbeatStall,
		"TransportClosed":     CrashReasonTransportClosed,
		"EndpointUnavailable": CrashReasonEndpointUnavailable,
		"StdioPipeBroken":     CrashReasonStdioPipeBroken,
		"StdioEOF":            CrashReasonStdioEOF,
		"ProcessTerminated":   CrashReasonProcessTerminated,
		"UnknownAdapterError": CrashReasonUnknownAdapterError,
	}
}

// TestCrashReasonTaxonomy pins the exported const set: every value is
// non-empty and distinct, so no two classifications can collide on the wire
// or in logs.
func TestCrashReasonTaxonomy(t *testing.T) {
	seen := make(map[string]string, len(crashReasonTaxonomy()))
	for name, reason := range crashReasonTaxonomy() {
		if reason == "" {
			t.Errorf("CrashReason%s is empty", name)
		}
		if dup, ok := seen[reason]; ok {
			t.Errorf("CrashReason%s duplicates CrashReason%s (%q)", name, dup, reason)
		}
		seen[reason] = name
	}
}

// exitedHandle is a minimal Handle that reports a process exit, exercising
// the ProcessExited branch of classifySessionCrash (which the message-
// heuristic cases above it cannot reach without a real subprocess).
type exitedHandle struct {
	Handle
	exited atomic.Bool
}

func (h *exitedHandle) ProcessExited() bool { return h.exited.Load() }

// TestClassifySessionCrashUsesTaxonomy proves every classifySessionCrash
// return path yields a member of the exported taxonomy (single source of
// truth: the classifier cannot drift off the const set).
func TestClassifySessionCrashUsesTaxonomy(t *testing.T) {
	taxonomy := crashReasonTaxonomy()
	members := make(map[string]bool, len(taxonomy))
	for _, reason := range taxonomy {
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
