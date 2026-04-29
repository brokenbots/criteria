package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
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

	// setModelCalls records the (model, effort) pairs passed to SetModel in order.
	setModelCalls []setModelCall
}

type setModelCall struct {
	model  string
	effort string // empty string when opts == nil or opts.ReasoningEffort == nil
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

func (f *fakeSession) SetModel(_ context.Context, model string, opts *copilot.SetModelOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	call := setModelCall{model: model}
	if opts != nil && opts.ReasoningEffort != nil {
		call.effort = *opts.ReasoningEffort
	}
	f.setModelCalls = append(f.setModelCalls, call)
	return f.setModelErr
}

func (f *fakeSession) getSetModelCalls() []setModelCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]setModelCall, len(f.setModelCalls))
	copy(out, f.setModelCalls)
	return out
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

// ── W09 tests ────────────────────────────────────────────────────────────────

// Test 6.1: OpenSession with reasoning_effort="high" and no model succeeds and
// calls SetModel with the expected effort. Calls the production helper
// applyOpenSessionModel so any regression in it fails this test.
func TestOpenSessionReasoningEffortWithoutModel(t *testing.T) {
	fake := &fakeSession{}
	p := &copilotPlugin{sessions: map[string]*sessionState{}}

	s := &sessionState{
		session: fake,
		pending: make(map[string]chan permDecision),
	}

	cfg := map[string]string{"reasoning_effort": "high"}
	if err := p.applyOpenSessionModel(context.Background(), s, cfg); err != nil {
		t.Fatalf("applyOpenSessionModel returned error: %v", err)
	}

	calls := fake.getSetModelCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 SetModel call, got %d", len(calls))
	}
	if calls[0].effort != "high" {
		t.Errorf("SetModel called with effort=%q, want %q", calls[0].effort, "high")
	}
	if calls[0].model != "" {
		t.Errorf("SetModel called with model=%q, want empty string", calls[0].model)
	}
	if s.defaultEffort != "high" {
		t.Errorf("defaultEffort = %q, want %q", s.defaultEffort, "high")
	}
	if s.defaultModel != "" {
		t.Errorf("defaultModel = %q, want empty string", s.defaultModel)
	}
}

// Test 6.2: OpenSession with both reasoning_effort and model set (regression guard).
// Calls the production helper applyOpenSessionModel so any regression in it fails this test.
func TestOpenSessionReasoningEffortWithModel(t *testing.T) {
	fake := &fakeSession{}
	p := &copilotPlugin{sessions: map[string]*sessionState{}}

	s := &sessionState{
		session: fake,
		pending: make(map[string]chan permDecision),
	}

	cfg := map[string]string{"model": "claude-sonnet-4.6", "reasoning_effort": "medium"}
	if err := p.applyOpenSessionModel(context.Background(), s, cfg); err != nil {
		t.Fatalf("applyOpenSessionModel returned error: %v", err)
	}

	calls := fake.getSetModelCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 SetModel call, got %d", len(calls))
	}
	if calls[0].model != "claude-sonnet-4.6" {
		t.Errorf("SetModel called with model=%q, want %q", calls[0].model, "claude-sonnet-4.6")
	}
	if calls[0].effort != "medium" {
		t.Errorf("SetModel called with effort=%q, want %q", calls[0].effort, "medium")
	}
	if s.defaultModel != "claude-sonnet-4.6" {
		t.Errorf("defaultModel = %q, want %q", s.defaultModel, "claude-sonnet-4.6")
	}
	if s.defaultEffort != "medium" {
		t.Errorf("defaultEffort = %q, want %q", s.defaultEffort, "medium")
	}
}

// Test 6.3: OpenSession with reasoning_effort="invalid" fails with a clear error.
func TestOpenSessionInvalidReasoningEffort(t *testing.T) {
	err := validateReasoningEffort("invalid")
	if err == nil {
		t.Fatal("expected error for invalid reasoning_effort, got nil")
	}
	if !strings.Contains(err.Error(), "low, medium, high, xhigh") {
		t.Errorf("error message should list valid values; got: %v", err)
	}
}

