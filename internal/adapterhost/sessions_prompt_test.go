package adapterhost

// sessions_prompt_test.go — SessionManager.Prompt gate and delivery semantics
// at the session level with in-memory fake handles (CRI-259, ADR-0006 D3/D9).
//
// The capability gate must short-circuit with ErrPromptUnsupportedAdapter
// without dispatching the Prompt RPC, and the positive path must issue
// exactly one Prompt call and forward the adapter's response. The live
// out-of-process equivalents run against the promptable/noop fixtures in
// internal/engine/prompt_live_test.go and
// internal/adapter/conformance/promptable_adapter_test.go.

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/brokenbots/criteria/workflow"
)

// promptCountingHandle embeds the inert snapshot mock and counts Prompt
// dispatches, so the gate-vs-dispatch boundary is observable.
type promptCountingHandle struct {
	*snapshotMockHandle

	mu          sync.Mutex
	promptCalls int
	accepted    bool
	detail      string
}

func (h *promptCountingHandle) Prompt(_ context.Context, req *PromptRequest) (*PromptResponse, error) {
	h.mu.Lock()
	h.promptCalls++
	accepted, detail := h.accepted, h.detail
	h.mu.Unlock()
	return &PromptResponse{Accepted: accepted, Detail: detail}, nil
}

func (h *promptCountingHandle) callCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.promptCalls
}

// failingPromptLoader always fails to resolve, so the lazy bind of a
// verified-only session record surfaces NO_ACTIVE_SESSION.
type failingPromptLoader struct{}

func (failingPromptLoader) Resolve(context.Context, string) (Handle, error) {
	return nil, errors.New("no adapter binary available")
}
func (failingPromptLoader) Shutdown(context.Context) error { return nil }

func promptCountingSession(t *testing.T, name string, caps []string, accepted bool, detail string) (*SessionManager, *promptCountingHandle) {
	t.Helper()
	h := &promptCountingHandle{snapshotMockHandle: &snapshotMockHandle{}, accepted: accepted, detail: detail}
	sm := NewSessionManager(nil)
	s := makeTestSession(sm, name, h, nil)
	s.Capabilities = caps
	return sm, h
}

// TestSessionPromptCapabilityGateShortCircuitsRPC verifies the
// UNSUPPORTED_ADAPTER short-circuit issues no Prompt RPC: a session whose
// adapter capabilities lack supports_prompt must fail with the typed error
// and zero prompt dispatches, even though the handle is prompt-capable.
func TestSessionPromptCapabilityGateShortCircuitsRPC(t *testing.T) {
	sm, h := promptCountingSession(t, "sess-gated", []string{"parallel_safe"}, true, "")

	_, err := sm.Prompt(context.Background(), "sess-gated", &workflow.StepNode{}, "", "hello")
	if !errors.Is(err, ErrPromptUnsupportedAdapter) {
		t.Fatalf("err = %v, want ErrPromptUnsupportedAdapter", err)
	}
	if calls := h.callCount(); calls != 0 {
		t.Fatalf("Prompt RPC dispatched %d time(s) despite capability gate", calls)
	}
}

// TestSessionPromptNoSessionForUnknownSessionName verifies the
// NO_ACTIVE_SESSION branch: a prompt addressed to a capability-carrying but
// unbound adapter session fails with the typed error and no dispatch. The
// verified-only record (lazy bind) fails to bind because the loader cannot
// resolve the adapter, surfacing NO_ACTIVE_SESSION deterministically.
func TestSessionPromptNoSessionForUnknownSessionName(t *testing.T) {
	sm, h := promptCountingSession(t, "sess-live", []string{"parallel_safe", PromptCapability}, true, "")
	sm.loader = &failingPromptLoader{}
	if err := sm.storeVerifiedRecord("sess-unbound", "promptable", OnCrashFail, nil, nil, nil, "", []string{"parallel_safe", PromptCapability}, "", ""); err != nil {
		t.Fatalf("storeVerifiedRecord: %v", err)
	}

	_, err := sm.Prompt(context.Background(), "sess-unbound", &workflow.StepNode{}, "", "hello")
	if !errors.Is(err, ErrPromptNoActiveSession) {
		t.Fatalf("err = %v, want ErrPromptNoActiveSession", err)
	}
	if calls := h.callCount(); calls != 0 {
		t.Fatalf("Prompt RPC dispatched %d time(s) with no live session", calls)
	}
}

// TestSessionPromptSessionMismatch verifies the SESSION_MISMATCH branch: an
// accepted session id that does not match the live session is an error, not
// silently ignored.
func TestSessionPromptSessionMismatch(t *testing.T) {
	sm, h := promptCountingSession(t, "sess-live", []string{"parallel_safe", PromptCapability}, true, "")

	_, err := sm.Prompt(context.Background(), "sess-live", &workflow.StepNode{}, "sess-other", "hello")
	if !errors.Is(err, ErrPromptSessionMismatch) {
		t.Fatalf("err = %v, want ErrPromptSessionMismatch", err)
	}
	if calls := h.callCount(); calls != 0 {
		t.Fatalf("Prompt RPC dispatched %d time(s) on session mismatch", calls)
	}
}

// TestSessionPromptDeliversThroughSeam covers the positive seam at session
// level: a prompt-capable session receives exactly one Prompt dispatch, and
// the manager returns the delivered session id.
func TestSessionPromptDeliversThroughSeam(t *testing.T) {
	sm, h := promptCountingSession(t, "sess-live", []string{"parallel_safe", PromptCapability}, true, "")

	sessionID, err := sm.Prompt(context.Background(), "sess-live", &workflow.StepNode{}, "", "hello")
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if sessionID != "sess-live" {
		t.Fatalf("delivered session id = %q, want %q", sessionID, "sess-live")
	}
	if calls := h.callCount(); calls != 1 {
		t.Fatalf("Prompt calls = %d, want 1", calls)
	}
}

// TestSessionPromptAdapterRejectionForwardsDetail covers the adapter-rejected
// branch: accepted=false forwards the adapter's detail via PromptRejectedError
// (ADR-0006 D9: the adapter's detail is the failure reason).
func TestSessionPromptAdapterRejectionForwardsDetail(t *testing.T) {
	sm, h := promptCountingSession(t, "sess-busy", []string{"parallel_safe", PromptCapability}, false, "adapter is mid-commit")

	sessionID, err := sm.Prompt(context.Background(), "sess-busy", &workflow.StepNode{}, "", "hello")
	if sessionID != "sess-busy" {
		t.Fatalf("session id = %q, want %q", sessionID, "sess-busy")
	}
	if calls := h.callCount(); calls != 1 {
		t.Fatalf("Prompt calls = %d, want 1 (adapter rejection still requires the RPC round trip)", calls)
	}
	var rejected *PromptRejectedError
	if !errors.As(err, &rejected) {
		t.Fatalf("err = %v (%T), want *PromptRejectedError", err, err)
	}
	if rejected.Detail != "adapter is mid-commit" {
		t.Fatalf("detail = %q, want adapter's forwarded detail", rejected.Detail)
	}
}
