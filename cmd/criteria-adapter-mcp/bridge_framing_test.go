package main

// bridge_framing_test.go — KB-161: the optional "framing" config key selects
// the JSON-RPC write framing for the stdio transport (lsp default, ndjson
// opt-in per the MCP stdio wire shape). Pinned here at the bridge seam: the
// Info surface advertises the key, unknown spellings fail the session open
// instead of silently downgrading the transport, and a valid ndjson config
// opens the fixture server in ndjson mode and discovers the full tool
// surface over it.

import (
	"context"
	"strings"
	"testing"
	"time"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
)

func TestMCPBridgeInfoSchemaAdvertisesFraming(t *testing.T) {
	bridge := &MCPBridge{sessions: map[string]*sessionState{}}
	resp, err := bridge.Info(context.Background(), &v2.InfoRequest{})
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	field, ok := resp.ConfigSchema.Fields["framing"]
	if !ok {
		t.Fatal("Info ConfigSchema missing framing field")
	}
	if field.Type != "string" {
		t.Errorf("framing field type = %q want string", field.Type)
	}
	if field.Required {
		t.Error("framing field must be optional (default lsp)")
	}
	if !strings.Contains(field.Description, "ndjson") || !strings.Contains(field.Description, "lsp") {
		t.Errorf("framing field description %q should name both modes", field.Description)
	}
}

func TestStartMCPServerRejectsUnknownFraming(t *testing.T) {
	_, err := startMCPServer(map[string]string{"command": testEchoBin, "framing": "websocket"})
	if err == nil {
		t.Fatal("startMCPServer with unknown framing succeeded, want error")
	}
	if !strings.Contains(err.Error(), "unknown framing") {
		t.Errorf("error = %v want unknown-framing parse error", err)
	}
}

// TestMCPBridgeNDJSONSession drives a bridge-configured ndjson session
// through the bridge RPC surface directly: OpenSession launches the fixture
// server in ndjson mode (MCP_FRAMING=ndjson), the handshake + discovery read
// the ndjson stream, and Info advertises the discovered tools. The lsp and
// ndjson modes are different environments (KB-157 fingerprints every config
// entry), and discovery works over both.
func TestMCPBridgeNDJSONSession(t *testing.T) {
	bridge := &MCPBridge{sessions: map[string]*sessionState{}}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := bridge.OpenSession(ctx, &v2.OpenSessionRequest{
		SessionId: "framing-ndjson",
		Config: map[string]string{
			"command": testEchoBin,
			"env":     "MCP_FRAMING=ndjson",
			"framing": "ndjson",
		},
	}); err != nil {
		t.Fatalf("OpenSession ndjson: %v", err)
	}
	defer func() {
		_, _ = bridge.CloseSession(context.Background(), &v2.CloseSessionRequest{SessionId: "framing-ndjson"})
	}()

	resp, err := bridge.Info(ctx, &v2.InfoRequest{})
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	discovered := map[string]bool{}
	for _, tool := range resp.GetTools() {
		discovered[tool.GetName()] = true
	}
	for _, want := range []string{"echo", "structured", "fault"} {
		if !discovered[want] {
			t.Errorf("discovered tools missing %q: %v", want, discovered)
		}
	}
}
