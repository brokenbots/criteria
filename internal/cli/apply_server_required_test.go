package cli

import (
	"context"
	"strings"
	"testing"
)

func TestApplyLocal_ServerRequiredSignalWait(t *testing.T) {
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())
	t.Setenv("CRITERIA_LOCAL_APPROVAL", "")
	workflowPath := writeWorkflowFile(t, `
workflow {
  name = "requires_signal"
  version = "0.1"
  initial_state = "execute"
  target_state  = "done"
}

adapter "shell" "default" {}

step "execute" {
  target = adapter.shell.default
  input {
    command = "echo hello"
  }
  outcome "success" { next = step.wait_for_signal }
  outcome "failure" { next = step.failed }
}

state "wait_for_signal" {
  requires = "signal"
}

state "done" {
  terminal = true
}

state "failed" {
  terminal = true
  success = false
}
`)

	err := runApply(context.Background(), applyOptions{workflowPath: workflowPath})
	if err == nil {
		t.Fatal("expected error for signal wait in local mode")
	}
	if !strings.Contains(err.Error(), "signal waits require an orchestrator") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestApplyLocal_WaitSignalNode(t *testing.T) {
	// W05: first-class wait { signal } node must be rejected in local mode.
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())
	t.Setenv("CRITERIA_LOCAL_APPROVAL", "")
	workflowPath := writeWorkflowFile(t, `
workflow {
  name = "wait_signal"
  version       = "0.1"
  initial_state = "gate"
  target_state  = "done"
}

wait "gate" {
  signal = "ready"
  outcome "received" { next = step.done }
}

state "done" {
  terminal = true
  success  = true
}
`)

	err := runApply(context.Background(), applyOptions{workflowPath: workflowPath})
	if err == nil {
		t.Fatal("expected error for wait { signal } in local mode")
	}
	if !strings.Contains(err.Error(), "signal waits require an orchestrator") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestApplyLocal_ApprovalNode(t *testing.T) {
	// W05: approval nodes must be rejected in local mode.
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())
	t.Setenv("CRITERIA_LOCAL_APPROVAL", "")
	workflowPath := writeWorkflowFile(t, `
workflow {
  name = "needs_approval"
  version       = "0.1"
  initial_state = "review"
  target_state  = "done"
}

approval "review" {
  approvers = ["alice"]
  reason    = "ship it?"
  outcome "approved" { next = step.done }
  outcome "rejected" { next = step.done }
}

state "done" {
  terminal = true
  success  = true
}
`)

	err := runApply(context.Background(), applyOptions{workflowPath: workflowPath})
	if err == nil {
		t.Fatal("expected error for approval node in local mode")
	}
	if !strings.Contains(err.Error(), "approval nodes require an orchestrator") {
		t.Fatalf("unexpected error: %v", err)
	}
}
