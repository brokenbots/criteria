package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/brokenbots/criteria/cmd/criteria-adapter-mcp/mcpclient"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
	pluginhost "github.com/brokenbots/criteria/sdk/pluginhost"
)

const (
	pluginName    = "mcp"
	pluginVersion = "0.1.0"

	closeGrace  = 5 * time.Second
	initTimeout = 5 * time.Second
)

var reservedExecuteKeys = map[string]struct{}{
	"tool":            {},
	"success_outcome": {},
	"command":         {},
	"args":            {},
	"env":             {},
	"cwd":             {},
}

type sessionState struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	client *mcpclient.Client
	stderr *bytes.Buffer

	execMu sync.Mutex

	mu       sync.Mutex
	tools    map[string]struct{}
	sink     pluginhost.ExecuteEventSender
	inFlight bool
}

func (s *sessionState) setSink(sink pluginhost.ExecuteEventSender, inFlight bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sink = sink
	s.inFlight = inFlight
}

func (s *sessionState) currentSink() (pluginhost.ExecuteEventSender, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sink, s.inFlight
}

func (s *sessionState) clearSink() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sink = nil
	s.inFlight = false
}

type MCPBridge struct {
	mu       sync.Mutex
	sessions map[string]*sessionState
}

func (b *MCPBridge) Info(_ context.Context, _ *pb.InfoRequest) (*pb.InfoResponse, error) {
	return &pb.InfoResponse{
		Name:         pluginName,
		Version:      pluginVersion,
		Capabilities: []string{"single_shot"},
		ConfigSchema: &pb.AdapterSchemaProto{Fields: map[string]*pb.ConfigFieldProto{
			"command": {Required: true, Type: "string", Doc: "MCP server binary to launch."},
			"args":    {Type: "string", Doc: "Comma-separated argument list for the server binary."},
			"env":     {Type: "string", Doc: "Comma-separated KEY=VALUE environment variable pairs."},
			"cwd":     {Type: "string", Doc: "Working directory for the MCP server process."},
		}},
		InputSchema: &pb.AdapterSchemaProto{Fields: map[string]*pb.ConfigFieldProto{
			"tool":            {Required: true, Type: "string", Doc: "MCP tool name to invoke."},
			"success_outcome": {Type: "string", Doc: "Outcome to report on success (default: success)."},
		}},
	}, nil
}

func (b *MCPBridge) OpenSession(ctx context.Context, req *pb.OpenSessionRequest) (*pb.OpenSessionResponse, error) { //nolint:funlen,gocyclo // complex session setup across MCP config, TLS, and stdio transport
	cfg := req.GetConfig()
	command := strings.TrimSpace(cfg["command"])
	if command == "" {
		return nil, fmt.Errorf("mcp: config.command is required")
	}

	args, err := parseCSVList(cfg["args"])
	if err != nil {
		return nil, fmt.Errorf("mcp: parse args: %w", err)
	}
	envPairs, err := parseEnvPairs(cfg["env"])
	if err != nil {
		return nil, fmt.Errorf("mcp: parse env: %w", err)
	}

	cmd := exec.Command(command, args...)
	if cwd := strings.TrimSpace(cfg["cwd"]); cwd != "" {
		cmd.Dir = cwd
	}
	if len(envPairs) > 0 {
		cmd.Env = append(os.Environ(), envPairs...)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("mcp: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("mcp: stdout pipe: %w", err)
	}
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("mcp: start server %q: %w", command, err)
	}

	state := &sessionState{cmd: cmd, stdin: stdin, stderr: stderr, tools: map[string]struct{}{}}
	state.client = mcpclient.New(stdout, stdin, func(n mcpclient.Notification) {
		if n.Method != "notifications/progress" {
			return
		}
		sink, inFlight := state.currentSink()
		if !inFlight || sink == nil {
			return
		}
		_ = sink.Send(adapterEvent("mcp.progress", n.Params))
	})

	handshakeCtx, cancel := context.WithTimeout(ctx, initTimeout)
	defer cancel()
	if err := state.client.Initialize(handshakeCtx, "criteria-adapter-mcp", pluginVersion); err != nil {
		_ = shutdownSession(ctx, state)
		return nil, fmt.Errorf("mcp: initialize: %w", err)
	}
	tools, err := state.client.ListTools(handshakeCtx)
	if err != nil {
		_ = shutdownSession(ctx, state)
		return nil, fmt.Errorf("mcp: tools/list: %w", err)
	}
	for _, tool := range tools {
		if tool.Name != "" {
			state.tools[tool.Name] = struct{}{}
		}
	}

	b.mu.Lock()
	if existing, ok := b.sessions[req.GetSessionId()]; ok {
		b.mu.Unlock()
		_ = shutdownSession(ctx, state)
		_ = shutdownSession(ctx, existing)
		return nil, fmt.Errorf("mcp: session %q already open", req.GetSessionId())
	}
	b.sessions[req.GetSessionId()] = state
	b.mu.Unlock()

	return &pb.OpenSessionResponse{}, nil
}

