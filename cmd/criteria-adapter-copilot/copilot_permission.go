// copilot_permission.go — Copilot permission-request bridging: the SDK
// OnPermissionRequest callback that forwards requests to the host engine as
// AdapterEvent(kind="permission.request"). WS03 auto-approves every request
// so tool calls proceed normally; WS16 adds a bidi Permissions stream for
// interactive grant/deny decisions.

package main

import (
	"encoding/json"
	"os"
	"strings"

	copilot "github.com/github/copilot-sdk/go"
	"github.com/google/uuid"
)

func (p *copilotAdapter) handlePermissionRequest(sessionID string, request copilot.PermissionRequest) (copilot.PermissionRequestResult, error) {
	s := p.getSession(sessionID)
	if s == nil {
		return copilot.PermissionRequestResult{Kind: copilot.PermissionRequestResultKindUserNotAvailable}, nil
	}

	details := permissionDetails(request)

	s.mu.Lock()
	sink := s.sink
	active := s.active
	s.mu.Unlock()

	if !active || sink == nil {
		return copilot.PermissionRequestResult{Kind: copilot.PermissionRequestResultKindUserNotAvailable}, nil
	}

	permID := uuid.NewString()
	payload := make(map[string]any, len(details)+2)
	payload["request_id"] = permID
	payload["tool"] = permissionTool(request)
	for k, v := range details {
		payload[k] = v
	}
	if sendErr := sink.Send(adapterEvent("permission.request", payload)); sendErr != nil {
		return copilot.PermissionRequestResult{Kind: copilot.PermissionRequestResultKindUserNotAvailable}, sendErr
	}

	// WS03 auto-approve stub: allow every permission request at the Copilot SDK
	// level so tool calls proceed normally. The host records the permission.request
	// event in the run log. WS16 adds an interactive bidi grant/deny back-channel.
	return copilot.PermissionRequestResult{Kind: copilot.PermissionRequestResultKindApproved}, nil
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

// permissionTool returns the host policy tool name for a permission request.
// Tool-specific names win when the SDK exposes them; otherwise we fall back to
// the canonical permission kind such as "read", "write", or "shell".
func permissionTool(request copilot.PermissionRequest) string {
	switch req := request.(type) {
	case copilot.PermissionRequestCustomTool:
		if req.ToolName != "" {
			return req.ToolName
		}
	case copilot.PermissionRequestHook:
		if req.ToolName != "" {
			return req.ToolName
		}
	case copilot.PermissionRequestMcp:
		if req.ToolName != "" {
			return req.ToolName
		}
	}
	return string(request.Kind())
}

// includeSensitivePermissionDetails controls whether rich permission payload
// fields (full command, paths/URLs, args, raw request JSON) are forwarded.
// Default is redacted to reduce sensitive data retention risk.
func includeSensitivePermissionDetails() bool {
	return os.Getenv(includeSensitivePermissionDetailsEnv) == "1"
}
