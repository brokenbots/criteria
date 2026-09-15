package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/brokenbots/criteria-go-adapter-sdk/adapterhost"
)

// fakeEventSender collects Execute events for assertions.
type fakeEventSender struct {
	events []*v2.ExecuteEvent
}

func (f *fakeEventSender) Send(ev *v2.ExecuteEvent) error {
	f.events = append(f.events, ev)
	return nil
}

var _ adapterhost.ExecuteEventSender = (*fakeEventSender)(nil)

// permittingEventSender wraps fakeEventSender and auto-approves permission.request
// events by resolving the bridge pending channel immediately. This is required for
// unit tests that call Execute without a real Permissions stream goroutine.
type permittingEventSender struct {
	inner  fakeEventSender
	bridge *MCPBridge
}

func (s *permittingEventSender) Send(ev *v2.ExecuteEvent) error {
	_ = s.inner.Send(ev)
	if a := ev.GetAdapter(); a != nil && a.GetEventKind() == "permission.request" {
		if p := a.GetPayload(); p != nil {
			if v, ok := p.GetFields()["request_id"]; ok {
				if reqID := v.GetStringValue(); reqID != "" {
					s.bridge.sendPermDecision(reqID, "allow")
				}
			}
		}
	}
	return nil
}

// TestParseCSVList covers all parseCSVList branches.
func TestParseCSVList(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    []string
		wantErr bool
	}{
		{"empty", "", nil, false},
		{"whitespace only", "   ", nil, false},
		{"single value", "foo", []string{"foo"}, false},
		{"csv values", "foo, bar, baz", []string{"foo", "bar", "baz"}, false},
		{"trims inner whitespace", " a , b ", []string{"a", "b"}, false},
		{"quoted value", `"hello, world"`, []string{"hello, world"}, false},
		{"skips blank entries", "a, , b", []string{"a", "b"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseCSVList(tc.input)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("len=%d want %d (got=%v want=%v)", len(got), len(tc.want), got, tc.want)
			}
			for i, v := range tc.want {
				if got[i] != v {
					t.Fatalf("[%d] got %q want %q", i, got[i], v)
				}
			}
		})
	}
}

// TestParseEnvPairs covers all parseEnvPairs branches.
func TestParseEnvPairs(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    []string
		wantErr bool
	}{
		{"empty", "", nil, false},
		{"single pair", "FOO=bar", []string{"FOO=bar"}, false},
		{"multiple pairs", "A=1, B=2", []string{"A=1", "B=2"}, false},
		{"value with equals", "URL=http://x=y", []string{"URL=http://x=y"}, false},
		{"missing equals errors", "NOEQUALS", nil, true},
		{"empty key errors", "=value", nil, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseEnvPairs(tc.input)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("len=%d want %d (got=%v want=%v)", len(got), len(tc.want), got, tc.want)
			}
			for i, v := range tc.want {
				if got[i] != v {
					t.Fatalf("[%d] got %q want %q", i, got[i], v)
				}
			}
		})
	}
}

// TestMCPBridge_Info validates the Info response schema shape.
func TestMCPBridge_Info(t *testing.T) {
	b := &MCPBridge{sessions: map[string]*sessionState{}}
	resp, err := b.Info(context.Background(), &v2.InfoRequest{})
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if resp.GetName() != adapterName {
		t.Fatalf("name=%q want %q", resp.GetName(), adapterName)
	}
	if resp.GetVersion() != adapterVersion {
		t.Fatalf("version=%q want %q", resp.GetVersion(), adapterVersion)
	}
	if resp.GetSourceUrl() == "" {
		t.Fatal("source_url is empty")
	}
	if len(resp.GetPlatforms()) == 0 {
		t.Fatal("platforms is empty")
	}

	// ConfigSchema must have "command" as required field.
	cfg := resp.GetConfigSchema()
	if cfg == nil {
		t.Fatal("config_schema is nil")
	}
	commandField, ok := cfg.GetFields()["command"]
	if !ok {
		t.Fatal("config_schema missing 'command' field")
	}
	if !commandField.GetRequired() {
		t.Fatal("config_schema.command must be required")
	}

	// CRI-172: the input surface is dynamic — no static InputSchema is
	// declared, because every non-reserved input key is an MCP tool argument
	// that varies with the discovered tool surface. Execute enforces the
	// "tool" routing key at call time.
	if in := resp.GetInputSchema(); in != nil && len(in.GetFields()) != 0 {
		t.Fatalf("input_schema = %v, want absent: the mcp input surface is dynamic", in.GetFields())
	}

	// CRI-172: the adapter declares the adapter_tools capability and the
	// CRI-171 tools list is empty until a session discovers a surface.
	found := false
	for _, c := range resp.GetCapabilities() {
		if c == "adapter_tools" {
			found = true
		}
	}
	if !found {
		t.Fatalf("capabilities %v must include adapter_tools", resp.GetCapabilities())
	}
	if len(resp.GetTools()) != 0 {
		t.Fatalf("tools = %v, want empty before any OpenSession discovery", resp.GetTools())
	}
}

