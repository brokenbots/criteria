package main

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	adapterhost "github.com/brokenbots/criteria-go-adapter-sdk/adapterhost"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/brokenbots/criteria/internal/adapterhost/heartbeatutil"
)

type noopService struct {
	mu       sync.Mutex
	sessions map[string]struct{}

	// pendingLogs carries Execute-requested log lines to the session's Log
	// stream: Execute queues a line for input["emit_log"], Log pumps it out
	// as a real v2.LogEvent. This lets tests assert log-line delivery over a
	// fixture that otherwise produces no output of its own.
	pendingLogs chan []byte

	toolBridgeOnce sync.Once
	toolBridge     *toolCallBridge
}

// Permissions drives the tool-call bridge: typed tool_call_result replies,
// denials, and allow-grant ACKs all arrive on this stream, so the bridge must
// run here (the SDK's embedded UnimplementedPermissions would auto-allow and
// drop the typed replies, wedging every tool call). Plain permission traffic
// keeps the auto-allow semantics the fixture has always had.
func (s *noopService) Permissions(ctx context.Context, stream adapterhost.PermissionsStream) error {
	return s.bridge().Permissions(ctx, stream)
}

// bridge returns the adapter's tool-call bridge, creating it on first use so
// adapters that never call tools keep the zero value.
func (s *noopService) bridge() *toolCallBridge {
	s.toolBridgeOnce.Do(func() { s.toolBridge = &toolCallBridge{} })
	return s.toolBridge
}

func (s *noopService) Info(_ context.Context, _ *v2.InfoRequest) (*v2.InfoResponse, error) {
	return &v2.InfoResponse{
		Name:               "noop",
		Version:            "0.1.0",
		SourceUrl:          "https://github.com/brokenbots/criteria",
		SdkProtocolVersion: "2",
		Platforms:          []string{"linux/amd64", "linux/arm64", "darwin/arm64"},
		Capabilities:       []string{"parallel_safe", "adapter_tools", "permission_gating", "permission_request_forwarding"},
	}, nil
}

func (s *noopService) OpenSession(_ context.Context, req *v2.OpenSessionRequest) (*v2.OpenSessionResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[req.GetSessionId()] = struct{}{}
	return &v2.OpenSessionResponse{}, nil
}

func (s *noopService) Execute(ctx context.Context, req *v2.ExecuteRequest, sink adapterhost.ExecuteEventSender) error {
	s.mu.Lock()
	_, ok := s.sessions[req.GetSessionId()]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("unknown session %q", req.GetSessionId())
	}
	// Queue the requested log line before any delay so it is delivered ahead
	// of later events — the ordering tests (e.g. pre-crash log capture) rely
	// on.
	if line := req.GetInput()["emit_log"]; line != "" && s.pendingLogs != nil {
		select {
		case s.pendingLogs <- []byte(line):
		default:
			return fmt.Errorf("log queue full")
		}
	}
	if rawDelay := req.GetInput()["delay_ms"]; rawDelay != "" {
		delayMS, err := strconv.Atoi(rawDelay)
		if err != nil || delayMS < 0 {
			return fmt.Errorf("invalid delay_ms %q", rawDelay)
		}
		if delayMS > 0 {
			timer := time.NewTimer(time.Duration(delayMS) * time.Millisecond)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	if req.GetInput()["emit_permission_request"] == "true" {
		payload, err := structpb.NewStruct(map[string]any{
			"kind":       "shell",
			"request_id": "noop-perm-1",
			"tool":       "shell",
		})
		if err != nil {
			return fmt.Errorf("build permission.request payload: %w", err)
		}
		if err := sink.Send(&v2.ExecuteEvent{
			Event: &v2.ExecuteEvent_Adapter{
				Adapter: &v2.AdapterEvent{
					EventKind: "permission.request",
					Payload:   payload,
				},
			},
		}); err != nil {
			return err
		}
	}
	// Tool-call modes (CRI-165): tool_target selects the caller mode; the
	// outputs passthrough is its data-ish callee counterpart. The modes are
	// mutually exclusive with each other.
	input := req.GetInput()
	_, hasToolTarget := input[inputToolTarget]
	_, hasToolName := input[inputToolName]
	_, hasToolArgs := input[inputToolArgs]
	_, hasOutputs := input[inputOutputs]
	toolCallMode := hasToolTarget || hasToolName || hasToolArgs
	switch {
	case toolCallMode && hasOutputs:
		return fmt.Errorf("invalid input: %q and %q are mutually exclusive", inputToolTarget, inputOutputs)
	case toolCallMode:
		return s.executeToolCall(ctx, input, sink)
	case hasOutputs:
		return executeNoopPassthrough(input, sink)
	}
	if connectAddr := req.GetInput()["connect"]; connectAddr != "" {
		outcome := probeConnect(connectAddr)
		return sink.Send(&v2.ExecuteEvent{
			Event: &v2.ExecuteEvent_Result{Result: &v2.ExecuteResult{Outcome: outcome}},
		})
	}
	return sink.Send(&v2.ExecuteEvent{
		Event: &v2.ExecuteEvent_Result{Result: &v2.ExecuteResult{Outcome: "success"}},
	})
}

// probeConnect attempts a TCP connection to addr and returns an outcome string
// suitable for asserting network namespace behavior in tests.
func probeConnect(addr string) string {
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err == nil {
		_ = conn.Close()
		return "connect_ok"
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "network is unreachable") ||
		strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "no route to host") ||
		strings.Contains(msg, "timeout") ||
		strings.Contains(msg, "context deadline exceeded") {
		return "network_unreachable"
	}
	return "connect_fail"
}

func (s *noopService) Log(ctx context.Context, req *v2.LogRequest, sender adapterhost.LogEventSender) error {
	// The log stream must remain open for the lifetime of the session. Returning
	// immediately would stop the SDK heartbeat ticker and break the host's
	// liveness contract, so block until the host cancels the stream.
	//
	// heartbeatutil.RunLogHeartbeat is a transitional shim: the Go SDK should
	// own session-lifetime heartbeats (see PR #283 Follow-ups). Remove this once
	// the SDK fix lands.
	logsCtx, stop := context.WithCancel(ctx)
	defer stop()
	go func() {
		for {
			select {
			case <-logsCtx.Done():
				return
			case line := <-s.pendingLogs:
				if err := sender.Send(&v2.LogEvent{
					SessionId:  req.GetSessionId(),
					StreamName: "stdout",
					Line:       line,
					Timestamp:  timestamppb.Now(),
				}); err != nil {
					return
				}
			}
		}
	}()
	return heartbeatutil.RunLogHeartbeat(ctx, sender)
}

func (s *noopService) CloseSession(_ context.Context, req *v2.CloseSessionRequest) (*v2.CloseSessionResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, req.GetSessionId())
	return &v2.CloseSessionResponse{}, nil
}

func main() {
	adapterhost.Serve(&noopService{
		sessions:    map[string]struct{}{},
		pendingLogs: make(chan []byte, 16),
	})
}
