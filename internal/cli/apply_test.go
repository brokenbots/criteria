package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	servertrans "github.com/brokenbots/criteria/internal/transport/server"
)

func TestApplyLocal_NoopPlugin_EmitsExpectedEvents(t *testing.T) {
	pluginBin := buildNoopPluginBinary(t)
	pluginDir := t.TempDir()
	pluginPath := filepath.Join(pluginDir, "criteria-adapter-noop")
	b, err := os.ReadFile(pluginBin)
	if err != nil {
		t.Fatalf("read plugin binary: %v", err)
	}
	if err := os.WriteFile(pluginPath, b, 0o755); err != nil {
		t.Fatalf("write plugin binary: %v", err)
	}

	t.Setenv("CRITERIA_PLUGINS", pluginDir)
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())

	workflowPath := writeWorkflowFile(t, `
workflow "local_apply_noop" {
  version = "0.1"
  initial_state = "open_agent"
  target_state  = "done"

  agent "demo" {
    adapter = "noop"
    config {
      bootstrap = "true"
    }
  }

  step "open_agent" {
    agent = "demo"
    lifecycle = "open"
    outcome "success" { transition_to = "run_agent" }
    outcome "failure" { transition_to = "failed" }
  }

  step "run_agent" {
    agent = "demo"
    input {
      prompt = "hello"
    }
    outcome "success" { transition_to = "close_agent" }
    outcome "failure" { transition_to = "failed" }
  }

  step "close_agent" {
    agent = "demo"
    lifecycle = "close"
    outcome "success" { transition_to = "done" }
    outcome "failure" { transition_to = "failed" }
  }

  state "done" {
    terminal = true
    success  = true
  }
  state "failed" {
    terminal = true
    success  = false
  }
}
`)

	eventsFile := filepath.Join(t.TempDir(), "events.ndjson")
	if err := runApply(context.Background(), applyOptions{workflowPath: workflowPath, eventsPath: eventsFile}); err != nil {
		t.Fatalf("runApply local: %v", err)
	}

	types, err := readPayloadTypes(eventsFile)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}

	if countPayloadType(types, "RunStarted") != 1 {
		t.Fatalf("expected exactly one RunStarted, got %d", countPayloadType(types, "RunStarted"))
	}
	if countPayloadType(types, "RunCompleted") != 1 {
		t.Fatalf("expected exactly one RunCompleted, got %d", countPayloadType(types, "RunCompleted"))
	}

	wantOrder := []string{"StepEntered", "StepOutcome", "StepTransition", "StepEntered", "StepOutcome", "StepTransition", "StepEntered", "StepOutcome", "StepTransition"}
	gotStepEvents := filterPayloadTypes(types, map[string]bool{"StepEntered": true, "StepOutcome": true, "StepTransition": true})
	if strings.Join(gotStepEvents, ",") != strings.Join(wantOrder, ",") {
		t.Fatalf("step event order mismatch\nwant: %v\ngot:  %v", wantOrder, gotStepEvents)
	}
}

type ndjsonEvent struct {
	PayloadType string `json:"payload_type"`
}

func readPayloadTypes(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var evt ndjsonEvent
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			return nil, err
		}
		out = append(out, evt.PayloadType)
	}
	return out, nil
}

func countPayloadType(types []string, target string) int {
	n := 0
	for _, typ := range types {
		if typ == target {
			n++
		}
	}
	return n
}

func filterPayloadTypes(types []string, allowed map[string]bool) []string {
	out := make([]string, 0, len(types))
	for _, typ := range types {
		if allowed[typ] {
			out = append(out, typ)
		}
	}
	return out
}

func TestRunApply_EmptyWorkflowPath(t *testing.T) {
	err := runApply(context.Background(), applyOptions{workflowPath: ""})
	if err == nil {
		t.Fatal("expected error for empty workflow path")
	}
}

func TestResumeLocalInFlightRuns_EmptyCheckpoints(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", stateDir)

	var buf bytes.Buffer
	log := newApplyLogger()
	// Must not panic or fail with no checkpoints.
	resumeLocalInFlightRuns(context.Background(), log, &buf, outputModeJSON)
	if buf.Len() != 0 {
		t.Fatalf("expected no output with empty checkpoints, got %q", buf.String())
	}
}

func TestResumeLocalInFlightRuns_SkipsServerCheckpoints(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", stateDir)

	// Write a checkpoint with a ServerURL — must be skipped (no RunFailed emitted).
	cp := &StepCheckpoint{
		RunID:       "server-run",
		CurrentStep: "build",
		ServerURL:   "http://localhost:8080",
	}
	writeCheckpointDirect(t, stateDir, cp)

	var buf bytes.Buffer
	log := newApplyLogger()
	resumeLocalInFlightRuns(context.Background(), log, &buf, outputModeJSON)
	// Server checkpoint must not produce any ND-JSON output.
	if buf.Len() != 0 {
		t.Fatalf("expected no output for server checkpoint, got %q", buf.String())
	}
}

func TestWriteRunCheckpoint_Success(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", dir)

	wfFile := writeWorkflowFile(t, `workflow "w" { version = "0.1" }`)
	log := newApplyLogger()

	// Must not panic; checkpoint written to stateDir.
	writeRunCheckpoint(log, "run-1", "my-workflow", wfFile, "", "build", 1, "cid-1", "tok-1")

	checkpoints, err := ListStepCheckpoints()
	if err != nil {
		t.Fatalf("ListStepCheckpoints: %v", err)
	}
	if len(checkpoints) != 1 {
		t.Fatalf("expected 1 checkpoint, got %d", len(checkpoints))
	}
	cp := checkpoints[0]
	if cp.RunID != "run-1" || cp.CurrentStep != "build" || cp.CriteriaID != "cid-1" {
		t.Fatalf("checkpoint fields wrong: %+v", cp)
	}
}

func TestRunApply_InvalidWorkflow_ReturnsError(t *testing.T) {
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())
	wfFile := writeWorkflowFile(t, `this is not HCL at all {{{`)
	err := runApply(context.Background(), applyOptions{workflowPath: wfFile})
	if err == nil {
		t.Fatal("expected error for invalid HCL")
	}
}

func TestRunApply_BadEventsFile_ReturnsError(t *testing.T) {
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())
	wfFile := writeWorkflowFile(t, `workflow "w" {
  version = "0.1"
  initial_state = "done"
  target_state  = "done"
  state "done" {
    terminal = true
    success  = true
  }
}`)
	err := runApply(context.Background(), applyOptions{
		workflowPath: wfFile,
		eventsPath:   "/nonexistent-dir/deeply/nested/events.ndjson",
	})
	if err == nil {
		t.Fatal("expected error for unwritable events file")
	}
}

func TestResumeInFlightRuns_ServerFn_EmptyCheckpoints(t *testing.T) {
	// The server-side resumeInFlightRuns (reattach.go) must be a no-op when there
	// are no checkpoints in the state dir.
	dir := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", dir)
	log := newApplyLogger()
	// Must not panic.
	resumeInFlightRuns(context.Background(), log, servertrans.Options{})
}

func TestRunApplyLocal_InvalidOutputMode_ReturnsError(t *testing.T) {
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())
	wfFile := writeWorkflowFile(t, `workflow "w" { version = "0.1" }`)
	err := runApply(context.Background(), applyOptions{
		workflowPath: wfFile,
		output:       "verbose", // invalid → resolveOutputMode error
	})
	if err == nil {
		t.Fatal("expected error for invalid output mode")
	}
	if !strings.Contains(err.Error(), "invalid --output") {
		t.Fatalf("unexpected error: %v", err)
	}
}
