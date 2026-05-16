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

func (p *copilotAdapter) handlePermissionRequest(sessionID string, request *copilot.PermissionRequest) (copilot.PermissionRequestResult, error) {
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

func permissionDetails(request *copilot.PermissionRequest) map[string]string { //nolint:funlen,gocognit,gocyclo // collecting optional fields from a struct; splitting into helpers would obscure the data contract
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
