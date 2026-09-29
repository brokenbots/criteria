package cli

import (
	"context"
	"strings"
	"testing"
	"time"
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

adapter "noop" "default" {}

step "execute" {
  target = adapter.noop.default
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
	if !strings.Contains(err.Error(), "signal waits are resolved via the run's local control listener") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestApplyLocal_WaitSignalNode(t *testing.T) {
	// Since CRI-255 a first-class wait { signal } node pauses the local run
	// and is resolved over the run's control listener (ResolveResume), not
	// rejected. Clear CRITERIA_LOCAL_APPROVAL so the global env value does
	// not auto-approve the signal wait and cause the test to pass
	// spuriously.
	t.Setenv("CRITERIA_LOCAL_APPROVAL", "")
	stateDir := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", stateDir)
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

	errCh := runApplyAsync(applyOptions{workflowPath: workflowPath})
	addr := waitForControlEndpoint(t, stateDir, 15*time.Second)
	runID := singleRunID(t, stateDir)
	accepted, reason := resolveApproval(t, addr, runID, "ready", map[string]string{"outcome": "received"})
	if !accepted || reason != "ok" {
		t.Fatalf("ResolveResume = (accepted=%t, reason=%q), want accepted ok", accepted, reason)
	}
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("expected successful run after RPC signal, got: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("runApply did not return after RPC signal")
	}
}

func TestApplyLocal_ApprovalNode(t *testing.T) {
	// Since CRI-255 an approval node pauses the local run and is resolved
	// over the run's control listener (ResolveResume), not rejected. Clear
	// CRITERIA_LOCAL_APPROVAL so the global env value does not auto-approve
	// the approval node and cause the test to pass spuriously.
	t.Setenv("CRITERIA_LOCAL_APPROVAL", "")
	stateDir := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", stateDir)
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

	errCh := runApplyAsync(applyOptions{workflowPath: workflowPath})
	addr := waitForControlEndpoint(t, stateDir, 15*time.Second)
	runID := singleRunID(t, stateDir)
	accepted, reason := resolveApproval(t, addr, runID, "review", map[string]string{"decision": "approved"})
	if !accepted || reason != "ok" {
		t.Fatalf("ResolveResume = (accepted=%t, reason=%q), want accepted ok", accepted, reason)
	}
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("expected successful run after RPC approval, got: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("runApply did not return after RPC approval")
	}
}