// TestMCPBridge_OpenSession_MissingCommand validates that OpenSession rejects
// a request with no command configured.
func TestMCPBridge_OpenSession_MissingCommand(t *testing.T) {
	b := &MCPBridge{sessions: map[string]*sessionState{}}
	_, err := b.OpenSession(context.Background(), &v2.OpenSessionRequest{
		SessionId: "sess-1",
		Config:    map[string]string{},
	})
	if err == nil {
		t.Fatal("expected error when command is empty")
	}
}

// TestMCPBridge_OpenSession_BadCommand validates that OpenSession rejects a
// command binary that does not exist.
func TestMCPBridge_OpenSession_BadCommand(t *testing.T) {
	b := &MCPBridge{sessions: map[string]*sessionState{}}
	_, err := b.OpenSession(context.Background(), &v2.OpenSessionRequest{
		SessionId: "sess-bad",
		Config: map[string]string{
			"command": "/no/such/binary-does-not-exist",
		},
	})
	if err == nil {
		t.Fatal("expected error for non-existent command")
	}
}

// TestMCPBridge_Execute_UnknownSession verifies Execute returns an error for
// an unknown session ID without panicking.
func TestMCPBridge_Execute_UnknownSession(t *testing.T) {
	b := &MCPBridge{sessions: map[string]*sessionState{}}
	err := b.Execute(context.Background(), &v2.ExecuteRequest{SessionId: "ghost"}, nil)
	if err == nil {
		t.Fatal("expected error for unknown session")
	}
}

// TestMCPBridge_CloseSession_UnknownSession verifies CloseSession is a no-op
// for unknown session IDs.
func TestMCPBridge_CloseSession_UnknownSession(t *testing.T) {
	b := &MCPBridge{sessions: map[string]*sessionState{}}
	_, err := b.CloseSession(context.Background(), &v2.CloseSessionRequest{SessionId: "ghost"})
	if err != nil {
		t.Fatalf("CloseSession unknown session: %v", err)
	}
}

// TestMCPBridge_OpenSession_BadEnvPairs validates that OpenSession rejects a
// malformed env pair config.
func TestMCPBridge_OpenSession_BadEnvPairs(t *testing.T) {
	b := &MCPBridge{sessions: map[string]*sessionState{}}
	_, err := b.OpenSession(context.Background(), &v2.OpenSessionRequest{
		SessionId: "sess-env",
		Config: map[string]string{
			"command": "/bin/echo",
			"env":     "NOEQUALS",
		},
	})
	if err == nil {
		t.Fatal("expected error for malformed env pairs")
	}
}

// TestMCPBridge_FullRoundTrip exercises OpenSession → Execute → CloseSession
// using the echo-mcp fixture binary built by TestMain.
func TestMCPBridge_FullRoundTrip(t *testing.T) {
	if testEchoBin == "" {
		t.Skip("echo-mcp binary not available (TestMain not run)")
	}
	b := &MCPBridge{sessions: map[string]*sessionState{}}
	ctx := context.Background()

	// OpenSession.
	_, err := b.OpenSession(ctx, &v2.OpenSessionRequest{
		SessionId: "sess-rt",
		Config:    map[string]string{"command": testEchoBin},
	})
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}

	// Execute the echo tool.
	sender := &permittingEventSender{bridge: b}
	err = b.Execute(ctx, &v2.ExecuteRequest{
		SessionId: "sess-rt",
		Input: map[string]string{
			"tool":            "echo",
			"success_outcome": "success",
			"message":         "hello",
		},
	}, sender)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Verify a result event was sent and it is the last event (ordering contract).
	// The echo-mcp server emits Log events first, then a Result last.
	if len(sender.inner.events) == 0 {
		t.Fatal("expected at least one event, got none")
	}
	last := sender.inner.events[len(sender.inner.events)-1]
	if last.GetResult() == nil {
		t.Fatalf("last event must be a Result; got %T", last.GetEvent())
	}
	if last.GetResult().GetOutcome() != "success" {
		t.Fatalf("outcome=%q want success", last.GetResult().GetOutcome())
	}

	// CloseSession.
	if _, err := b.CloseSession(ctx, &v2.CloseSessionRequest{SessionId: "sess-rt"}); err != nil {
		t.Fatalf("CloseSession: %v", err)
	}
}