// Test 6.4: Execute with per-step reasoning_effort="high" applies the override and
// restores the agent default ("medium") after the step. Assert the SDK call sequence.
func TestExecutePerStepReasoningEffortRestoresDefault(t *testing.T) {
	fake := &fakeSession{
		emitOnSend: []copilot.SessionEvent{
			{Type: copilot.SessionEventTypeAssistantMessage, Data: &copilot.AssistantMessageData{MessageID: "m1", Content: "RESULT: success"}},
			{Type: copilot.SessionEventTypeSessionIdle, Data: &copilot.SessionIdleData{}},
		},
	}

	// Session has agent-level default effort "medium".
	s := &sessionState{
		session:       fake,
		pending:       make(map[string]chan permDecision),
		defaultEffort: "medium",
		defaultModel:  "",
	}
	p := &copilotPlugin{sessions: map[string]*sessionState{"s1": s}}
	sender := &recordingSender{}

	err := p.Execute(context.Background(), &pb.ExecuteRequest{
		SessionId: "s1",
		Config:    map[string]string{"prompt": "hi", "reasoning_effort": "high"},
	}, sender)
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	// Expected call sequence:
	//   1. applyRequestEffort: SetModel("", effort="high")
	//   2. (applyRequestModel: no-op, no model in cfg)
	//   3. Send
	//   4. defer restoreEffort: SetModel("", effort="medium")
	calls := fake.getSetModelCalls()
	if len(calls) != 2 {
		t.Fatalf("expected 2 SetModel calls (apply + restore), got %d: %+v", len(calls), calls)
	}
	if calls[0].effort != "high" {
		t.Errorf("call[0].effort = %q, want %q (apply override)", calls[0].effort, "high")
	}
	if calls[1].effort != "medium" {
		t.Errorf("call[1].effort = %q, want %q (restore default)", calls[1].effort, "medium")
	}

	// Confirm the step still produced a result event.
	hasSuccess := false
	for _, ev := range sender.snapshot() {
		if r := ev.GetResult(); r != nil && r.GetOutcome() == "success" {
			hasSuccess = true
		}
	}
	if !hasSuccess {
		t.Fatal("expected success result event")
	}
}

// TestExecutePerStepEffortRestoresWhenNoDefault verifies B2 fix: when an agent
// session was opened without a reasoning_effort default, a per-step override
// must still be reversed at the end of the step by calling SetModel with a
// nil opts (clearing the effort), not by being silently skipped.
func TestExecutePerStepEffortRestoresWhenNoDefault(t *testing.T) {
	fake := &fakeSession{
		emitOnSend: []copilot.SessionEvent{
			{Type: copilot.SessionEventTypeAssistantMessage, Data: &copilot.AssistantMessageData{MessageID: "m1", Content: "RESULT: success"}},
			{Type: copilot.SessionEventTypeSessionIdle, Data: &copilot.SessionIdleData{}},
		},
	}

	// Session has NO agent-level default effort (opened without reasoning_effort in config).
	s := &sessionState{
		session:       fake,
		pending:       make(map[string]chan permDecision),
		defaultEffort: "",
		defaultModel:  "",
	}
	p := &copilotPlugin{sessions: map[string]*sessionState{"s1": s}}
	sender := &recordingSender{}

	if err := p.Execute(context.Background(), &pb.ExecuteRequest{
		SessionId: "s1",
		Config:    map[string]string{"prompt": "hi", "reasoning_effort": "high"},
	}, sender); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	// Expected SDK call sequence:
	//   1. applyRequestEffort:  SetModel("", effort="high")   — apply override
	//   2. defer restoreEffort: SetModel("", nil)             — restore: clear effort
	calls := fake.getSetModelCalls()
	if len(calls) != 2 {
		t.Fatalf("expected 2 SetModel calls (apply + restore), got %d: %+v", len(calls), calls)
	}
	if calls[0].effort != "high" {
		t.Errorf("calls[0].effort = %q, want %q (apply override)", calls[0].effort, "high")
	}
	// Restore call must have empty effort (nil opts → "" effort in the fake).
	if calls[1].effort != "" {
		t.Errorf("calls[1].effort = %q, want empty string (restore: clear effort)", calls[1].effort)
	}
}