func (b *MCPBridge) Execute(ctx context.Context, req *pb.ExecuteRequest, sink pluginhost.ExecuteEventSender) error { //nolint:funlen,gocognit // event-driven tool dispatch with permission gating and chunked output
	s := b.getSession(req.GetSessionId())
	if s == nil {
		return fmt.Errorf("mcp: unknown session %q", req.GetSessionId())
	}

	toolName := strings.TrimSpace(req.GetConfig()["tool"])
	if toolName == "" {
		return fmt.Errorf("mcp: config.tool is required")
	}
	if _, ok := s.tools[toolName]; !ok {
		return fmt.Errorf("mcp: unknown tool %q", toolName)
	}

	arguments := make(map[string]any, len(req.GetConfig()))
	for k, v := range req.GetConfig() {
		if _, reserved := reservedExecuteKeys[k]; reserved {
			continue
		}
		arguments[k] = v
	}

	s.execMu.Lock()
	defer s.execMu.Unlock()
	s.setSink(sink, true)
	defer s.clearSink()

	result, err := s.client.CallTool(ctx, toolName, arguments)
	if err != nil {
		return fmt.Errorf("mcp: tools/call %q: %w", toolName, err)
	}

	for _, item := range result.Content {
		typ, _ := item["type"].(string)
		if typ == "text" {
			text, _ := item["text"].(string)
			if strings.TrimSpace(text) != "" {
				if err := sink.Send(logEvent("agent", text)); err != nil {
					return err
				}
			}
			continue
		}
		if err := sink.Send(adapterEvent("mcp.content", item)); err != nil {
			return err
		}
	}

	outcome := "success"
	if configured := strings.TrimSpace(req.GetConfig()["success_outcome"]); configured != "" {
		outcome = configured
	}
	if result.IsError {
		outcome = "failure"
	}
	return sink.Send(resultEvent(outcome))
}

func (b *MCPBridge) Permit(context.Context, *pb.PermitRequest) (*pb.PermitResponse, error) {
	return &pb.PermitResponse{}, nil
}

func (b *MCPBridge) CloseSession(ctx context.Context, req *pb.CloseSessionRequest) (*pb.CloseSessionResponse, error) {
	b.mu.Lock()
	s, ok := b.sessions[req.GetSessionId()]
	if ok {
		delete(b.sessions, req.GetSessionId())
	}
	b.mu.Unlock()
	if !ok {
		return &pb.CloseSessionResponse{}, nil
	}
	if err := shutdownSession(ctx, s); err != nil {
		return &pb.CloseSessionResponse{}, err
	}
	return &pb.CloseSessionResponse{}, nil
}

func (b *MCPBridge) getSession(id string) *sessionState {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sessions[id]
}

func shutdownSession(ctx context.Context, s *sessionState) error {
	if s == nil {
		return nil
	}
	_, inFlight := s.currentSink()
	if inFlight {
		_ = s.client.Notification(context.WithoutCancel(ctx), "notifications/cancelled", map[string]any{"reason": "session_close"})
	}
	s.client.Close()
	_ = s.stdin.Close()

	waitDone := make(chan error, 1)
	go func() {
		waitDone <- s.cmd.Wait()
	}()

	select {
	case err := <-waitDone:
		if err != nil && !strings.Contains(err.Error(), "signal: killed") {
			return fmt.Errorf("mcp: wait server exit: %w", err)
		}
		return nil
	case <-time.After(closeGrace):
		_ = s.cmd.Process.Kill()
		<-waitDone
		return nil
	}
}

func logEvent(stream, chunk string) *pb.ExecuteEvent {
	if !strings.HasSuffix(chunk, "\n") {
		chunk += "\n"
	}
	return &pb.ExecuteEvent{
		Event: &pb.ExecuteEvent_Log{Log: &pb.LogEvent{Stream: stream, Chunk: []byte(chunk)}},
	}
}

func resultEvent(outcome string) *pb.ExecuteEvent {
	return &pb.ExecuteEvent{
		Event: &pb.ExecuteEvent_Result{Result: &pb.ExecuteResult{Outcome: outcome}},
	}
}

func adapterEvent(kind string, data map[string]any) *pb.ExecuteEvent {
	payload := map[string]any{}
	for k, v := range data {
		payload[k] = v
	}
	s, _ := structpb.NewStruct(payload)
	return &pb.ExecuteEvent{
		Event: &pb.ExecuteEvent_Adapter{Adapter: &pb.AdapterEvent{Kind: kind, Data: s}},
	}
}

func parseCSVList(raw string) ([]string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, nil
	}
	r := csv.NewReader(strings.NewReader(trimmed))
	r.TrimLeadingSpace = true
	r.FieldsPerRecord = -1
	vals, err := r.Read()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		v = strings.TrimSpace(v)
		if v != "" {
			out = append(out, v)
		}
	}
	return out, nil
}

func parseEnvPairs(raw string) ([]string, error) {
	vals, err := parseCSVList(raw)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		if !strings.Contains(v, "=") {
			return nil, fmt.Errorf("invalid env pair %q", v)
		}
		parts := strings.SplitN(v, "=", 2)
		if strings.TrimSpace(parts[0]) == "" {
			return nil, fmt.Errorf("invalid env key in %q", v)
		}
		out = append(out, parts[0]+"="+parts[1])
	}
	return out, nil
}
