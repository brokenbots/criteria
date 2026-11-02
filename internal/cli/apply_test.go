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
	"github.com/brokenbots/criteria/workflow/workflowtest"
)

func TestApplyLocal_NoopAdapter_EmitsExpectedEvents(t *testing.T) {
	adapterBin := buildNoopAdapterBinary(t)
	adapterDir := t.TempDir()
	pluginPath := filepath.Join(adapterDir, "criteria-adapter-noop")
	b, err := os.ReadFile(adapterBin)
	if err != nil {
		t.Fatalf("read adapter binary: %v", err)
	}
	if err := os.WriteFile(pluginPath, b, 0o755); err != nil {
		t.Fatalf("write adapter binary: %v", err)
	}

	t.Setenv("CRITERIA_ADAPTERS", adapterDir)
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())

	falseVal := false
	workflowPath := writeWorkflowFile(t, workflowtest.New().
		WithHeader("local_apply_noop", "0.1", "run_adapter", "done").
		WithAdapter("noop", "demo").
		WithStep("run_adapter",
			workflowtest.Target("adapter.noop.demo"),
			workflowtest.StepRaw(`  input {
    prompt = "hello"
  }`),
			workflowtest.Outcome("success", "step.done"),
			workflowtest.Outcome("failure", "step.failed"),
		).
		WithState("done", true, nil).
		WithState("failed", true, &falseVal).
		Build())

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

	wantOrder := []string{"StepEntered", "StepOutcome", "StepTransition"}
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
	resumeLocalInFlightRuns(context.Background(), log, &buf, outputModeJSON, "", nil, localApprovalConfig{})
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
	resumeLocalInFlightRuns(context.Background(), log, &buf, outputModeJSON, "", nil, localApprovalConfig{})
	// Server checkpoint must not produce any ND-JSON output.
	if buf.Len() != 0 {
		t.Fatalf("expected no output for server checkpoint, got %q", buf.String())
	}
}

func TestWriteRunCheckpoint_Success(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", dir)

	wfFile := writeWorkflowFile(t, workflowtest.New().WithHeader("w", "0.1", "", "").Build())
	log := newApplyLogger()

	// Must not panic; checkpoint written to stateDir.
	writeRunCheckpoint(log, "run-1", "my-workflow", wfFile, "", "", "build", 1, "cid-1", "tok-1", nil)

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
	wfFile := writeWorkflowFile(t, workflowtest.New().
		WithHeader("w", "0.1", "done", "done").
		WithState("done", true, nil).
		Build())
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
	resumeInFlightRuns(context.Background(), log, &servertrans.Options{}, nil, "")
}

func TestRunApplyLocal_InvalidOutputMode_ReturnsError(t *testing.T) {
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())
	wfFile := writeWorkflowFile(t, workflowtest.New().WithHeader("w", "0.1", "", "").Build())
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

// TestApplyLocal_CHCLWorkflowFile verifies that a .chcl workflow file is
// accepted end-to-end through the local apply pipeline.
func TestApplyLocal_CHCLWorkflowFile(t *testing.T) {
	adapterBin := buildNoopAdapterBinary(t)
	adapterDir := t.TempDir()
	pluginPath := filepath.Join(adapterDir, "criteria-adapter-noop")
	b, err := os.ReadFile(adapterBin)
	if err != nil {
		t.Fatalf("read adapter binary: %v", err)
	}
	if err := os.WriteFile(pluginPath, b, 0o755); err != nil {
		t.Fatalf("write adapter binary: %v", err)
	}

	t.Setenv("CRITERIA_ADAPTERS", adapterDir)
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())

	dir := t.TempDir()
	workflowPath := filepath.Join(dir, "workflow.chcl")
	falseVal := false
	if err := os.WriteFile(workflowPath, []byte(workflowtest.New().
		WithHeader("chcl_wf", "0.1", "run_adapter", "done").
		WithAdapter("noop", "demo").
		WithStep("run_adapter",
			workflowtest.Target("adapter.noop.demo"),
			workflowtest.StepRaw(`  input {
    prompt = "hello"
  }`),
			workflowtest.Outcome("success", "step.done"),
			workflowtest.Outcome("failure", "step.failed"),
		).
		WithState("done", true, nil).
		WithState("failed", true, &falseVal).
		Build()), 0o600); err != nil {
		t.Fatalf("write workflow.chcl: %v", err)
	}

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
}

