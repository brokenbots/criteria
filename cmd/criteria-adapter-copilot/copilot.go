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
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	copilot "github.com/github/copilot-sdk/go"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/structpb"

	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
	pluginhost "github.com/brokenbots/criteria/sdk/pluginhost"
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

type copilotSession interface {
	On(handler copilot.SessionEventHandler) func()
	Send(ctx context.Context, options copilot.MessageOptions) (string, error)
	SetModel(ctx context.Context, model string, opts *copilot.SetModelOptions) error
	Disconnect() error
	Destroy() error
}

type sdkSession struct {
	inner *copilot.Session
}

func (s *sdkSession) On(handler copilot.SessionEventHandler) func() {
	return s.inner.On(handler)
}

func (s *sdkSession) Send(ctx context.Context, options copilot.MessageOptions) (string, error) {
	return s.inner.Send(ctx, options)
}

func (s *sdkSession) SetModel(ctx context.Context, model string, opts *copilot.SetModelOptions) error {
	return s.inner.SetModel(ctx, model, opts)
}

func (s *sdkSession) Disconnect() error {
	return s.inner.Disconnect()
}

func (s *sdkSession) Destroy() error {
	return s.inner.Destroy()
}

type permDecision struct {
	allow  bool
	reason string
}

type sessionState struct {
	session copilotSession

	execMu sync.Mutex

	mu             sync.Mutex
	pending        map[string]chan permDecision
	active         bool
	activeCh       chan struct{}
	sink           pluginhost.ExecuteEventSender
	permissionDeny bool

	// defaultModel and defaultEffort record the agent-level model and
	// reasoning_effort values set at OpenSession time. applyRequestEffort uses
	// these to restore the session's effort after a per-step override.
	// These values are constant for the lifetime of the session; any future
	// feature that dynamically updates the agent default mid-run must update
	// these fields accordingly.
	defaultModel  string
	defaultEffort string
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

func (p *copilotPlugin) OpenSession(ctx context.Context, req *pb.OpenSessionRequest) (*pb.OpenSessionResponse, error) {
	client, err := p.ensureClient(ctx)
	if err != nil {
		return nil, err
	}

	cfg := req.GetConfig()
	pluginSessionID := req.GetSessionId()
	sessionConfig := p.buildSessionConfig(cfg, pluginSessionID)

	session, err := client.CreateSession(ctx, sessionConfig)
	if err != nil {
		return nil, fmt.Errorf("copilot: create session: %w", err)
	}

	s := &sessionState{
		session: &sdkSession{inner: session},
		pending: make(map[string]chan permDecision),
	}

	p.mu.Lock()
	p.sessions[pluginSessionID] = s
	p.mu.Unlock()

	if err := p.applyOpenSessionModel(ctx, s, cfg); err != nil {
		return nil, err
	}

	return &pb.OpenSessionResponse{}, nil
}

// buildSessionConfig constructs the SDK SessionConfig from agent-level config fields.
func (p *copilotPlugin) buildSessionConfig(cfg map[string]string, pluginSessionID string) *copilot.SessionConfig {
	sc := &copilot.SessionConfig{
		Streaming: true,
		Model:     cfg["model"],
		OnPermissionRequest: func(r copilot.PermissionRequest, _ copilot.PermissionInvocation) (copilot.PermissionRequestResult, error) {
			return p.handlePermissionRequest(pluginSessionID, r)
		},
	}
	if wd := strings.TrimSpace(cfg["working_directory"]); wd != "" {
		sc.WorkingDirectory = wd
	}
	if sp := strings.TrimSpace(cfg["system_prompt"]); sp != "" {
		sc.SystemMessage = &copilot.SystemMessageConfig{Content: sp}
	}
	return sc
}

// applyOpenSessionModel validates and applies model/reasoning_effort at session open,
// then captures the agent-level defaults into s for per-step restore.
func (p *copilotPlugin) applyOpenSessionModel(ctx context.Context, s *sessionState, cfg map[string]string) error {
	model := strings.TrimSpace(cfg["model"])
	effort := strings.TrimSpace(cfg["reasoning_effort"])

	if effort != "" {
		if err := validateReasoningEffort(effort); err != nil {
			return err
		}
	}

	if model != "" || effort != "" {
		var opts *copilot.SetModelOptions
		if effort != "" {
			opts = &copilot.SetModelOptions{ReasoningEffort: &effort}
		}
		if err := s.session.SetModel(ctx, model, opts); err != nil {
			return fmt.Errorf("copilot: set model at open: %w", err)
		}
	}

	// Capture agent-level defaults so per-step overrides can restore them.
	s.defaultModel = model
	s.defaultEffort = effort
	return nil
}

func (p *copilotPlugin) Execute(ctx context.Context, req *pb.ExecuteRequest, sink pluginhost.ExecuteEventSender) error {
	s, prompt, maxTurns, err := p.prepareExecute(req)
	if err != nil {
		return err
	}

	s.execMu.Lock()
	defer s.execMu.Unlock()

	cleanup := s.beginExecution(sink)
	defer cleanup()

	state := newTurnState(maxTurns)
	unsubscribe := s.session.On(state.handleEvent(sink))
	defer unsubscribe()

	restoreEffort, err := applyRequestEffort(ctx, s, s.session, req.GetConfig())
	if err != nil {
		return err
	}
	defer restoreEffort()

	if err := applyRequestModel(ctx, s.session, req.GetConfig()); err != nil {
		return err
	}

	if _, err := s.session.Send(ctx, copilot.MessageOptions{Prompt: prompt}); err != nil {
		return fmt.Errorf("copilot: send prompt: %w", err)
	}

	return state.awaitOutcome(ctx, s, sink)
}

// prepareExecute validates the request and returns the session state, prompt,
// and max_turns limit. Returns an error when any required field is missing or
// the session is unknown.
func (p *copilotPlugin) prepareExecute(req *pb.ExecuteRequest) (s *sessionState, prompt string, maxTurns int, err error) {
	s = p.getSession(req.GetSessionId())
	if s == nil {
		return nil, "", 0, fmt.Errorf("copilot: unknown session %q", req.GetSessionId())
	}

	prompt = strings.TrimSpace(req.GetConfig()["prompt"])
	if prompt == "" {
		return nil, "", 0, fmt.Errorf("copilot: config.prompt is required")
	}

	if raw := strings.TrimSpace(req.GetConfig()["max_turns"]); raw != "" {
		n, parseErr := strconv.Atoi(raw)
		if parseErr != nil || n < 0 {
			return nil, "", 0, fmt.Errorf("copilot: invalid max_turns %q", raw)
		}
		maxTurns = n
	}
	return s, prompt, maxTurns, nil
}

// beginExecution marks the session active and wires up the event sink.
// The returned cleanup function must be deferred by the caller.
func (s *sessionState) beginExecution(sink pluginhost.ExecuteEventSender) func() {
	execDone := make(chan struct{})
	s.mu.Lock()
	s.active = true
	s.activeCh = execDone
	s.sink = sink
	s.permissionDeny = false
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		s.active = false
		s.sink = nil
		if s.activeCh != nil {
			close(s.activeCh)
			s.activeCh = nil
		}
		s.mu.Unlock()
	}
}

