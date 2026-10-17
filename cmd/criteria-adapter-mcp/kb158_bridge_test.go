package main

// KB-158 end-to-end bridge tests: the MCP adapter speaks the 2026-07-28
// stateless core through the probe-first stdio procedure of record, keeping
// the legacy 2025-03-26 handshake path intact behind explicit negotiation
// configs. The modern fixture dialect (testfixtures/echo-mcp, MCP_PROTOCOL)
// validates the wire shapes end to end.

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/brokenbots/criteria/cmd/criteria-adapter-mcp/mcpclient"
)

// kb158OpenConfig opens a session with cfg (command + env already resolved),
// asserts success, and wires teardown. Returns the bridge.
func kb158OpenConfig(t *testing.T, sessionID string, cfg map[string]string) *MCPBridge {
	t.Helper()
	bridge := &MCPBridge{sessions: map[string]*sessionState{}}
	if _, err := bridge.OpenSession(context.Background(), &v2.OpenSessionRequest{SessionId: sessionID, Config: cfg}); err != nil {
		t.Fatalf("OpenSession(%s): %v", sessionID, err)
	}
	t.Cleanup(func() {
		_, _ = bridge.CloseSession(context.Background(), &v2.CloseSessionRequest{SessionId: sessionID})
	})
	return bridge
}

// kb158ReadMethodlog drains the modern fixture's method log (MCP_METHODLOG).
func kb158ReadMethodlog(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read method log %s: %v", path, err)
	}
	var lines []string
	if s := strings.TrimSpace(string(raw)); s != "" {
		lines = strings.Split(s, "\n")
	}
	methods := make([]string, 0, len(lines))
	for _, line := range lines {
		methods = append(methods, strings.TrimPrefix(strings.TrimSpace(line), "method="))
	}
	return methods
}

// TestKB158_ProbeFirstAdoptsModernServer: with probe-first (the default) a
// modern fixture is adopted through server/discover — never the removed
// initialize handshake — and tools/call round-trips on the stateless session
// under the per-request _meta contract the fixture enforces strictly.
func TestKB158_ProbeFirstAdoptsModernServer(t *testing.T) {
	methodlog := t.TempDir() + "/methods"
	cfg := map[string]string{
		"command": testEchoBin,
		"env":     "MCP_PROTOCOL=2026-07-28,MCP_METHODLOG=" + methodlog,
	}
	b := kb158OpenConfig(t, "kb158-probe-first", cfg)

	state := b.sessions["kb158-probe-first"]
	if state.negotiation != mcpclient.NegotiationProbeFirst {
		t.Fatalf("negotiation = %q, want probe-first", state.negotiation)
	}
	methods := kb158ReadMethodlog(t, methodlog)
	if len(methods) == 0 || methods[0] != "server/discover" {
		t.Fatalf("first method = %v, want server/discover first", methods)
	}
	for _, m := range methods {
		if m == "initialize" {
			t.Fatalf("modern adoption must not send the removed initialize handshake; got %v", methods)
		}
	}
	if !strings.Contains(strings.Join(methods, ","), "tools/list") {
		t.Fatalf("modern adoption must discover the tool surface; got %v", methods)
	}

	// The fixture answers every request with a strict -32022 check on the
	// per-request _meta protocol version, so a successful echo proves the
	// stateless per-request meta contract end to end.
	sender := &permittingEventSender{bridge: b}
	if err := b.Execute(context.Background(), &v2.ExecuteRequest{
		SessionId: "kb158-probe-first",
		Input:     map[string]string{"tool": "echo", "message": "kb158", "success_outcome": "success"},
	}, sender); err != nil {
		t.Fatalf("Execute(echo): %v", err)
	}
	if text := kb155ContentTexts(t, sender.all()); !strings.Contains(text, "kb158") {
		t.Fatalf("echo text %q missing payload", text)
	}
}

