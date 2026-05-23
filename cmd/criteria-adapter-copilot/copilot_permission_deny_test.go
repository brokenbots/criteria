// copilot_permission_deny_test.go — denial-path tests for handlePermissionRequest.
// WS03: permissions are auto-approved; this file covers the paths that return
// UserNotAvailable (no session, inactive session, or failed observability send)
// and verifies that a working active session returns Approved.

package main

import (
	"errors"
	"testing"

	copilot "github.com/github/copilot-sdk/go"

	v2 "github.com/brokenbots/criteria/sdk/pb/criteria/v2"
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

// TestHandlePermissionRequestSendError verifies that a sink.Send failure causes
// the adapter to return UserNotAvailable instead of Approved — fail closed so
// that a tool action never proceeds when the only in-scope observability event
// cannot be recorded.
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
	if err != nil {
		t.Fatalf("unexpected error (non-nil means SDK-level failure): %v", err)
	}
	if result.Kind != copilot.PermissionRequestResultKindUserNotAvailable {
		t.Fatalf("result.Kind = %q, want %q (fail-closed when observability send fails)", result.Kind, copilot.PermissionRequestResultKindUserNotAvailable)
	}
}

// TestHandlePermissionRequestActiveSessionApproved verifies the WS03 auto-approve
// stub: an active session with a valid sink emits a permission.request AdapterEvent
// and returns Approved. WS16 adds interactive grant/deny via the bidi Permissions
// stream.
func TestHandlePermissionRequestActiveSessionApproved(t *testing.T) {
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
	if result.Kind != copilot.PermissionRequestResultKindApproved {
		t.Fatalf("result.Kind = %q, want %q", result.Kind, copilot.PermissionRequestResultKindApproved)
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