// turnState tracks per-Execute state: final content, turn count, and channels
// for coordinating the event handler goroutine with the wait loop.
type turnState struct {
	finalContent   string
	assistantTurns int
	turnDone       chan struct{}
	errCh          chan error
	maxTurns       int
}

func newTurnState(maxTurns int) *turnState {
	return &turnState{
		turnDone: make(chan struct{}, 1),
		errCh:    make(chan error, 1),
		maxTurns: maxTurns,
	}
}

// sendErr non-blockingly forwards a non-nil error to the error channel.
func (ts *turnState) sendErr(err error) {
	if err == nil {
		return
	}
	select {
	case ts.errCh <- err:
	default:
	}
}

// handleEvent returns a SessionEventHandler that dispatches SDK events to the
// appropriate per-event-type methods on ts.
func (ts *turnState) handleEvent(sink pluginhost.ExecuteEventSender) func(copilot.SessionEvent) {
	return func(event copilot.SessionEvent) {
		switch d := event.Data.(type) {
		case *copilot.AssistantMessageDeltaData:
			ts.handleAssistantDelta(sink, event.Type, d)
		case *copilot.AssistantMessageData:
			ts.handleAssistantMessage(sink, event.Type, d)
		case *copilot.ExternalToolRequestedData:
			ts.sendErr(sink.Send(adapterEvent("tool.invocation", map[string]any{
				"request_id":   d.RequestID,
				"tool_call_id": d.ToolCallID,
				"name":         d.ToolName,
				"arguments":    stringifyAny(d.Arguments),
				"event_type":   string(event.Type),
			})))
		case *copilot.ExternalToolCompletedData:
			ts.sendErr(sink.Send(adapterEvent("tool.result", map[string]any{
				"request_id": d.RequestID,
				"event_type": string(event.Type),
			})))
		case *copilot.SessionIdleData:
			select {
			case ts.turnDone <- struct{}{}:
			default:
			}
		}
	}
}

