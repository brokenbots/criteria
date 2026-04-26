package cli

import (
	"context"
	"strings"
	"testing"
)

func TestApplyLocal_CastleRequiredSignalWait(t *testing.T) {
	t.Setenv("OVERSEER_STATE_DIR", t.TempDir())
	workflowPath := writeWorkflowFile(t, `
workflow "requires_signal" {
  version = "0.1"
  initial_state = "execute"
  target_state  = "done"

  step "execute" {
    adapter = "shell"
    input {
      command = "echo hello"
    }
    outcome "success" { transition_to = "wait_for_signal" }
    outcome "failure" { transition_to = "failed" }
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
}
`)

	err := runApply(context.Background(), applyOptions{workflowPath: workflowPath})
	if err == nil {
		t.Fatal("expected error for signal wait in local mode")
	}
	if !strings.Contains(err.Error(), "signal waits require --castle <url>") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestApplyLocal_CastleRequiredWaitLifecycle(t *testing.T) {
	// TODO(W05): lifecycle="wait" is not yet a valid HCL value; the compiler
	// rejects it before ensureLocalModeSupported is reached.  Expand this test
	// once W05 adds wait/approval lifecycle support to the workflow schema.
	t.Skip("requires W05 lifecycle=wait compiler support")
	workflowPath := writeWorkflowFile(t, `
workflow "requires_wait_lifecycle" {
  version = "0.1"
  initial_state = "pause"
  target_state  = "done"

  step "pause" {
    adapter   = "shell"
    lifecycle = "wait"
    input {
      command = "echo waiting"
    }
    outcome "success" { transition_to = "done" }
    outcome "failure" { transition_to = "failed" }
  }

  state "done" {
    terminal = true
  }

  state "failed" {
    terminal = true
    success = false
  }
}
`)

	err := runApply(context.Background(), applyOptions{workflowPath: workflowPath})
	if err == nil {
		t.Fatal("expected error for wait lifecycle step in local mode")
	}
	if !strings.Contains(err.Error(), "signal waits require --castle <url>") {
		t.Fatalf("unexpected error: %v", err)
	}
}
