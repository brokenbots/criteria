// Package main implements the criteria-adapter-copilot out-of-process plugin.
//
// The plugin preserves the Criteria plugin boundary while using the Copilot SDK
// internally for a structured session protocol (instead of parsing free-form CLI
// stdout). The SDK manages CLI daemon startup/transport and exposes typed events.
//
// One SDK session is created per OpenSession and can be reused for multiple
// Execute calls (multi-turn). Permission requests are bridged to the host via
// plugin Permit RPC: Execute blocks until Permit resolves each request.
//
// max_turns semantics:
//   - max_turns is enforced plugin-side per Execute call by counting assistant
//     message events for that turn.
//   - if the cap is reached, the plugin emits Adapter("limit.reached", ...)
//     and returns outcome "needs_review".
//
// Outcome semantics:
//   - the plugin parses the final assistant message for RESULT: <outcome>.
//   - if absent or empty, outcome defaults to "needs_review".
//
// File layout:
//   - copilot.go         — constants, types (copilotPlugin), Info/ensureClient/getSession
//   - copilot_session.go — session lifecycle: copilotSession interface, sdkSession, sessionState, Open/CloseSession
//   - copilot_turn.go    — Execute, turnState, event handlers
//   - copilot_model.go   — model/effort helpers: applyRequestModel, applyRequestEffort, validateReasoningEffort
//   - copilot_permission.go — Permit, handlePermissionRequest, permissionDetails
//   - copilot_util.go    — resultEvent, logEvent, adapterEvent, stringifyAny
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	copilot "github.com/github/copilot-sdk/go"

	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

const (
	pluginName    = "copilot"
	pluginVersion = "0.1.0"

	defaultBinEnv = "CRITERIA_COPILOT_BIN"
	defaultBin    = "copilot"

	includeSensitivePermissionDetailsEnv = "CRITERIA_COPILOT_INCLUDE_SENSITIVE_PERMISSION_DETAILS"

	resultPrefix = "result:"
)

var errMaxTurnsReached = errors.New("copilot: max_turns reached")
var closeSessionGrace = 5 * time.Second

// validReasoningEfforts is the documented set of accepted reasoning effort values.
var validReasoningEfforts = map[string]bool{
	"low":    true,
	"medium": true,
	"high":   true,
	"xhigh":  true,
}

type permDecision struct {
	allow  bool
	reason string
}

type copilotPlugin struct {
	mu       sync.Mutex
	sessions map[string]*sessionState

	clientMu sync.Mutex
	client   *copilot.Client
}

func (p *copilotPlugin) Info(_ context.Context, _ *pb.InfoRequest) (*pb.InfoResponse, error) {
	return &pb.InfoResponse{
		Name:    pluginName,
		Version: pluginVersion,
		Capabilities: []string{
			"multi_turn",
			"permission_gating",
			"structured_events",
		},
		ConfigSchema: &pb.AdapterSchemaProto{Fields: map[string]*pb.ConfigFieldProto{
			"model":             {Type: "string", Doc: "Copilot model to use for this session."},
			"reasoning_effort":  {Type: "string", Doc: "Reasoning effort level for the model: low, medium, high, xhigh."},
			"working_directory": {Type: "string", Doc: "Working directory for tool invocations."},
			"max_turns":         {Type: "number", Doc: "Maximum assistant turns per Execute call (default: unlimited)."},
			"system_prompt":     {Type: "string", Doc: "System prompt prepended at session open."},
		}},
		InputSchema: &pb.AdapterSchemaProto{Fields: map[string]*pb.ConfigFieldProto{
			"prompt":           {Required: true, Type: "string", Doc: "User prompt to send to the assistant."},
			"max_turns":        {Type: "number", Doc: "Per-step override for max assistant turns."},
			"reasoning_effort": {Type: "string", Doc: "Per-step override for reasoning effort. Resets to the session default after this step. Valid: low, medium, high, xhigh."},
		}},
	}, nil
}

func (p *copilotPlugin) ensureClient(ctx context.Context) (*copilot.Client, error) {
	p.clientMu.Lock()
	defer p.clientMu.Unlock()
	if p.client != nil {
		return p.client, nil
	}

	cliPath := os.Getenv(defaultBinEnv)
	if strings.TrimSpace(cliPath) == "" {
		cliPath = defaultBin
	}

	token := resolveGitHubToken()
	options := &copilot.ClientOptions{
		CLIPath:   cliPath,
		LogLevel:  "info",
		AutoStart: copilot.Bool(true),
	}
	if token != "" {
		options.GitHubToken = token
		options.UseLoggedInUser = copilot.Bool(false)
	}

	client := copilot.NewClient(options)
	if err := client.Start(ctx); err != nil {
		return nil, fmt.Errorf("copilot: start client: %w", err)
	}
	p.client = client
	return p.client, nil
}

func resolveGitHubToken() string {
	if token := strings.TrimSpace(os.Getenv("COPILOT_GITHUB_TOKEN")); token != "" {
		return token
	}
	if token := strings.TrimSpace(os.Getenv("GH_TOKEN")); token != "" {
		return token
	}
	if token := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); token != "" {
		return token
	}
	return ""
}

func (p *copilotPlugin) getSession(sessionID string) *sessionState {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sessions[sessionID]
}