// TestMCPBridge_Execute_UnknownTool verifies Execute reports a typed
// unknown_tool failure (CRI-172): a failure result carrying the reserved
// call_error output, not a bare Execute error — the host's adapter-tools seam
// delivers it as the well-known call_error, distinguishable from a policy
// deny and from a callee crash.
func TestMCPBridge_Execute_UnknownTool(t *testing.T) {
	if testEchoBin == "" {
		t.Skip("echo-mcp binary not available")
	}
	b := &MCPBridge{sessions: map[string]*sessionState{}}
	ctx := context.Background()

	if _, err := b.OpenSession(ctx, &v2.OpenSessionRequest{
		SessionId: "sess-unk",
		Config:    map[string]string{"command": testEchoBin},
	}); err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	defer func() { _, _ = b.CloseSession(ctx, &v2.CloseSessionRequest{SessionId: "sess-unk"}) }()

	sender := &permittingEventSender{bridge: b}
	if err := b.Execute(ctx, &v2.ExecuteRequest{
		SessionId: "sess-unk",
		Input:     map[string]string{"tool": "no-such-tool"},
	}, sender); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	assertTypedUnknownToolResult(t, sender.inner.events)
}

// assertTypedUnknownToolResult asserts the last Execute event is a failure
// result whose outputs carry the reserved call_error=unknown_tool shape, and
// that no MCP tool content was produced.
func assertTypedUnknownToolResult(t *testing.T, events []*v2.ExecuteEvent) {
	t.Helper()
	if len(events) == 0 {
		t.Fatal("expected events, got none")
	}
	last := events[len(events)-1]
	res := last.GetResult()
	if res == nil {
		t.Fatalf("last event must be a Result; got %T", last.GetEvent())
	}
	if res.GetOutcome() != "failure" {
		t.Fatalf("outcome=%q want failure", res.GetOutcome())
	}
	var outputs map[string]any
	if err := json.Unmarshal(res.GetOutputsJson(), &outputs); err != nil {
		t.Fatalf("decode outputs_json: %v", err)
	}
	if outputs["call_error"] != callErrorUnknownTool {
		t.Fatalf("outputs = %v, want call_error %q", outputs, callErrorUnknownTool)
	}
	for _, ev := range events {
		if a := ev.GetAdapter(); a != nil && a.GetEventKind() == "mcp.content" {
			t.Fatal("unknown tool must not produce mcp.content events")
		}
	}
}

// TestMCPBridge_Info_ToolsAfterDiscovery verifies the CRI-171 tools list
// reflects the tools/list surface discovered at OpenSession, with names,
// descriptions, and input schemas (CRI-172).
func TestMCPBridge_Info_ToolsAfterDiscovery(t *testing.T) {
	if testEchoBin == "" {
		t.Skip("echo-mcp binary not available")
	}
	b := &MCPBridge{sessions: map[string]*sessionState{}}
	ctx := context.Background()

	if _, err := b.OpenSession(ctx, &v2.OpenSessionRequest{
		SessionId: "sess-info",
		Config:    map[string]string{"command": testEchoBin},
	}); err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	defer func() { _, _ = b.CloseSession(ctx, &v2.CloseSessionRequest{SessionId: "sess-info"}) }()

	resp, err := b.Info(ctx, &v2.InfoRequest{})
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	tools := map[string]*v2.ToolInfo{}
	for _, tool := range resp.GetTools() {
		tools[tool.GetName()] = tool
	}
	echo, ok := tools["echo"]
	if !ok {
		t.Fatalf("tools %v must contain the discovered \"echo\" tool", resp.GetTools())
	}
	if echo.GetDescription() == "" {
		t.Fatal("echo tool description must be populated from tools/list")
	}
	if echo.GetArgsSchemaJson() == "" {
		t.Fatal("echo tool args_schema_json must carry the input schema")
	}
	structured, ok := tools["structured"]
	if !ok {
		t.Fatalf("tools %v must contain the discovered \"structured\" tool", resp.GetTools())
	}
	if structured.GetDescription() == "" {
		t.Fatal("structured tool description must be populated from tools/list")
	}
}

