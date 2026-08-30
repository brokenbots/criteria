package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runAndReadEvents executes runApply for the given workflow and returns the
// resulting Go error and the parsed ND-JSON events.
func runAndReadEvents(t *testing.T, opts *applyOptions) (runErr error, events []map[string]interface{}) {
	t.Helper()
	eventsFile := filepath.Join(t.TempDir(), "events.ndjson")
	opts.eventsPath = eventsFile

	runErr = runApply(context.Background(), *opts)

	raw, err := os.ReadFile(eventsFile)
	if err != nil {
		t.Fatalf("read events file: %v", err)
	}
	lines := strings.Split(string(raw), "\n")
	events = make([]map[string]interface{}, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var evt map[string]interface{}
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			t.Fatalf("unmarshal event: %v", err)
		}
		events = append(events, evt)
	}
	return runErr, events
}

// findRunCompleted returns the single RunCompleted event in events, failing the
// test if zero or more than one is present.
func findRunCompleted(t *testing.T, events []map[string]interface{}) map[string]interface{} {
	t.Helper()
	var found map[string]interface{}
	for _, evt := range events {
		if evt["payload_type"] != "RunCompleted" {
			continue
		}
		if found != nil {
			t.Fatal("expected exactly one RunCompleted event")
		}
		found = evt
	}
	if found == nil {
		t.Fatal("no RunCompleted event found")
	}
	return found
}

// TestApplyLocal_TerminalFailure_ExitsNonZero verifies that a workflow reaching
// a terminal state with success=false causes runApply to return a non-nil error,
// which translates to OS exit status 1 through the CLI.
func TestApplyLocal_TerminalFailure_ExitsNonZero(t *testing.T) {
	failBin := buildFailAdapterBinary(t)
	adapterDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(adapterDir, "criteria-adapter-fail"), mustReadFile(t, failBin), 0o755); err != nil {
		t.Fatalf("write fail adapter: %v", err)
	}

	t.Setenv("CRITERIA_ADAPTERS", adapterDir)
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())

	workflowPath := writeWorkflowFile(t, `
workflow {
  name = "local_apply_failure"
  version = "0.1"
  initial_state = "run_adapter"
  target_state  = "failed"
}

adapter "fail" "default" {
  config {
    bootstrap = "true"
  }
}

step "run_adapter" {
  target = adapter.fail.default
  input {
    prompt = "hello"
  }
  outcome "success" { next = step.done }
  outcome "failure" { next = step.failed }
}

state "done" {
  terminal = true
  success  = true
}
state "failed" {
  terminal = true
  success  = false
}
`)

	runErr, events := runAndReadEvents(t, &applyOptions{workflowPath: workflowPath})
	if runErr == nil {
		t.Fatal("expected non-nil error for terminal failed run")
	}
	if !strings.Contains(runErr.Error(), "success=false") {
		t.Fatalf("error should report success=false, got: %v", runErr)
	}

	rc := findRunCompleted(t, events)
	payload, ok := rc["payload"].(map[string]interface{})
	if !ok {
		t.Fatal("RunCompleted payload missing")
	}
	if finalState, _ := payload["finalState"].(string); finalState != "failed" {
		t.Fatalf("RunCompleted.finalState = %q, want failed", finalState)
	}
	if success, _ := payload["success"].(bool); success {
		t.Fatal("RunCompleted.success = true, want false")
	}
}

// TestApplyLocal_TerminalSuccess_ExitsZero verifies that a workflow reaching a
// terminal state with success=true continues to return a nil error (exit 0).
func TestApplyLocal_TerminalSuccess_ExitsZero(t *testing.T) {
	noopBin := buildNoopAdapterBinary(t)
	adapterDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(adapterDir, "criteria-adapter-noop"), mustReadFile(t, noopBin), 0o755); err != nil {
		t.Fatalf("write noop adapter: %v", err)
	}

	t.Setenv("CRITERIA_ADAPTERS", adapterDir)
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())

	workflowPath := writeWorkflowFile(t, `
workflow {
  name = "local_apply_success"
  version = "0.1"
  initial_state = "run_adapter"
  target_state  = "done"
}

adapter "noop" "default" {
  config {
    bootstrap = "true"
  }
}

step "run_adapter" {
  target = adapter.noop.default
  input {
    prompt = "hello"
  }
  outcome "success" { next = step.done }
  outcome "failure" { next = step.failed }
}

state "done" {
  terminal = true
  success  = true
}
state "failed" {
  terminal = true
  success  = false
}
`)

	runErr, events := runAndReadEvents(t, &applyOptions{workflowPath: workflowPath})
	if runErr != nil {
		t.Fatalf("expected nil error for terminal successful run, got: %v", runErr)
	}

	rc := findRunCompleted(t, events)
	payload, ok := rc["payload"].(map[string]interface{})
	if !ok {
		t.Fatal("RunCompleted payload missing")
	}
	if finalState, _ := payload["finalState"].(string); finalState != "done" {
		t.Fatalf("RunCompleted.finalState = %q, want done", finalState)
	}
	if success, _ := payload["success"].(bool); !success {
		t.Fatal("RunCompleted.success = false, want true")
	}
}

// TestApplyLocal_InitialTerminalFailure_ExitsNonZero verifies the simplest
// possible failure path: a workflow whose initial state is terminal with
// success=false. No adapter or step is executed.
func TestApplyLocal_InitialTerminalFailure_ExitsNonZero(t *testing.T) {
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())

	workflowPath := writeWorkflowFile(t, `
workflow {
  name = "initial_failure"
  version = "0.1"
  initial_state = "failed"
  target_state  = "failed"
}

state "failed" {
  terminal = true
  success  = false
}
`)

	runErr, events := runAndReadEvents(t, &applyOptions{workflowPath: workflowPath})
	if runErr == nil {
		t.Fatal("expected non-nil error for initial terminal failure")
	}

	rc := findRunCompleted(t, events)
	payload, ok := rc["payload"].(map[string]interface{})
	if !ok {
		t.Fatal("RunCompleted payload missing")
	}
	if finalState, _ := payload["finalState"].(string); finalState != "failed" {
		t.Fatalf("RunCompleted.finalState = %q, want failed", finalState)
	}
	if success, _ := payload["success"].(bool); success {
		t.Fatal("RunCompleted.success = true, want false")
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}
