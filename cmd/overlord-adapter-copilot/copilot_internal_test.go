package main

import (
	"context"
	"sync"
	"testing"
	"time"

	pb "github.com/brokenbots/overlord/shared/pb/overlord/v1"
	copilot "github.com/github/copilot-sdk/go"
)

type recordingSender struct {
	mu     sync.Mutex
	events []*pb.ExecuteEvent
}

func (r *recordingSender) Send(event *pb.ExecuteEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
	return nil
}

func (r *recordingSender) snapshot() []*pb.ExecuteEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*pb.ExecuteEvent, len(r.events))
	copy(out, r.events)
	return out
}

type fakeSession struct {
	mu          sync.Mutex
	handlers    []copilot.SessionEventHandler
	emitOnSend  []copilot.SessionEvent
	disconnect  func() error
	destroyed   bool
	setModelErr error
	sendErr     error
}

func (f *fakeSession) On(handler copilot.SessionEventHandler) func() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers = append(f.handlers, handler)
	idx := len(f.handlers) - 1
	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		if idx >= 0 && idx < len(f.handlers) {
			f.handlers[idx] = nil
		}
	}
}

func (f *fakeSession) Send(_ context.Context, _ copilot.MessageOptions) (string, error) {
	if f.sendErr != nil {
		return "", f.sendErr
	}
	f.mu.Lock()
	handlers := append([]copilot.SessionEventHandler(nil), f.handlers...)
	events := append([]copilot.SessionEvent(nil), f.emitOnSend...)
	f.mu.Unlock()
	for _, event := range events {
		for _, handler := range handlers {
			if handler != nil {
				handler(event)
			}
		}
	}
	return "msg-1", nil
}

func (f *fakeSession) SetModel(_ context.Context, _ string, _ *copilot.SetModelOptions) error {
	return f.setModelErr
}

func (f *fakeSession) Disconnect() error {
	if f.disconnect != nil {
		return f.disconnect()
	}
	return nil
}

func (f *fakeSession) Destroy() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.destroyed = true
	return nil
}

// TestParseOutcome verifies RESULT: line extraction.
func TestParseOutcome(t *testing.T) {
	cases := []struct {
		name string
		line string
		want string
	}{
		{"exact success", "RESULT: success", "success"},
		{"trailing whitespace", "RESULT:   needs_review  ", "needs_review"},
		{"lowercase prefix", "result: failure", "failure"},
		{"mixed case", "Result: success", "success"},
		{"empty after colon defaults", "RESULT:", "needs_review"},
		{"not a result line defaults", "some log line", "needs_review"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseOutcome(tc.line)
			if got != tc.want {
				t.Errorf("parseOutcome(%q) = %q, want %q", tc.line, got, tc.want)
			}
		})
	}
}

func TestStringifyAny(t *testing.T) {
	t.Run("nil", func(t *testing.T) {
		if got := stringifyAny(nil); got != "" {
			t.Fatalf("stringifyAny(nil) = %q, want empty string", got)
		}
	})

	t.Run("map", func(t *testing.T) {
		got := stringifyAny(map[string]any{"tool": "bash"})
		if got == "" || got == "<nil>" {
			t.Fatalf("stringifyAny(map) returned empty/invalid: %q", got)
		}
	})
}

func TestPermissionDetails(t *testing.T) {
	t.Setenv(includeSensitivePermissionDetailsEnv, "")

	toolCallID := "tc-1"
	intention := "write file"
	fullCommand := "echo hi > out.txt"
	warning := "danger"
	path := "out.txt"
	cmds := []copilot.PermissionRequestCommand{{Identifier: "echo", ReadOnly: false}}

	request := copilot.PermissionRequest{
		Kind:            copilot.PermissionRequestKindShell,
		ToolCallID:      &toolCallID,
		Intention:       &intention,
		FullCommandText: &fullCommand,
		Warning:         &warning,
		Path:            &path,
		Commands:        cmds,
	}

	details := permissionDetails(request)
	if details["kind"] == "" {
		t.Fatalf("expected kind in details")
	}
	if details["tool_call_id"] != toolCallID {
		t.Fatalf("tool_call_id = %q, want %q", details["tool_call_id"], toolCallID)
	}
	if details["commands"] != "echo" {
		t.Fatalf("commands = %q, want %q", details["commands"], "echo")
	}
	if _, ok := details["request_json"]; ok {
		t.Fatalf("request_json should be redacted by default")
	}
	if _, ok := details["full_command_text"]; ok {
		t.Fatalf("full_command_text should be redacted by default")
	}
	if _, ok := details["path"]; ok {
		t.Fatalf("path should be redacted by default")
	}
}

func TestPermissionDetailsSensitiveOptIn(t *testing.T) {
	t.Setenv(includeSensitivePermissionDetailsEnv, "1")

	toolCallID := "tc-2"
	fullCommand := "echo hello > secret.txt"
	path := "secret.txt"
	request := copilot.PermissionRequest{
		Kind:            copilot.PermissionRequestKindShell,
		ToolCallID:      &toolCallID,
		FullCommandText: &fullCommand,
		Path:            &path,
	}

	details := permissionDetails(request)
	if details["request_json"] == "" {
		t.Fatalf("expected request_json when sensitive details are enabled")
	}
	if details["full_command_text"] != fullCommand {
		t.Fatalf("full_command_text = %q, want %q", details["full_command_text"], fullCommand)
	}
	if details["path"] != path {
		t.Fatalf("path = %q, want %q", details["path"], path)
	}
}