// TestMCPBridge_Execute_ResultOutputs verifies successful tool calls map MCP
// content into outputs_json (CRI-172): text content as the primary "text"
// payload, structuredContent passed through verbatim under "structured".
func TestMCPBridge_Execute_ResultOutputs(t *testing.T) {
	if testEchoBin == "" {
		t.Skip("echo-mcp binary not available")
	}
	b := &MCPBridge{sessions: map[string]*sessionState{}}
	ctx := context.Background()

	if _, err := b.OpenSession(ctx, &v2.OpenSessionRequest{
		SessionId: "sess-out",
		Config:    map[string]string{"command": testEchoBin},
	}); err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	defer func() { _, _ = b.CloseSession(ctx, &v2.CloseSessionRequest{SessionId: "sess-out"}) }()

	execute := func(tool string, input map[string]string) *v2.ExecuteResult {
		t.Helper()
		full := map[string]string{"tool": tool, "success_outcome": "success"}
		for k, v := range input {
			full[k] = v
		}
		sender := &permittingEventSender{bridge: b}
		if err := b.Execute(ctx, &v2.ExecuteRequest{SessionId: "sess-out", Input: full}, sender); err != nil {
			t.Fatalf("Execute %s: %v", tool, err)
		}
		last := sender.inner.events[len(sender.inner.events)-1]
		res := last.GetResult()
		if res == nil {
			t.Fatalf("last event must be a Result; got %T", last.GetEvent())
		}
		return res
	}

	echoRes := execute("echo", map[string]string{"message": "hi"})
	if echoRes.GetOutcome() != "success" {
		t.Fatalf("echo outcome=%q want success", echoRes.GetOutcome())
	}
	var echoOut map[string]any
	if err := json.Unmarshal(echoRes.GetOutputsJson(), &echoOut); err != nil {
		t.Fatalf("decode echo outputs: %v", err)
	}
	if echoOut["text"] != `{"message":"hello"}` && !strings.Contains(fmt.Sprint(echoOut["text"]), "message") {
		t.Fatalf("echo outputs text = %v, want the echoed args payload", echoOut["text"])
	}
	if _, hasStructured := echoOut["structured"]; hasStructured {
		t.Fatalf("echo outputs = %v, must not carry structured content", echoOut)
	}

	structRes := execute("structured", nil)
	if structRes.GetOutcome() != "success" {
		t.Fatalf("structured outcome=%q want success", structRes.GetOutcome())
	}
	var structOut map[string]any
	if err := json.Unmarshal(structRes.GetOutputsJson(), &structOut); err != nil {
		t.Fatalf("decode structured outputs: %v", err)
	}
	if structOut["text"] != "structured payload" {
		t.Fatalf("structured outputs text = %v, want the text content payload", structOut["text"])
	}
	structured, ok := structOut["structured"].(map[string]any)
	if !ok {
		t.Fatalf("structured outputs = %v, want structuredContent passthrough", structOut)
	}
	if structured["count"] != float64(2) {
		t.Fatalf("structured passthrough = %v, want the server's structuredContent", structured)
	}
}

// TestMCPBridge_Execute_MissingTool verifies Execute returns an error when the
// "tool" key is missing from the config.
func TestMCPBridge_Execute_MissingTool(t *testing.T) {
	if testEchoBin == "" {
		t.Skip("echo-mcp binary not available")
	}
	b := &MCPBridge{sessions: map[string]*sessionState{}}
	ctx := context.Background()

	if _, err := b.OpenSession(ctx, &v2.OpenSessionRequest{
		SessionId: "sess-notool",
		Config:    map[string]string{"command": testEchoBin},
	}); err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	defer func() { _, _ = b.CloseSession(ctx, &v2.CloseSessionRequest{SessionId: "sess-notool"}) }()

	err := b.Execute(ctx, &v2.ExecuteRequest{
		SessionId: "sess-notool",
		Input:     map[string]string{}, // missing "tool"
	}, &fakeEventSender{})
	if err == nil {
		t.Fatal("expected error for missing tool")
	}
}

// denyingEventSender wraps fakeEventSender and immediately denies permission
// requests. Used to test that Execute stops and never calls the MCP tool
// when the host denies the permission.
type denyingEventSender struct {
	inner  fakeEventSender
	bridge *MCPBridge
}

func (s *denyingEventSender) Send(ev *v2.ExecuteEvent) error {
	_ = s.inner.Send(ev)
	if a := ev.GetAdapter(); a != nil && a.GetEventKind() == "permission.request" {
		if p := a.GetPayload(); p != nil {
			if v, ok := p.GetFields()["request_id"]; ok {
				if reqID := v.GetStringValue(); reqID != "" {
					s.bridge.sendPermDecision(reqID, "deny")
				}
			}
		}
	}
	return nil
}

