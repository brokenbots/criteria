// copilot_permission_deny_test.go — denial-path tests for handlePermissionRequest.
// Covers the three scenarios that previously used deprecated PermissionRequestResultKind values:
//   - no session found   → PermissionRequestResultKindUserNotAvailable (was DeniedCouldNotRequestFromUser)
//   - session inactive   → PermissionRequestResultKindUserNotAvailable (was DeniedCouldNotRequestFromUser)
//   - sink.Send failure  → PermissionRequestResultKindUserNotAvailable + non-nil error (was DeniedCouldNotRequestFromUser)
//   - interactive deny   → PermissionRequestResultKindRejected (was DeniedInteractivelyByUser)

package main

import (
	"context"
	"errors"
	"testing"
	"time"

	copilot "github.com/github/copilot-sdk/go"

	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

// failSender is an executeEventSender that always returns the configured error.
type failSender struct {
	err error
}

func (f *failSender) Send(_ *pb.ExecuteEvent) error {
	return f.err
}

// TestHandlePermissionRequestNoSession asserts that an unknown session ID
// returns UserNotAvailable with no error and sends no event.
func TestHandlePermissionRequestNoSession(t *testing.T) {
	p := &copilotPlugin{sessions: map[string]*sessionState{}}
	req := &copilot.PermissionRequest{Kind: copilot.PermissionRequestKindShell}

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
		session:  &fakeSession{},
		pending:  map[string]chan permDecision{},
		active:   false,
		activeCh: make(chan struct{}),
		sink:     sink,
	}
	p := &copilotPlugin{sessions: map[string]*sessionState{"s1": s}}
	req := &copilot.PermissionRequest{Kind: copilot.PermissionRequestKindShell}

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
		session:  &fakeSession{},
		pending:  map[string]chan permDecision{},
		active:   true,
		activeCh: make(chan struct{}),
		sink:     &failSender{err: sendErr},
	}
	p := &copilotPlugin{sessions: map[string]*sessionState{"s1": s}}
	req := &copilot.PermissionRequest{Kind: copilot.PermissionRequestKindShell}

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

	// The pending entry must have been cleaned up after the send error.
	s.mu.Lock()
	pendingCount := len(s.pending)
	s.mu.Unlock()
	if pendingCount != 0 {
		t.Fatalf("pending map has %d entries after send error, want 0", pendingCount)
	}
}

// TestHandlePermissionRequestInteractiveDeny asserts that an explicit user denial
// (Allow=false via Permit) returns PermissionRequestResultKindRejected.
func TestHandlePermissionRequestInteractiveDeny(t *testing.T) {
	sender := &recordingSender{}
	s := &sessionState{
		session:  &fakeSession{},
		pending:  map[string]chan permDecision{},
		active:   true,
		activeCh: make(chan struct{}),
		sink:     sender,
	}
	p := &copilotPlugin{sessions: map[string]*sessionState{"s1": s}}

	req := &copilot.PermissionRequest{Kind: copilot.PermissionRequestKindShell}
	resCh := make(chan copilot.PermissionRequestResult, 1)
	go func() {
		result, _ := p.handlePermissionRequest("s1", req)
		resCh <- result
	}()

	// Wait for the permission-request event to arrive on the sink.
	var permID string
	deadline := time.After(300 * time.Millisecond)
	for permID == "" {
		select {
		case <-deadline:
			t.Fatal("timeout waiting for permission request event on sink")
		default:
			for _, ev := range sender.snapshot() {
				if r := ev.GetPermission(); r != nil {
					permID = r.GetId()
				}
			}
		}
	}

	_, err := p.Permit(context.Background(), &pb.PermitRequest{
		SessionId:    "s1",
		PermissionId: permID,
		Allow:        false,
		Reason:       "test deny",
	})
	if err != nil {
		t.Fatalf("Permit returned error: %v", err)
	}

	select {
	case result := <-resCh:
		if result.Kind != copilot.PermissionRequestResultKindRejected {
			t.Fatalf("result.Kind = %q, want %q", result.Kind, copilot.PermissionRequestResultKindRejected)
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("timeout waiting for permission handler result")
	}
}
