// Package main implements the overlord-adapter-copilot out-of-process plugin.
//
// The plugin preserves the Overseer plugin boundary while using the Copilot SDK
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
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	copilot "github.com/github/copilot-sdk/go"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/structpb"

	pluginpkg "github.com/brokenbots/overlord/overseer/internal/plugin"
	pb "github.com/brokenbots/overlord/shared/pb/overlord/v1"
)

const (
	pluginName    = "copilot"
	pluginVersion = "0.1.0"

	defaultBinEnv = "OVERLORD_COPILOT_BIN"
	defaultBin    = "copilot"

	includeSensitivePermissionDetailsEnv = "OVERLORD_COPILOT_INCLUDE_SENSITIVE_PERMISSION_DETAILS"

	resultPrefix = "result:"
)

var errMaxTurnsReached = errors.New("copilot: max_turns reached")
var closeSessionGrace = 5 * time.Second

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
	sink           pluginpkg.ExecuteEventSender
	permissionDeny bool
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
			"working_directory": {Type: "string", Doc: "Working directory for tool invocations."},
			"max_turns":         {Type: "number", Doc: "Maximum assistant turns per Execute call (default: unlimited)."},
			"system_prompt":     {Type: "string", Doc: "System prompt prepended at session open."},
		}},
		InputSchema: &pb.AdapterSchemaProto{Fields: map[string]*pb.ConfigFieldProto{
			"prompt":    {Required: true, Type: "string", Doc: "User prompt to send to the assistant."},
			"max_turns": {Type: "number", Doc: "Per-step override for max assistant turns."},
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
	sessionConfig := &copilot.SessionConfig{
		Streaming: true,
		Model:     cfg["model"],
		OnPermissionRequest: func(r copilot.PermissionRequest, _ copilot.PermissionInvocation) (copilot.PermissionRequestResult, error) {
			return p.handlePermissionRequest(pluginSessionID, r)
		},
	}
	if wd := strings.TrimSpace(cfg["working_directory"]); wd != "" {
		sessionConfig.WorkingDirectory = wd
	}
	if sp := strings.TrimSpace(cfg["system_prompt"]); sp != "" {
		sessionConfig.SystemMessage = &copilot.SystemMessageConfig{Content: sp}
	}

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

	return &pb.OpenSessionResponse{}, nil
}

func (p *copilotPlugin) Execute(ctx context.Context, req *pb.ExecuteRequest, sink pluginpkg.ExecuteEventSender) error {
	s := p.getSession(req.GetSessionId())
	if s == nil {
		return fmt.Errorf("copilot: unknown session %q", req.GetSessionId())
	}

	prompt := strings.TrimSpace(req.GetConfig()["prompt"])
	if prompt == "" {
		return fmt.Errorf("copilot: config.prompt is required")
	}

	maxTurns := 0
	if raw := strings.TrimSpace(req.GetConfig()["max_turns"]); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			return fmt.Errorf("copilot: invalid max_turns %q", raw)
		}
		maxTurns = n
	}

	s.execMu.Lock()
	defer s.execMu.Unlock()

	execDone := make(chan struct{})
	s.mu.Lock()
	s.active = true
	s.activeCh = execDone
	s.sink = sink
	s.permissionDeny = false
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.active = false
		s.sink = nil
		if s.activeCh != nil {
			close(s.activeCh)
			s.activeCh = nil
		}
		s.mu.Unlock()
	}()

	finalContent := ""
	assistantTurns := 0
	turnDone := make(chan struct{}, 1)
	errCh := make(chan error, 1)

	sendErr := func(err error) {
		if err == nil {
			return
		}
		select {
		case errCh <- err:
		default:
		}
	}

	unsubscribe := s.session.On(func(event copilot.SessionEvent) {
		switch d := event.Data.(type) {
		case *copilot.AssistantMessageDeltaData:
			if d.DeltaContent != "" {
				sendErr(sink.Send(logEvent("agent", d.DeltaContent)))
				sendErr(sink.Send(adapterEvent("agent.message", map[string]any{
					"message_id": d.MessageID,
					"delta":      d.DeltaContent,
					"event_type": string(event.Type),
				})))
			}

		case *copilot.AssistantMessageData:
			finalContent = d.Content
			sendErr(sink.Send(logEvent("agent", d.Content)))
			sendErr(sink.Send(adapterEvent("agent.message", map[string]any{
				"message_id": d.MessageID,
				"content":    d.Content,
				"event_type": string(event.Type),
			})))

			for _, tr := range d.ToolRequests {
				sendErr(sink.Send(adapterEvent("tool.invocation", map[string]any{
					"tool_call_id": tr.ToolCallID,
					"name":         tr.Name,
					"arguments":    stringifyAny(tr.Arguments),
					"event_type":   string(event.Type),
				})))
			}

			assistantTurns++
			if maxTurns > 0 && assistantTurns >= maxTurns {
				sendErr(sink.Send(adapterEvent("limit.reached", map[string]any{
					"max_turns": strconv.Itoa(maxTurns),
				})))
				sendErr(errMaxTurnsReached)
				return
			}

		case *copilot.ExternalToolRequestedData:
			sendErr(sink.Send(adapterEvent("tool.invocation", map[string]any{
				"request_id":   d.RequestID,
				"tool_call_id": d.ToolCallID,
				"name":         d.ToolName,
				"arguments":    stringifyAny(d.Arguments),
				"event_type":   string(event.Type),
			})))

		case *copilot.ExternalToolCompletedData:
			sendErr(sink.Send(adapterEvent("tool.result", map[string]any{
				"request_id": d.RequestID,
				"event_type": string(event.Type),
			})))

		case *copilot.SessionIdleData:
			select {
			case turnDone <- struct{}{}:
			default:
			}
		}
	})
	defer unsubscribe()

	if model := strings.TrimSpace(req.GetConfig()["model"]); model != "" {
		if err := s.session.SetModel(ctx, model, nil); err != nil {
			return fmt.Errorf("copilot: set model %q: %w", model, err)
		}
	}

	if _, err := s.session.Send(ctx, copilot.MessageOptions{Prompt: prompt}); err != nil {
		return fmt.Errorf("copilot: send prompt: %w", err)
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-errCh:
			if errors.Is(err, errMaxTurnsReached) {
				return sink.Send(resultEvent("needs_review"))
			}
			return err
		case <-turnDone:
			s.mu.Lock()
			denied := s.permissionDeny
			s.mu.Unlock()
			if denied {
				return sink.Send(resultEvent("needs_review"))
			}
			return sink.Send(resultEvent(parseOutcome(finalContent)))
		}
	}
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
		return copilot.PermissionRequestResult{Kind: copilot.PermissionRequestResultKindDeniedCouldNotRequestFromUser}, nil
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