// drainingEventSender wraps fakeEventSender and simulates a Permissions stream
// teardown (context cancel) by draining all pending permissions as denied. Used
// to test that Execute stops when the Permissions bidi stream closes.
type drainingEventSender struct {
	inner  fakeEventSender
	bridge *MCPBridge
}

func (s *drainingEventSender) Send(ev *v2.ExecuteEvent) error {
	_ = s.inner.Send(ev)
	if a := ev.GetAdapter(); a != nil && a.GetEventKind() == "permission.request" {
		s.bridge.drainPendingPerms()
	}
	return nil
}

// hasMCPContentEvent returns true if any of the collected events carries an
// mcp.content payload, which would indicate CallTool was reached.
func hasMCPContentEvent(events []*v2.ExecuteEvent) bool {
	for _, ev := range events {
		if a := ev.GetAdapter(); a != nil && a.GetEventKind() == "mcp.content" {
			return true
		}
	}
	return false
}

// TestMCPBridge_Execute_PermissionDenied asserts that when the host denies a
// permission.request event the Execute call returns a failure result and
// CallTool is never reached (no mcp.content event is emitted).
func TestMCPBridge_Execute_PermissionDenied(t *testing.T) {
	if testEchoBin == "" {
		t.Skip("echo-mcp binary not available")
	}
	b := &MCPBridge{sessions: map[string]*sessionState{}}
	ctx := context.Background()

	if _, err := b.OpenSession(ctx, &v2.OpenSessionRequest{
		SessionId: "sess-deny",
		Config: map[string]string{
			"command":     testEchoBin,
			"allow_tools": "", // block all tools — requires permission for every call
		},
	}); err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	defer func() { _, _ = b.CloseSession(ctx, &v2.CloseSessionRequest{SessionId: "sess-deny"}) }()

	sender := &denyingEventSender{bridge: b}
	err := b.Execute(ctx, &v2.ExecuteRequest{
		SessionId: "sess-deny",
		Input: map[string]string{
			"tool":            "echo",
			"success_outcome": "success",
			"message":         "hi",
		},
	}, sender)
	// Execute must complete without a Go error (the failure is communicated
	// via the Result event, not the error return).
	if err != nil {
		t.Fatalf("Execute: unexpected error: %v", err)
	}

	if hasMCPContentEvent(sender.inner.events) {
		t.Error("mcp.content event emitted after permission denied — CallTool must not run")
	}

	// Expect a failure result event.
	if len(sender.inner.events) == 0 {
		t.Fatal("no events emitted")
	}
	last := sender.inner.events[len(sender.inner.events)-1]
	if last.GetResult() == nil {
		t.Fatalf("last event must be a Result; got %T", last.GetEvent())
	}
	if last.GetResult().GetOutcome() == "success" {
		t.Error("result outcome should not be success after permission denied")
	}
}

// TestMCPBridge_Execute_PermissionsStreamTeardown asserts that when the
// Permissions stream closes (drainPendingPerms called on all pending requests)
// the Execute call returns a failure result and CallTool is never reached.
func TestMCPBridge_Execute_PermissionsStreamTeardown(t *testing.T) {
	if testEchoBin == "" {
		t.Skip("echo-mcp binary not available")
	}
	b := &MCPBridge{sessions: map[string]*sessionState{}}
	ctx := context.Background()

	if _, err := b.OpenSession(ctx, &v2.OpenSessionRequest{
		SessionId: "sess-drain",
		Config: map[string]string{
			"command":     testEchoBin,
			"allow_tools": "", // block all tools
		},
	}); err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	defer func() { _, _ = b.CloseSession(ctx, &v2.CloseSessionRequest{SessionId: "sess-drain"}) }()

	sender := &drainingEventSender{bridge: b}
	err := b.Execute(ctx, &v2.ExecuteRequest{
		SessionId: "sess-drain",
		Input: map[string]string{
			"tool":            "echo",
			"success_outcome": "success",
			"message":         "hi",
		},
	}, sender)
	if err != nil {
		t.Fatalf("Execute: unexpected error: %v", err)
	}

	if hasMCPContentEvent(sender.inner.events) {
		t.Error("mcp.content event emitted after stream teardown — CallTool must not run")
	}

	if len(sender.inner.events) == 0 {
		t.Fatal("no events emitted")
	}
	last := sender.inner.events[len(sender.inner.events)-1]
	if last.GetResult() == nil {
		t.Fatalf("last event must be a Result; got %T", last.GetEvent())
	}
	if last.GetResult().GetOutcome() == "success" {
		t.Error("result outcome should not be success after stream teardown")
	}
}
