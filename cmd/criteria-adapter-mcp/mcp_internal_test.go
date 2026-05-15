package main

import (
	"context"
	"testing"

	"github.com/brokenbots/criteria/sdk/adapterhost"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

// fakeEventSender collects Execute events for assertions.
type fakeEventSender struct {
	events []*pb.ExecuteEvent
}

func (f *fakeEventSender) Send(ev *pb.ExecuteEvent) error {
	f.events = append(f.events, ev)
	return nil
}

var _ adapterhost.ExecuteEventSender = (*fakeEventSender)(nil)

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
	resp, err := b.Info(context.Background(), &pb.InfoRequest{})
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if resp.GetName() != adapterName {
		t.Fatalf("name=%q want %q", resp.GetName(), adapterName)
	}
	if resp.GetVersion() != adapterVersion {
		t.Fatalf("version=%q want %q", resp.GetVersion(), adapterVersion)
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

	// InputSchema must have "tool" as required.
	in := resp.GetInputSchema()
	if in == nil {
		t.Fatal("input_schema is nil")
	}
	toolField, ok := in.GetFields()["tool"]
	if !ok {
		t.Fatal("input_schema missing 'tool' field")
	}
	if !toolField.GetRequired() {
		t.Fatal("input_schema.tool must be required")
	}
}

// TestMCPBridge_OpenSession_MissingCommand validates that OpenSession rejects
// a request with no command configured.
func TestMCPBridge_OpenSession_MissingCommand(t *testing.T) {
	b := &MCPBridge{sessions: map[string]*sessionState{}}
	_, err := b.OpenSession(context.Background(), &pb.OpenSessionRequest{
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
	_, err := b.OpenSession(context.Background(), &pb.OpenSessionRequest{
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
	err := b.Execute(context.Background(), &pb.ExecuteRequest{SessionId: "ghost"}, nil)
	if err == nil {
		t.Fatal("expected error for unknown session")
	}
}

// TestMCPBridge_CloseSession_UnknownSession verifies CloseSession is a no-op
// for unknown session IDs.
func TestMCPBridge_CloseSession_UnknownSession(t *testing.T) {
	b := &MCPBridge{sessions: map[string]*sessionState{}}
	_, err := b.CloseSession(context.Background(), &pb.CloseSessionRequest{SessionId: "ghost"})
	if err != nil {
		t.Fatalf("CloseSession unknown session: %v", err)
	}
}

// TestMCPBridge_OpenSession_BadEnvPairs validates that OpenSession rejects a
// malformed env pair config.
func TestMCPBridge_OpenSession_BadEnvPairs(t *testing.T) {
	b := &MCPBridge{sessions: map[string]*sessionState{}}
	_, err := b.OpenSession(context.Background(), &pb.OpenSessionRequest{
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
	_, err := b.OpenSession(ctx, &pb.OpenSessionRequest{
		SessionId: "sess-rt",
		Config:    map[string]string{"command": testEchoBin},
	})
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}

	// Execute the echo tool.
	sender := &fakeEventSender{}
	err = b.Execute(ctx, &pb.ExecuteRequest{
		SessionId: "sess-rt",
		Config: map[string]string{
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
	if len(sender.events) == 0 {
		t.Fatal("expected at least one event, got none")
	}
	last := sender.events[len(sender.events)-1]
	if last.GetResult() == nil {
		t.Fatalf("last event must be a Result; got %T", last.GetEvent())
	}
	if last.GetResult().GetOutcome() != "success" {
		t.Fatalf("outcome=%q want success", last.GetResult().GetOutcome())
	}

	// CloseSession.
	if _, err := b.CloseSession(ctx, &pb.CloseSessionRequest{SessionId: "sess-rt"}); err != nil {
		t.Fatalf("CloseSession: %v", err)
	}
}

// TestMCPBridge_Execute_UnknownTool verifies Execute returns an error when the
// requested tool was not advertised by the MCP server.
func TestMCPBridge_Execute_UnknownTool(t *testing.T) {
	if testEchoBin == "" {
		t.Skip("echo-mcp binary not available")
	}
	b := &MCPBridge{sessions: map[string]*sessionState{}}
	ctx := context.Background()

	if _, err := b.OpenSession(ctx, &pb.OpenSessionRequest{
		SessionId: "sess-unk",
		Config:    map[string]string{"command": testEchoBin},
	}); err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	defer func() { _, _ = b.CloseSession(ctx, &pb.CloseSessionRequest{SessionId: "sess-unk"}) }()

	err := b.Execute(ctx, &pb.ExecuteRequest{
		SessionId: "sess-unk",
		Config:    map[string]string{"tool": "no-such-tool"},
	}, &fakeEventSender{})
	if err == nil {
		t.Fatal("expected error for unknown tool")
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

	if _, err := b.OpenSession(ctx, &pb.OpenSessionRequest{
		SessionId: "sess-notool",
		Config:    map[string]string{"command": testEchoBin},
	}); err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	defer func() { _, _ = b.CloseSession(ctx, &pb.CloseSessionRequest{SessionId: "sess-notool"}) }()

	err := b.Execute(ctx, &pb.ExecuteRequest{
		SessionId: "sess-notool",
		Config:    map[string]string{}, // missing "tool"
	}, &fakeEventSender{})
	if err == nil {
		t.Fatal("expected error for missing tool")
	}
}