// TestKB158_MrtrTypedFailure: an interim input_required result (SEP-2322)
// lands as the first-pass typed call_error failure with the input requests
// and request state surfaced as detail outputs — the step never wedges.
func TestKB158_MrtrTypedFailure(t *testing.T) {
	cfg := map[string]string{
		"command": testEchoBin,
		"env":     "MCP_PROTOCOL=2026-07-28,MCP_MRTR=1",
	}
	b := kb158OpenConfig(t, "kb158-mrtr", cfg)

	sender := &permittingEventSender{bridge: b}
	if err := b.Execute(context.Background(), &v2.ExecuteRequest{
		SessionId: "kb158-mrtr",
		Input:     map[string]string{"tool": "mrtr", "success_outcome": "success"},
	}, sender); err != nil {
		t.Fatalf("Execute(mrtr): %v", err)
	}
	events := sender.all()
	if len(events) < 2 {
		t.Fatalf("mrtr: events = %v, want the interim stream plus the typed failure", describeEvents(events))
	}
	// The failure result is the terminal event; everything before it is the
	// ordinary interim stream (permission gating, progress notification).
	last := events[len(events)-1]
	res := last.GetResult()
	if res == nil || res.GetOutcome() != "failure" {
		t.Fatalf("mrtr: terminal event must be a failure Result; got %v", describeEvents(events))
	}
	var outputs map[string]any
	if len(res.GetOutputsJson()) > 0 {
		if err := json.Unmarshal(res.GetOutputsJson(), &outputs); err != nil {
			t.Fatalf("mrtr: outputs_json: %v", err)
		}
	}
	if got, _ := outputs["call_error"].(string); got != "input_required" {
		t.Fatalf("mrtr: call_error = %q, want input_required (ADR-0004 §8 register)", got)
	}
	detail, _ := outputs["input_requests"].([]any)
	if len(detail) != 1 {
		t.Fatalf("mrtr: input_requests = %v, want one entry", outputs["input_requests"])
	}
	if entry, _ := detail[0].(string); entry != "req-1: sampling/createMessage" {
		t.Fatalf("mrtr: input_requests[0] = %q, want \"req-1: sampling/createMessage\"", entry)
	}
	if state, _ := outputs["request_state"].(string); state != "state-req-1" {
		t.Fatalf("mrtr: request_state = %q, want state-req-1", state)
	}
}

// TestKB158_HandshakeDiscoverAgainstLegacyFailsBounded: negotiation
// discover-stateless-core-only against a legacy 2025-03-26 fixture fails the
// open with the disabled-fallback signature, inside a short, bounded window.
func TestKB158_HandshakeDiscoverAgainstLegacyFailsBounded(t *testing.T) {
	deadline := time.Now().Add(3 * time.Second)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	bridge := &MCPBridge{sessions: map[string]*sessionState{}}
	_, err := bridge.OpenSession(ctx, &v2.OpenSessionRequest{
		SessionId: "kb158-discover-legacy",
		Config:    map[string]string{"command": testEchoBin, "handshake": "discover"},
	})
	if err == nil {
		t.Fatal("discover negotiation against a legacy server must fail")
	}
	if !strings.Contains(err.Error(), "legacy fallback is disabled") {
		t.Fatalf("discover failure must carry the disabled-fallback signature: %v", err)
	}
	if time.Now().After(deadline) {
		t.Fatalf("discover failure outlived its deadline: %v", err)
	}
}

// TestKB158_HandshakeInitializeAgainstModernRejected: the compat-window
// escape hatch explicitly speaks the legacy handshake; a 2026-07-28 server
// rejects the removed method statelessly (-32022 strict per-request meta on
// the handshake request, which carries no _meta). The open fails fast with
// the reserved error.
func TestKB158_HandshakeInitializeAgainstModernRejected(t *testing.T) {
	methodlog := t.TempDir() + "/methods"
	deadline := time.Now().Add(3 * time.Second)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	bridge := &MCPBridge{sessions: map[string]*sessionState{}}
	cfg := map[string]string{
		"command":   testEchoBin,
		"env":       "MCP_PROTOCOL=2026-07-28,MCP_METHODLOG=" + methodlog,
		"handshake": "initialize",
	}
	_, err := bridge.OpenSession(ctx, &v2.OpenSessionRequest{SessionId: "kb158-init-modern", Config: cfg})
	if err == nil {
		t.Fatal("initialize negotiation against a 2026-07-28 server must fail")
	}
	if !strings.Contains(err.Error(), "unsupported protocol version") {
		t.Fatalf("initialize failure must surface the reserved -32022: %v", err)
	}
	methods := kb158ReadMethodlog(t, methodlog)
	if len(methods) == 0 || methods[0] != "initialize" {
		t.Fatalf("initialize negotiation must start with the legacy handshake; got %v", methods)
	}
	if time.Now().After(deadline) {
		t.Fatalf("initialize failure outlived its deadline: %v", err)
	}
}