// handleAssistantDelta forwards a streaming delta event.
func (ts *turnState) handleAssistantDelta(sink pluginhost.ExecuteEventSender, eventType copilot.SessionEventType, d *copilot.AssistantMessageDeltaData) {
	if d.DeltaContent == "" {
		return
	}
	ts.sendErr(sink.Send(logEvent("agent", d.DeltaContent)))
	ts.sendErr(sink.Send(adapterEvent("agent.message", map[string]any{
		"message_id": d.MessageID,
		"delta":      d.DeltaContent,
		"event_type": string(eventType),
	})))
}

// handleAssistantMessage processes a complete assistant turn, forwarding
// content and tool invocations, then enforcing the max_turns limit.
func (ts *turnState) handleAssistantMessage(sink pluginhost.ExecuteEventSender, eventType copilot.SessionEventType, d *copilot.AssistantMessageData) {
	ts.finalContent = d.Content
	ts.sendErr(sink.Send(logEvent("agent", d.Content)))
	ts.sendErr(sink.Send(adapterEvent("agent.message", map[string]any{
		"message_id": d.MessageID,
		"content":    d.Content,
		"event_type": string(eventType),
	})))
	for _, tr := range d.ToolRequests {
		ts.sendErr(sink.Send(adapterEvent("tool.invocation", map[string]any{
			"tool_call_id": tr.ToolCallID,
			"name":         tr.Name,
			"arguments":    stringifyAny(tr.Arguments),
			"event_type":   string(eventType),
		})))
	}
	ts.assistantTurns++
	if ts.maxTurns > 0 && ts.assistantTurns >= ts.maxTurns {
		ts.sendErr(sink.Send(adapterEvent("limit.reached", map[string]any{
			"max_turns": strconv.Itoa(ts.maxTurns),
		})))
		ts.sendErr(errMaxTurnsReached)
	}
}

// awaitOutcome blocks until the session becomes idle, an error occurs, or ctx
// is cancelled. It emits the result event and returns.
func (ts *turnState) awaitOutcome(ctx context.Context, s *sessionState, sink pluginhost.ExecuteEventSender) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-ts.errCh:
			if errors.Is(err, errMaxTurnsReached) {
				return sink.Send(resultEvent("needs_review"))
			}
			return err
		case <-ts.turnDone:
			s.mu.Lock()
			denied := s.permissionDeny
			s.mu.Unlock()
			if denied {
				return sink.Send(resultEvent("needs_review"))
			}
			return sink.Send(resultEvent(parseOutcome(ts.finalContent)))
		}
	}
}

// applyRequestModel applies a per-request model override if cfg["model"] is
// set. This helper is intentionally extracted without behavior change so W09
// can fix the reasoning_effort-without-model drop in isolation.
func applyRequestModel(ctx context.Context, session copilotSession, cfg map[string]string) error {
	model := strings.TrimSpace(cfg["model"])
	if model == "" {
		return nil
	}
	var opts *copilot.SetModelOptions
	if effort := strings.TrimSpace(cfg["reasoning_effort"]); effort != "" {
		opts = &copilot.SetModelOptions{ReasoningEffort: &effort}
	}
	if err := session.SetModel(ctx, model, opts); err != nil {
		return fmt.Errorf("copilot: set model %q: %w", model, err)
	}
	return nil
}