// TestApplyLocal_VarFileOverridesVariable proves that --var-file values flow
// through to the engine and affect workflow execution. A variable with a
// default is overridden by a .chcl var-file; the overridden value appears in
// the run output event.
func TestApplyLocal_VarFileOverridesVariable(t *testing.T) {
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())

	workflowPath := writeWorkflowFile(t, workflowtest.New().
		WithHeader("var_file_override", "1", "done", "done").
		WithVariable("greeting", "string", `"hello"`).
		WithRaw(`output "greeting" {
  value = var.greeting
}`).
		WithState("done", true, nil).
		Build())

	varFilePath := filepath.Join(t.TempDir(), "vars.chcl")
	if err := os.WriteFile(varFilePath, []byte(`greeting = "world"`+"\n"), 0o600); err != nil {
		t.Fatalf("write var-file: %v", err)
	}

	eventsFile := filepath.Join(t.TempDir(), "events.ndjson")
	if err := runApply(context.Background(), applyOptions{
		workflowPath: workflowPath,
		eventsPath:   eventsFile,
		varFiles:     []string{varFilePath},
	}); err != nil {
		t.Fatalf("runApply failed: %v", err)
	}

	events, err := parseNDJSON(eventsFile)
	if err != nil {
		t.Fatalf("parse events: %v", err)
	}

	var greetingValue string
	for _, evt := range events {
		typ, ok := evt["payload_type"].(string)
		if !ok || typ != "run.outputs" {
			continue
		}
		payload, ok := evt["payload"].(map[string]interface{})
		if !ok {
			continue
		}
		outList, ok := payload["outputs"].([]interface{})
		if !ok {
			continue
		}
		for _, o := range outList {
			outMap, ok := o.(map[string]interface{})
			if !ok {
				continue
			}
			if outMap["name"] == "greeting" {
				if v, ok := outMap["value"].(string); ok {
					greetingValue = v
				}
			}
		}
	}

	// renderCtyValue JSON-marshals string values, so "world" becomes "\"world\"".
	if greetingValue != `"world"` {
		t.Fatalf("expected output greeting=%q from var-file override, got %q", `"world"`, greetingValue)
	}
}

// writeSubworkflowParent writes a parent workflow file and a child
// subworkflow directory next to it, then returns the parent file path.
func writeSubworkflowParent(t *testing.T, parent, child string) string {
	t.Helper()
	dir := t.TempDir()
	workflowtest.WriteDir(t, dir, "child", child+"\n")
	return workflowtest.WriteFile(t, dir, "parent.hcl", parent+"\n")
}

// TestApply_SwitchAfterSubworkflowWrite tests that a switch node evaluates its
// match condition against updated data written by a preceding subworkflow step
// (regression for CRI-37).
func TestApply_SwitchAfterSubworkflowWrite(t *testing.T) {
	adapterBin := buildNoopAdapterBinary(t)
	adapterDir := t.TempDir()
	pluginPath := filepath.Join(adapterDir, "criteria-adapter-noop")
	b, err := os.ReadFile(adapterBin)
	if err != nil {
		t.Fatalf("read adapter binary: %v", err)
	}
	if err := os.WriteFile(pluginPath, b, 0o755); err != nil {
		t.Fatalf("write adapter binary: %v", err)
	}

	t.Setenv("CRITERIA_ADAPTERS", adapterDir)
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())

	parentPath := writeSubworkflowParent(t, `
workflow {
  name = "cri37_cli_parent"
  version = "0.1"
  initial_state = "run"
  target_state  = "done"
}

data "internal" "verdict" {
  type  = string
  value = "initial"
}

subworkflow "child" {
  source = "./child"
}

step "run" {
  target = subworkflow.child
  outcome "success" {
    next = switch.route
    write {
      target = data.internal.verdict.value
      value  = subworkflow.verdict
    }
  }
  outcome "failure" {
    next = state.fail
  }
}

switch "route" {
  match {
    condition = data.internal.verdict.value == "reproduced"
    next      = state.done
  }
  default {
    next = state.fail
  }
}

state "done" {
  terminal = true
  success  = true
}
state "fail" {
  terminal = true
  success  = false
}
`, `
workflow {
  name = "cri37_cli_child"
  version = "0.1"
  initial_state = "done"
  target_state  = "done"
}

output "verdict" {
  value = "reproduced"
}

state "done" {
  terminal = true
  success  = true
}
`)

	eventsFile := filepath.Join(t.TempDir(), "events.ndjson")
	if err := runApply(context.Background(), applyOptions{workflowPath: parentPath, eventsPath: eventsFile}); err != nil {
		t.Fatalf("runApply: %v", err)
	}

	raw, err := os.ReadFile(eventsFile)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	lines := splitNDJSONLines(string(raw))

	type runCompletedEvent struct {
		FinalState string `json:"finalState"`
		Success    bool   `json:"success"`
	}

	var runCompleted *runCompletedEvent
	for _, line := range lines {
		var evt ndjsonEnvelope
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			t.Fatalf("unmarshal event: %v", err)
		}
		switch evt.PayloadType {
		case "RunFailed":
			t.Fatalf("run failed: %s", string(evt.Payload))
		case "RunCompleted":
			var rc runCompletedEvent
			if err := json.Unmarshal(evt.Payload, &rc); err != nil {
				t.Fatalf("unmarshal RunCompleted: %v", err)
			}
			runCompleted = &rc
		}
	}

	if runCompleted == nil {
		t.Fatal("no RunCompleted event emitted")
	}
	if runCompleted.FinalState != "done" {
		t.Errorf("finalState = %q, want \"done\"", runCompleted.FinalState)
	}
	if !runCompleted.Success {
		t.Errorf("RunCompleted.success = false, want true; switch routed to failure branch due to stale data")
	}
}