// TestKB158_HandshakeDiscoverModeModernOK: discover-only negotiation against
// a modern fleet skips the probe question entirely and opens statelessly.
func TestKB158_HandshakeDiscoverModeModernOK(t *testing.T) {
	cfg := map[string]string{
		"command":   testEchoBin,
		"env":       "MCP_PROTOCOL=2026-07-28",
		"handshake": "discover",
	}
	b := kb158OpenConfig(t, "kb158-discover-ok", cfg)
	state := b.sessions["kb158-discover-ok"]
	if state.negotiation != mcpclient.NegotiationDiscover {
		t.Fatalf("negotiation = %q, want discover", state.negotiation)
	}
	sender := &permittingEventSender{bridge: b}
	if err := b.Execute(context.Background(), &v2.ExecuteRequest{
		SessionId: "kb158-discover-ok",
		Input:     map[string]string{"tool": "echo", "message": "stateless", "success_outcome": "success"},
	}, sender); err != nil {
		t.Fatalf("Execute(echo): %v", err)
	}
	if text := kb155ContentTexts(t, sender.all()); !strings.Contains(text, "stateless") {
		t.Fatalf("echo text %q missing payload", text)
	}
}

// TestKB158_ProbeFirstFallsBackToLegacyServer: the default negotiation must
// keep opening 2025-03-26-era servers through the initialize-handshake
// fallback — the compat-window contract behind the probe.
func TestKB158_ProbeFirstFallsBackToLegacyServer(t *testing.T) {
	b := kb158OpenConfig(t, "kb158-legacy-fallback", map[string]string{"command": testEchoBin})
	state := b.sessions["kb158-legacy-fallback"]
	if state.negotiation != mcpclient.NegotiationProbeFirst {
		t.Fatalf("negotiation = %q, want probe-first", state.negotiation)
	}
	sender := &permittingEventSender{bridge: b}
	if err := b.Execute(context.Background(), &v2.ExecuteRequest{
		SessionId: "kb158-legacy-fallback",
		Input:     map[string]string{"tool": "echo", "message": "legacy", "success_outcome": "success"},
	}, sender); err != nil {
		t.Fatalf("Execute(echo) via legacy fallback: %v", err)
	}
	if text := kb155ContentTexts(t, sender.all()); !strings.Contains(text, "legacy") {
		t.Fatalf("echo text %q missing payload", text)
	}
}

// TestKB158_DiscoveryTTLAdoption: a modern server's CacheableResult ttl hint
// adapts the session's tools/list cache window within the adapter guardrails
// — adopted verbatim when inside [floor, ceiling], clamped up to the floor
// when the hint degenerates — and without a hint the adapter-default
// discovery TTL stands.
func TestKB158_DiscoveryTTLAdoption(t *testing.T) {
	cfg := map[string]string{
		"command": testEchoBin,
		"env":     "MCP_PROTOCOL=2026-07-28,MCP_TTL_MS=25000",
	}
	b := kb158OpenConfig(t, "kb158-ttl-modern", cfg)
	if got := b.sessions["kb158-ttl-modern"].discoveryTTL; got != 25*time.Second {
		t.Fatalf("discoveryTTL with ttlMs=25000 = %v, want 25s", got)
	}

	floored := kb158OpenConfig(t, "kb158-ttl-floored", map[string]string{
		"command": testEchoBin,
		"env":     "MCP_PROTOCOL=2026-07-28,MCP_TTL_MS=50",
	})
	if got := floored.sessions["kb158-ttl-floored"].discoveryTTL; got != discoveryTTLFloor {
		t.Fatalf("discoveryTTL with sub-floor hint = %v, want the %v guardrail", got, discoveryTTLFloor)
	}

	plain := kb158OpenConfig(t, "kb158-ttl-default", map[string]string{"command": testEchoBin})
	if got := plain.sessions["kb158-ttl-default"].discoveryTTL; got != discoveryCacheTTL {
		t.Fatalf("discoveryTTL without hint = %v, want adapter default %v", got, discoveryCacheTTL)
	}
}
