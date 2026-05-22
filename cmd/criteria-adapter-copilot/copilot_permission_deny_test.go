// copilot_permission_deny_test.go — non-happy-path tests for handlePermissionRequest.
// Covers the failure scenarios:
//   - no session found   → PermissionRequestResultKindUserNotAvailable
//   - session inactive   → PermissionRequestResultKindUserNotAvailable
//   - sink.Send failure  → PermissionRequestResultKindUserNotAvailable + non-nil error
//
// In v2 the adapter always denies at the SDK level so the tool never runs;
// the host applies PermissionPolicy via its captureSink and overrides the step
// outcome to "needs_review" when a tool is denied. WS16 adds interactive grant/deny.

package main

import (
	"errors"
	"testing"

	copilot "github.com/github/copilot-sdk/go"

	v2 "github.com/brokenbots/criteria/proto/criteria/v2"
)

// failSender is an ExecuteEventSender that always returns the configured error.
type failSender struct {
	err error
}

func (f *failSender) Send(_ *v2.ExecuteEvent) error {
	return f.err
}

// TestHandlePermissionRequestNoSession asserts that an unknown session ID
// returns UserNotAvailable with no error and sends no event.
func TestHandlePermissionRequestNoSession(t *testing.T) {
	p := &copilotAdapter{sessions: map[string]*sessionState{}}
	req := copilot.PermissionRequestShell{}

	result, err := p.handlePermissionRequest("nonexistent", req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Kind != copilot.PermissionRequestResultKindUserNotAvailable {
		t.Fatalf("result.Kind = %q, want %q", result.Kind, copilot.PermissionRequestResultKindUserNotAvailable)
	}
}

// TestHandlePermissionRequestInactiveSession asserts that an inactive session
// (active=false) returns UserNotAvailable with no error and sends no events,
// even when a recording sink is wired up (distinguishing the active=false branch
// from the sink=nil branch).
func TestHandlePermissionRequestInactiveSession(t *testing.T) {
	sink := &recordingSender{}
	s := &sessionState{
		session: &fakeSession{},
		active:  false,
		sink:    sink,
	}
	p := &copilotAdapter{sessions: map[string]*sessionState{"s1": s}}
	req := copilot.PermissionRequestShell{}

	result, err := p.handlePermissionRequest("s1", req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Kind != copilot.PermissionRequestResultKindUserNotAvailable {
		t.Fatalf("result.Kind = %q, want %q", result.Kind, copilot.PermissionRequestResultKindUserNotAvailable)
	}
	if got := sink.snapshot(); len(got) != 0 {
		t.Fatalf("expected no events sent on sink, got %d event(s)", len(got))
	}
}

// TestHandlePermissionRequestSendError asserts that a sink.Send failure returns
// UserNotAvailable and propagates the send error to the caller.
func TestHandlePermissionRequestSendError(t *testing.T) {
	sendErr := errors.New("connection closed")
	s := &sessionState{
		session: &fakeSession{},
		active:  true,
		sink:    &failSender{err: sendErr},
	}
	p := &copilotAdapter{sessions: map[string]*sessionState{"s1": s}}
	req := copilot.PermissionRequestShell{}

	result, err := p.handlePermissionRequest("s1", req)
	if err == nil {
		t.Fatal("expected non-nil error when sink.Send fails, got nil")
	}
	if !errors.Is(err, sendErr) {
		t.Fatalf("error = %v, want wrapping %v", err, sendErr)
	}
	if result.Kind != copilot.PermissionRequestResultKindUserNotAvailable {
		t.Fatalf("result.Kind = %q, want %q", result.Kind, copilot.PermissionRequestResultKindUserNotAvailable)
	}
}

// TestHandlePermissionRequestActiveSessionDenied verifies the v2 deny-first
// behavior: an active session with a valid sink emits a permission.request
// AdapterEvent and returns Rejected. The host applies PermissionPolicy separately
// and overrides the step outcome to "needs_review" when a tool is denied.
func TestHandlePermissionRequestActiveSessionDenied(t *testing.T) {
	sender := &recordingSender{}
	s := &sessionState{
		session: &fakeSession{},
		active:  true,
		sink:    sender,
	}
	p := &copilotAdapter{sessions: map[string]*sessionState{"s1": s}}

	req := copilot.PermissionRequestShell{}
	result, err := p.handlePermissionRequest("s1", req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Kind != copilot.PermissionRequestResultKindRejected {
		t.Fatalf("result.Kind = %q, want %q", result.Kind, copilot.PermissionRequestResultKindRejected)
	}

	var found bool
	for _, ev := range sender.snapshot() {
		if a := ev.GetAdapter(); a != nil && a.GetEventKind() == "permission.request" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected permission.request adapter event on sink")
	}
}