func TestResolveGitHubTokenPrecedence(t *testing.T) {
	t.Run("copilot_token_precedence", func(t *testing.T) {
		t.Setenv("COPILOT_GITHUB_TOKEN", "copilot-token")
		t.Setenv("GH_TOKEN", "gh-token")
		t.Setenv("GITHUB_TOKEN", "github-token")
		if got := resolveGitHubToken(); got != "copilot-token" {
			t.Fatalf("resolveGitHubToken() = %q, want %q", got, "copilot-token")
		}
	})

	t.Run("gh_token_fallback", func(t *testing.T) {
		t.Setenv("COPILOT_GITHUB_TOKEN", "")
		t.Setenv("GH_TOKEN", "gh-token")
		t.Setenv("GITHUB_TOKEN", "github-token")
		if got := resolveGitHubToken(); got != "gh-token" {
			t.Fatalf("resolveGitHubToken() = %q, want %q", got, "gh-token")
		}
	})

	t.Run("github_token_fallback", func(t *testing.T) {
		t.Setenv("COPILOT_GITHUB_TOKEN", "")
		t.Setenv("GH_TOKEN", "")
		t.Setenv("GITHUB_TOKEN", "github-token")
		if got := resolveGitHubToken(); got != "github-token" {
			t.Fatalf("resolveGitHubToken() = %q, want %q", got, "github-token")
		}
	})

	t.Run("empty_when_absent", func(t *testing.T) {
		t.Setenv("COPILOT_GITHUB_TOKEN", "")
		t.Setenv("GH_TOKEN", "")
		t.Setenv("GITHUB_TOKEN", "")
		if got := resolveGitHubToken(); got != "" {
			t.Fatalf("resolveGitHubToken() = %q, want empty", got)
		}
	})
}

func TestPermissionPermitHandshake(t *testing.T) {
	sender := &recordingSender{}
	s := &sessionState{
		session:  &fakeSession{},
		pending:  map[string]chan permDecision{},
		active:   true,
		activeCh: make(chan struct{}),
		sink:     sender,
	}
	p := &copilotPlugin{sessions: map[string]*sessionState{"s1": s}}

	toolCallID := "tc-123"
	request := copilot.PermissionRequest{
		Kind:       copilot.PermissionRequestKindShell,
		ToolCallID: &toolCallID,
	}

	resCh := make(chan copilot.PermissionRequestResult, 1)
	go func() {
		result, _ := p.handlePermissionRequest("s1", request)
		resCh <- result
	}()

	var permissionID string
	deadline := time.After(300 * time.Millisecond)
	for permissionID == "" {
		select {
		case <-deadline:
			t.Fatal("timeout waiting for permission request event")
		default:
			events := sender.snapshot()
			for _, ev := range events {
				if req := ev.GetPermission(); req != nil {
					permissionID = req.GetId()
					break
				}
			}
		}
	}

	_, err := p.Permit(context.Background(), &pb.PermitRequest{SessionId: "s1", PermissionId: permissionID, Allow: true})
	if err != nil {
		t.Fatalf("Permit returned error: %v", err)
	}

	select {
	case result := <-resCh:
		if result.Kind != copilot.PermissionRequestResultKindApproved {
			t.Fatalf("permission result kind = %q, want approved", result.Kind)
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("timeout waiting for permission handler result")
	}
}

func TestExecuteMaxTurnsLimit(t *testing.T) {
	fake := &fakeSession{
		emitOnSend: []copilot.SessionEvent{
			{Type: copilot.SessionEventTypeAssistantMessage, Data: &copilot.AssistantMessageData{MessageID: "m1", Content: "hello"}},
		},
	}
	p := &copilotPlugin{sessions: map[string]*sessionState{
		"s1": {session: fake, pending: map[string]chan permDecision{}},
	}}
	sender := &recordingSender{}

	err := p.Execute(context.Background(), &pb.ExecuteRequest{SessionId: "s1", Config: map[string]string{"prompt": "hi", "max_turns": "1"}}, sender)
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	hasLimitReached := false
	hasNeedsReview := false
	for _, ev := range sender.snapshot() {
		if adapter := ev.GetAdapter(); adapter != nil && adapter.GetKind() == "limit.reached" {
			hasLimitReached = true
		}
		if result := ev.GetResult(); result != nil && result.GetOutcome() == "needs_review" {
			hasNeedsReview = true
		}
	}
	if !hasLimitReached {
		t.Fatal("expected limit.reached adapter event")
	}
	if !hasNeedsReview {
		t.Fatal("expected needs_review result event")
	}
}

func TestCloseSessionTimeoutEscalatesToDestroy(t *testing.T) {
	origGrace := closeSessionGrace
	closeSessionGrace = 20 * time.Millisecond
	defer func() { closeSessionGrace = origGrace }()

	release := make(chan struct{})
	fake := &fakeSession{
		disconnect: func() error {
			<-release
			return nil
		},
	}
	p := &copilotPlugin{sessions: map[string]*sessionState{
		"s1": {session: fake, pending: map[string]chan permDecision{}},
	}}

	start := time.Now()
	_, err := p.CloseSession(context.Background(), &pb.CloseSessionRequest{SessionId: "s1"})
	if err != nil {
		t.Fatalf("CloseSession returned error: %v", err)
	}

	if time.Since(start) > 250*time.Millisecond {
		t.Fatalf("CloseSession exceeded expected timeout bound: %v", time.Since(start))
	}

	if !fake.destroyed {
		t.Fatal("expected Destroy to be called after disconnect timeout")
	}
	close(release)
}