// applyRequestEffort applies a per-step reasoning_effort override, returning a
// restore function that resets the effort to the agent-level default when called.
// The restore is always safe to call; it is a no-op when no override was applied.
//
// When cfg["model"] is also set, applyRequestModel handles the full model+effort
// combination. applyRequestEffort still provides the restore so that the session's
// default effort is reinstated after the step regardless of whether per-step model
// switching was also requested.
//
// Restore semantics: when defaultEffort == "" (session opened without a configured
// effort), the restore calls SetModel(defaultModel, nil) to clear the per-step
// override and let the server revert to its own default. Passing nil opts instead of
// a non-nil opts with empty effort avoids accidentally pinning an empty-string effort.
//
// SDK choice: SetModel is called with an empty model string when only effort is
// overridden. The Copilot SDK sends modelId="" in the model.switchTo RPC, which the
// server interprets as "apply only the effort change, keep the current model."
// The reviewer should verify this interpretation against the live server behaviour.
// If the server rejects modelId="", the loud-failure alternative is to return an
// error here directing the author to also specify a model.
func applyRequestEffort(ctx context.Context, s *sessionState, session copilotSession, cfg map[string]string) (func(), error) {
	effort := strings.TrimSpace(cfg["reasoning_effort"])
	if effort == "" {
		return func() {}, nil
	}

	if err := validateReasoningEffort(effort); err != nil {
		return nil, err
	}

	// When cfg also specifies a model, applyRequestModel (called after this)
	// will call SetModel with both model and effort. To avoid a redundant
	// effort-only SetModel call before that, we skip the forward apply here.
	// The restore is still registered so the default effort is reinstated.
	if strings.TrimSpace(cfg["model"]) == "" {
		if err := session.SetModel(ctx, "", &copilot.SetModelOptions{ReasoningEffort: &effort}); err != nil {
			return nil, fmt.Errorf("copilot: set per-step reasoning_effort %q: %w", effort, err)
		}
	}

	restore := func() {
		defaultModel := s.defaultModel
		defaultEffort := s.defaultEffort
		var opts *copilot.SetModelOptions
		if defaultEffort != "" {
			opts = &copilot.SetModelOptions{ReasoningEffort: &defaultEffort}
		}
		// Restore the session's default effort (or clear it when no default was set).
		// Errors here are best-effort: the step already completed and we cannot fail it
		// retroactively, so we log the failure and move on.
		if err := session.SetModel(ctx, defaultModel, opts); err != nil {
			slog.Warn("copilot: restore per-step reasoning_effort failed", "error", err)
		}
	}
	return restore, nil
}

// validateReasoningEffort returns an error when effort is not in the documented set.
func validateReasoningEffort(effort string) error {
	if !validReasoningEfforts[effort] {
		return fmt.Errorf("copilot: reasoning_effort %q is not valid; valid values: low, medium, high, xhigh", effort)
	}
	return nil
}

func (p *copilotPlugin) Permit(_ context.Context, req *pb.PermitRequest) (*pb.PermitResponse, error) {
	s := p.getSession(req.GetSessionId())
	if s == nil {
		return nil, fmt.Errorf("copilot: unknown session %q", req.GetSessionId())
	}

	s.mu.Lock()
	ch, ok := s.pending[req.GetPermissionId()]
	s.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("copilot: no pending permission %q", req.GetPermissionId())
	}

	ch <- permDecision{allow: req.GetAllow(), reason: req.GetReason()}
	return &pb.PermitResponse{}, nil
}

