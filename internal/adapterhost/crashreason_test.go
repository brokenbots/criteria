package adapterhost

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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
		"CrashReasonChildRunLost":        CrashReasonChildRunLost,
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
		"CrashReasonChildRunLost":        "workflow.v1 child run was in flight when the peer connection was lost",
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
// the ProcessExited branch of classifySessionCrash (which a session with
// no supervision fact cannot reach without a real subprocess).
type exitedHandle struct {
	Handle
	exited atomic.Bool
}

func (h *exitedHandle) ProcessExited() bool { return h.exited.Load() }

// settledOnlySupervisedHandle is a supervised handle whose plain query
// reports no fact (the supervision journal has not recorded the crash
// yet) while its settled query delivers one — the flip window between a
// transport death and the journal's exit fact, exposed for classification
// tests. Embedding supervisedCrashHandle gives it the SupervisedHandle
// shape with an empty plain query; only SettledSupervisionCrashReason is
// overridden.
type settledOnlySupervisedHandle struct {
	*supervisedCrashHandle

	mu      sync.Mutex
	settled string
}

func (h *settledOnlySupervisedHandle) SettledSupervisionCrashReason(context.Context) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.settled == "" {
		return "", false
	}
	return h.settled, true
}

func (h *settledOnlySupervisedHandle) deliverSettled(reason string) {
	h.mu.Lock()
	h.settled = reason
	h.mu.Unlock()
}

// TestClassifySessionCrashUsesTaxonomy proves every classifySessionCrash
// return path yields a member of the exported taxonomy (single source of
// truth: the classifier cannot drift off the const set), and pins the M5
// classification source: error text is never evidence, so the deleted
// message heuristics fall through to the unknown-adapter-error class.
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
		// M5: classification is journal-only. These texts were once the
		// string-matching crash arms; with the handle carrying no fact,
		// they are ordinary adapter errors and classify nothing.
		{"stdio EOF text without supervision fact", nil, errors.New("EOF"), CrashReasonUnknownAdapterError},
		{"transport-close text without supervision fact", nil, errors.New("rpc error: transport is closing"), CrashReasonUnknownAdapterError},
		{"fallback", nil, errors.New("bizarre failure"), CrashReasonUnknownAdapterError},
		{"process exited", &Session{handle: exited}, errors.New("anything"), CrashReasonProcessExitedEarly},
		// The watchdog stall evidence rides the error itself, and outranks
		// the structural exit: a fresh-exit handle with a stall carrier
		// classifies as the stall, not as the exit.
		{
			name:    "watchdog stall evidence",
			sess:    &Session{handle: &exitedHandle{}},
			execErr: &watchdogStallError{threshold: time.Second},
			want:    CrashReasonHeartbeatStall,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifySessionCrash(context.Background(), tc.sess, tc.execErr)
			if got != tc.want {
				t.Errorf("classifySessionCrash() = %q, want %q", got, tc.want)
			}
			if !members[got] {
				t.Errorf("classifySessionCrash() returned %q, which is not a member of the crash-reason taxonomy", got)
			}
		})
	}
}

// TestClassifySessionCrashJournalDeliveryOnly proves the M5 classification
// source: every crash class is reachable through the journal-delivery seam
// (the peer's CrashClassified wire facts, served verbatim by the handle's
// SupervisionCrashReason), and — for a supervised handle in the flip
// window — through the settled query. Nothing else classifies: the same
// transport-shaped error text against a fact-free handle is not a crash.
func TestClassifySessionCrashJournalDeliveryOnly(t *testing.T) {
	// Transport-shaped text that the deleted string arms once matched, fed
	// alongside every delivered fact: the fact must win over the text.
	legacyText := errors.New("rpc error: code = Unavailable desc = transport is closing")

	for name, reason := range crashReasonConsts() {
		t.Run("delivered fact "+name, func(t *testing.T) {
			_, sess, h := newSupervisedSession()
			h.deliver(reason)
			if got := classifySessionCrash(context.Background(), sess, legacyText); got != reason {
				t.Errorf("classifySessionCrash() = %q, want delivered fact %q verbatim", got, reason)
			}
		})
	}

	t.Run("settled query bridges the flip window", func(t *testing.T) {
		h := &settledOnlySupervisedHandle{
			supervisedCrashHandle: &supervisedCrashHandle{
				cri287Handle: &cri287Handle{name: "fake", deadAfter: 0},
			},
		}
		sess := &Session{Name: "shell.settled", Adapter: "shell", handle: h}

		// Before the settled fact lands the handle carries nothing, so the
		// transport-shaped error stays an ordinary adapter error.
		if got := classifySessionCrash(context.Background(), sess, legacyText); got != CrashReasonUnknownAdapterError {
			t.Errorf("pre-settle classifySessionCrash() = %q, want %q", got, CrashReasonUnknownAdapterError)
		}

		h.deliverSettled(CrashReasonTransportClosed)
		if got := classifySessionCrash(context.Background(), sess, legacyText); got != CrashReasonTransportClosed {
			t.Errorf("post-settle classifySessionCrash() = %q, want %q from the settled journal query", got, CrashReasonTransportClosed)
		}
	})
}
