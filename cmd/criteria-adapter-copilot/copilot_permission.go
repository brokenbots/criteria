// copilot_permission.go — Copilot permission-request bridging: Permit RPC and
// the SDK OnPermissionRequest callback that forwards requests to the host engine.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	copilot "github.com/github/copilot-sdk/go"
	"github.com/google/uuid"

	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

func (p *copilotAdapter) Permit(_ context.Context, req *pb.PermitRequest) (*pb.PermitResponse, error) {
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

func (p *copilotAdapter) handlePermissionRequest(sessionID string, request copilot.PermissionRequest) (copilot.PermissionRequestResult, error) {
	s := p.getSession(sessionID)
	if s == nil {
		return copilot.PermissionRequestResult{Kind: copilot.PermissionRequestResultKindUserNotAvailable}, nil
	}

	permID := uuid.NewString()
	details := permissionDetails(request)

	s.mu.Lock()
	sink := s.sink
	active := s.active
	done := s.activeCh
	if !active || sink == nil {
		s.mu.Unlock()
		return copilot.PermissionRequestResult{Kind: copilot.PermissionRequestResultKindUserNotAvailable}, nil
	}
	ch := make(chan permDecision, 1)
	s.pending[permID] = ch
	s.mu.Unlock()

	sendErr := sink.Send(buildPermissionEvent(permID, details))
	if sendErr != nil {
		s.mu.Lock()
		delete(s.pending, permID)
		s.mu.Unlock()
		return copilot.PermissionRequestResult{Kind: copilot.PermissionRequestResultKindUserNotAvailable}, sendErr
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
		return copilot.PermissionRequestResult{Kind: copilot.PermissionRequestResultKindRejected}, nil
	case <-done:
		s.mu.Lock()
		delete(s.pending, permID)
		s.mu.Unlock()
		return copilot.PermissionRequestResult{Kind: copilot.PermissionRequestResultKindNoResult}, nil
	}
}

func buildPermissionEvent(permID string, details map[string]string) *pb.ExecuteEvent {
	return &pb.ExecuteEvent{
		Event: &pb.ExecuteEvent_Permission{
			Permission: &pb.PermissionRequest{
				Id:         permID,
				Permission: details["kind"],
				Details:    details,
			},
		},
	}
}

func permissionDetails(request copilot.PermissionRequest) map[string]string { //nolint:funlen,gocognit,gocyclo // collecting optional fields from SDK request variants; splitting further would obscure the boundary mapping
	includeSensitive := includeSensitivePermissionDetails()

	details := map[string]string{
		"kind": string(request.Kind()),
	}
	switch req := request.(type) {
	case copilot.PermissionRequestCustomTool:
		setString(details, "tool_call_id", req.ToolCallID)
		if req.ToolName != "" {
			details["tool_name"] = req.ToolName
		}
		if includeSensitive && req.Args != nil {
			details["args"] = stringifyAny(req.Args)
		}
	case copilot.PermissionRequestExtensionManagement:
		setString(details, "tool_call_id", req.ToolCallID)
		setString(details, "extension_name", req.ExtensionName)
		if req.Operation != "" {
			details["operation"] = req.Operation
		}
	case copilot.PermissionRequestExtensionPermissionAccess:
		setString(details, "tool_call_id", req.ToolCallID)
		if req.ExtensionName != "" {
			details["extension_name"] = req.ExtensionName
		}
		if len(req.Capabilities) > 0 {
			details["capabilities"] = strings.Join(req.Capabilities, ",")
		}
	case copilot.PermissionRequestHook:
		setString(details, "tool_call_id", req.ToolCallID)
		if req.ToolName != "" {
			details["tool_name"] = req.ToolName
		}
		setString(details, "hook_message", req.HookMessage)
		if includeSensitive && req.ToolArgs != nil {
			details["tool_args"] = stringifyAny(req.ToolArgs)
		}
	case copilot.PermissionRequestMcp:
		setString(details, "tool_call_id", req.ToolCallID)
		if req.ServerName != "" {
			details["server_name"] = req.ServerName
		}
		if req.ToolName != "" {
			details["tool_name"] = req.ToolName
		}
		if includeSensitive && req.Args != nil {
			details["args"] = stringifyAny(req.Args)
		}
	case copilot.PermissionRequestMemory:
		setString(details, "tool_call_id", req.ToolCallID)
		if req.Fact != "" {
			details["fact"] = req.Fact
		}
		setString(details, "subject", req.Subject)
		setString(details, "citations", req.Citations)
		setString(details, "reason", req.Reason)
	case copilot.PermissionRequestRead:
		setString(details, "tool_call_id", req.ToolCallID)
		if req.Intention != "" {
			details["intention"] = req.Intention
		}
		if includeSensitive && req.Path != "" {
			details["path"] = req.Path
		}
	case copilot.PermissionRequestShell:
		setString(details, "tool_call_id", req.ToolCallID)
		if req.Intention != "" {
			details["intention"] = req.Intention
		}
		setString(details, "warning", req.Warning)
		if includeSensitive && req.FullCommandText != "" {
			details["full_command_text"] = req.FullCommandText
		}
		if includeSensitive && len(req.PossiblePaths) > 0 {
			if len(req.PossiblePaths) == 1 {
				details["path"] = req.PossiblePaths[0]
			}
			details["possible_paths"] = strings.Join(req.PossiblePaths, ",")
		}
		if len(req.Commands) > 0 {
			cmds := make([]string, 0, len(req.Commands))
			for _, cmd := range req.Commands {
				if strings.TrimSpace(cmd.Identifier) != "" {
					cmds = append(cmds, cmd.Identifier)
				}
			}
			if len(cmds) > 0 {
				details["commands"] = strings.Join(cmds, ",")
			}
		}
	case copilot.PermissionRequestURL:
		setString(details, "tool_call_id", req.ToolCallID)
		if req.Intention != "" {
			details["intention"] = req.Intention
		}
		if includeSensitive && req.URL != "" {
			details["url"] = req.URL
		}
	case copilot.PermissionRequestWrite:
		setString(details, "tool_call_id", req.ToolCallID)
		if req.Intention != "" {
			details["intention"] = req.Intention
		}
		if includeSensitive && req.FileName != "" {
			details["path"] = req.FileName
		}
	}

	if includeSensitive {
		if b, err := json.Marshal(request); err == nil {
			details["request_json"] = string(b)
		}
	}
	return details
}

func setString(details map[string]string, key string, value *string) {
	if value != nil && *value != "" {
		details[key] = *value
	}
}

// includeSensitivePermissionDetails controls whether rich permission payload
// fields (full command, paths/URLs, args, raw request JSON) are forwarded.
// Default is redacted to reduce sensitive data retention risk.
func includeSensitivePermissionDetails() bool {
	return os.Getenv(includeSensitivePermissionDetailsEnv) == "1"
}