func (p *copilotPlugin) CloseSession(_ context.Context, req *pb.CloseSessionRequest) (*pb.CloseSessionResponse, error) {
	p.mu.Lock()
	s, ok := p.sessions[req.GetSessionId()]
	if ok {
		delete(p.sessions, req.GetSessionId())
	}
	p.mu.Unlock()
	if !ok {
		return &pb.CloseSessionResponse{}, nil
	}

	disconnectDone := make(chan error, 1)
	go func() {
		disconnectDone <- s.session.Disconnect()
	}()

	select {
	case err := <-disconnectDone:
		if err != nil {
			_ = s.session.Destroy()
			return &pb.CloseSessionResponse{}, fmt.Errorf("copilot: disconnect session: %w", err)
		}
	case <-time.After(closeSessionGrace):
		_ = s.session.Destroy()
	}

	return &pb.CloseSessionResponse{}, nil
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

func (p *copilotPlugin) handlePermissionRequest(sessionID string, request copilot.PermissionRequest) (copilot.PermissionRequestResult, error) {
	s := p.getSession(sessionID)
	if s == nil {
		return copilot.PermissionRequestResult{Kind: copilot.PermissionRequestResultKindDeniedCouldNotRequestFromUser}, nil
	}

	permID := uuid.NewString()
	details := permissionDetails(request)

	s.mu.Lock()
	sink := s.sink
	active := s.active
	done := s.activeCh
	if !active || sink == nil {
		s.mu.Unlock()
		return copilot.PermissionRequestResult{Kind: copilot.PermissionRequestResultKindDeniedCouldNotRequestFromUser}, nil
	}
	ch := make(chan permDecision, 1)
	s.pending[permID] = ch
	s.mu.Unlock()

	sendErr := sink.Send(&pb.ExecuteEvent{
		Event: &pb.ExecuteEvent_Permission{
			Permission: &pb.PermissionRequest{
				Id:         permID,
				Permission: details["kind"],
				Details:    details,
			},
		},
	})
	if sendErr != nil {
		s.mu.Lock()
		delete(s.pending, permID)
		s.mu.Unlock()
		return copilot.PermissionRequestResult{Kind: copilot.PermissionRequestResultKindDeniedCouldNotRequestFromUser}, sendErr
	}

	select {
	case decision := <-ch:
		s.mu.Lock()
		delete(s.pending, permID)
		if !decision.allow {
			s.permissionDeny = true
		}
		s.mu.Unlock()
		if decision.allow {
			return copilot.PermissionRequestResult{Kind: copilot.PermissionRequestResultKindApproved}, nil
		}
		return copilot.PermissionRequestResult{Kind: copilot.PermissionRequestResultKindDeniedInteractivelyByUser}, nil
	case <-done:
		s.mu.Lock()
		delete(s.pending, permID)
		s.mu.Unlock()
		return copilot.PermissionRequestResult{Kind: copilot.PermissionRequestResultKindNoResult}, nil
	}
}

func resultEvent(outcome string) *pb.ExecuteEvent {
	return &pb.ExecuteEvent{
		Event: &pb.ExecuteEvent_Result{
			Result: &pb.ExecuteResult{Outcome: outcome},
		},
	}
}

func logEvent(stream, chunk string) *pb.ExecuteEvent {
	if !strings.HasSuffix(chunk, "\n") {
		chunk += "\n"
	}
	return &pb.ExecuteEvent{
		Event: &pb.ExecuteEvent_Log{
			Log: &pb.LogEvent{Stream: stream, Chunk: []byte(chunk)},
		},
	}
}

func adapterEvent(kind string, data map[string]any) *pb.ExecuteEvent {
	s, _ := structpb.NewStruct(data)
	return &pb.ExecuteEvent{
		Event: &pb.ExecuteEvent_Adapter{
			Adapter: &pb.AdapterEvent{
				Kind: kind,
				Data: s,
			},
		},
	}
}

func parseOutcome(content string) string {
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		lower := strings.ToLower(trimmed)
		if strings.HasPrefix(lower, resultPrefix) {
			outcome := strings.TrimSpace(trimmed[len(resultPrefix):])
			if outcome == "" {
				return "needs_review"
			}
			return strings.ToLower(outcome)
		}
	}
	return "needs_review"
}

func stringifyAny(v any) string {
	if v == nil {
		return ""
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

func permissionDetails(request copilot.PermissionRequest) map[string]string {
	includeSensitive := includeSensitivePermissionDetails()

	details := map[string]string{
		"kind": string(request.Kind),
	}
	if request.ToolCallID != nil {
		details["tool_call_id"] = *request.ToolCallID
	}
	if request.Intention != nil {
		details["intention"] = *request.Intention
	}
	if includeSensitive && request.FullCommandText != nil {
		details["full_command_text"] = *request.FullCommandText
	}
	if includeSensitive && request.Path != nil {
		details["path"] = *request.Path
	}
	if includeSensitive && request.URL != nil {
		details["url"] = *request.URL
	}
	if request.ServerName != nil {
		details["server_name"] = *request.ServerName
	}
	if request.ToolName != nil {
		details["tool_name"] = *request.ToolName
	}
	if request.Warning != nil {
		details["warning"] = *request.Warning
	}
	if includeSensitive && request.Args != nil {
		details["args"] = stringifyAny(request.Args)
	}
	if includeSensitive && request.ToolArgs != nil {
		details["tool_args"] = stringifyAny(request.ToolArgs)
	}
	if includeSensitive && len(request.PossiblePaths) > 0 {
		details["possible_paths"] = strings.Join(request.PossiblePaths, ",")
	}
	if len(request.Commands) > 0 {
		cmds := make([]string, 0, len(request.Commands))
		for _, cmd := range request.Commands {
			if strings.TrimSpace(cmd.Identifier) != "" {
				cmds = append(cmds, cmd.Identifier)
			}
		}
		if len(cmds) > 0 {
			details["commands"] = strings.Join(cmds, ",")
		}
	}

	if includeSensitive {
		if b, err := json.Marshal(request); err == nil {
			details["request_json"] = string(b)
		}
	}
	return details
}

// includeSensitivePermissionDetails controls whether rich permission payload
// fields (full command, paths/URLs, args, raw request JSON) are forwarded.
// Default is redacted to reduce sensitive data retention risk.
func includeSensitivePermissionDetails() bool {
	return os.Getenv(includeSensitivePermissionDetailsEnv) == "1"
}
