// copilot_permission_test.go — tests for permissionDetails and the Permit handler.

package main

import (
	"testing"

	copilot "github.com/github/copilot-sdk/go"
)

func TestPermissionDetailsKindField(t *testing.T) {
	req := &copilot.PermissionRequest{Kind: copilot.PermissionRequestKindRead}
	details := permissionDetails(req)
	if details["kind"] != "read" {
		t.Errorf("details[kind] = %q, want %q", details["kind"], "read")
	}
}

func TestPermissionDetailsOptionalFields(t *testing.T) {
	toolCallID := "tc-123"
	intention := "read source file"
	req := &copilot.PermissionRequest{
		Kind:       copilot.PermissionRequestKindRead,
		ToolCallID: &toolCallID,
		Intention:  &intention,
	}
	details := permissionDetails(req)
	if details["tool_call_id"] != toolCallID {
		t.Errorf("details[tool_call_id] = %q, want %q", details["tool_call_id"], toolCallID)
	}
	if details["intention"] != intention {
		t.Errorf("details[intention] = %q, want %q", details["intention"], intention)
	}
}

func TestPermissionDetailsSensitiveFieldsRedactedByDefault(t *testing.T) {
	path := "/etc/passwd"
	cmd := "cat /etc/passwd"
	req := &copilot.PermissionRequest{
		Kind:            copilot.PermissionRequestKindRead,
		Path:            &path,
		FullCommandText: &cmd,
	}
	details := permissionDetails(req)
	if _, ok := details["path"]; ok {
		t.Error("details[path] must be absent when sensitive details not enabled")
	}
	if _, ok := details["full_command_text"]; ok {
		t.Error("details[full_command_text] must be absent when sensitive details not enabled")
	}
}

func TestPermissionDetailsCommandsField(t *testing.T) {
	req := &copilot.PermissionRequest{
		Kind: copilot.PermissionRequestKindShell,
		Commands: []copilot.PermissionRequestCommand{
			{Identifier: "git status"},
		},
	}
	details := permissionDetails(req)
	if details["commands"] != "git status" {
		t.Errorf("details[commands] = %q, want %q", details["commands"], "git status")
	}
}
