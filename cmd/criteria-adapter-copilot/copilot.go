// Package main implements the criteria-adapter-copilot out-of-process adapter.
//
// The adapter preserves the Criteria adapter boundary while using the Copilot SDK
// internally for a structured session protocol (instead of parsing free-form CLI
// stdout). The SDK manages CLI daemon startup/transport and exposes typed events.
//
// One SDK session is created per OpenSession and can be reused for multiple
// Execute calls (multi-turn). Permission requests are bridged to the host via
// adapter Permit RPC: Execute blocks until Permit resolves each request.
//
// max_turns semantics:
//   - max_turns is enforced adapter-side per Execute call by counting assistant
//     message events for that turn.
//   - if the cap is reached, the adapter emits Adapter("limit.reached", ...)
//     and returns outcome "failure" (or "needs_review" if that outcome is in
//     the step's allowed set).
//
// Outcome semantics:
//   - the adapter registers a `submit_outcome` tool at OpenSession.
//   - per Execute, the host's allowed outcomes are loaded onto sessionState
//     before the prompt is sent.
//   - the model MUST call submit_outcome exactly once with a valid outcome;
//     the adapter forwards that value via ExecuteResult.
//   - on missing / invalid finalize, the adapter reprompts up to 2 additional
//     times. After 3 failed attempts the adapter returns "failure" with a
//     structured diagnostic event.
//   - permission denial returns "failure".
//
// File layout:
//   - copilot.go         — constants, types (copilotAdapter), Info/ensureClient/getSession
//   - copilot_session.go — session lifecycle: copilotSession interface, sdkSession, sessionState, Open/CloseSession
//   - copilot_turn.go    — Execute, turnState, event handlers
//   - copilot_outcome.go — submit_outcome tool: SubmitOutcomeArgs, handleSubmitOutcome, helpers
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

	v2 "github.com/brokenbots/criteria/proto/criteria/v2"
	adapterhost "github.com/brokenbots/criteria/sdk/adapterhost"
)

const (
	adapterName    = "copilot"
	adapterVersion = "0.1.0"

	defaultBinEnv = "CRITERIA_COPILOT_BIN"
	defaultBin    = "copilot"

	includeSensitivePermissionDetailsEnv = "CRITERIA_COPILOT_INCLUDE_SENSITIVE_PERMISSION_DETAILS"

	submitOutcomeToolName = "submit_outcome"

	// submitOutcomeToolDescription is the description surfaced to the model for
	// the submit_outcome tool. It conveys the contract: call exactly once with
	// a valid outcome before ending the turn, or the step fails.
	submitOutcomeToolDescription = "Finalize the outcome for the current step. Call this exactly once with one of the allowed outcomes for the step. The list of allowed outcomes is provided in the user prompt. Failure to call this tool with a valid outcome will fail the step."
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

type copilotAdapter struct {
	adapterhost.UnimplementedPermissions
	mu       sync.Mutex
	sessions map[string]*sessionState

	clientMu sync.Mutex
	client   *copilot.Client
}

func (p *copilotAdapter) Info(_ context.Context, _ *v2.InfoRequest) (*v2.InfoResponse, error) {
	return &v2.InfoResponse{
		Name:    adapterName,
		Version: adapterVersion,
		Capabilities: []string{
			"multi_turn",
			"permission_gating",
			"structured_events",
		},
		ConfigSchema: &v2.AdapterSchemaProto{Fields: map[string]*v2.ConfigFieldProto{
			"model":             {Type: "string", Description: "Copilot model to use for this session."},
			"reasoning_effort":  {Type: "string", Description: "Reasoning effort level for the model: low, medium, high, xhigh."},
			"working_directory": {Type: "string", Description: "Working directory for tool invocations."},
			"max_turns":         {Type: "number", Description: "Maximum assistant turns per Execute call (default: unlimited)."},
			"system_prompt":     {Type: "string", Description: "System prompt prepended at session open."},
			// Custom provider (BYOK) — point the session at an OpenAI-compatible
			// endpoint (Ollama, vLLM, Azure OpenAI, etc.). When provider_base_url
			// is set, the session uses this provider instead of GitHub Copilot's
			// default backend; in that case `model` is required.
			"provider_type":              {Type: "string", Description: "Custom provider type: openai, azure, or anthropic. Default: openai. Only used when provider_base_url is set."},
			"provider_base_url":          {Type: "string", Description: "Custom provider API endpoint URL. Setting this enables BYOK mode (e.g. http://localhost:11434/v1 for Ollama, vLLM endpoint). Requires `model` to be set."},
			"provider_api_key":           {Type: "string", Description: "Custom provider API key. Optional for local providers like Ollama. Prefer env() in HCL to keep secrets out of source."},
			"provider_bearer_token":      {Type: "string", Description: "Custom provider bearer token. Sets Authorization header directly; takes precedence over provider_api_key."},
			"provider_wire_api":          {Type: "string", Description: "Custom provider wire format (openai/azure only): completions or responses. Default: completions."},
			"provider_azure_api_version": {Type: "string", Description: "Azure API version, used when provider_type=azure. Default: 2024-10-21."},
		}},
		InputSchema: &v2.AdapterSchemaProto{Fields: map[string]*v2.ConfigFieldProto{
			"prompt":           {Required: true, Type: "string", Description: "User prompt to send to the assistant."},
			"max_turns":        {Type: "number", Description: "Per-step override for max assistant turns."},
			"reasoning_effort": {Type: "string", Description: "Per-step override for reasoning effort. Resets to the session default after this step. Valid: low, medium, high, xhigh."},
		}},
	}, nil
}

func (p *copilotAdapter) ensureClient(ctx context.Context) (*copilot.Client, error) {
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
		Connection: copilot.StdioConnection{Path: cliPath},
		LogLevel:   "info",
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

// Log blocks until ctx is cancelled (when the host closes the Log stream after
// Execute returns). WS15 wires real log line forwarding; WS03 drops log lines.
func (p *copilotAdapter) Log(ctx context.Context, _ *v2.LogRequest, _ adapterhost.LogEventSender) error {
	<-ctx.Done()
	return nil
}

func (p *copilotAdapter) getSession(sessionID string) *sessionState {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sessions[sessionID]
}
