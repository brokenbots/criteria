package cli

// CRI-125 regression tests: a runner pod restart must not silently convert a
// failed run into a fresh run that overwrites prior state. These tests pin the
// three engine-side obligations:
//
//  1. the events file is append-only across runs (never truncated by a new
//     run_id);
//  2. a restarted invocation whose identity matches an in-flight checkpoint
//     resumes the original run (no second live run / zombie);
//  3. the identity fingerprint is deterministic and does not match other
//     invocations, so unrelated work never suppress a fresh run.
//
// The noop adapter binary is built once and shared by the adapter tests; each
// test uses its own CRITERIA_STATE_DIR (and, where relevant, its own events
// file) so tests stay isolated.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/brokenbots/criteria/internal/cli/applytest"
	servertrans "github.com/brokenbots/criteria/internal/transport/server"
)

// cri125NoopWorkflow is a minimal single-step noop-adapter workflow.
const cri125NoopWorkflow = `
workflow {
  name          = "cri125_noop"
  version       = "0.1"
  initial_state = "run_adapter"
  target_state  = "done"
}

adapter "noop" "demo" {
  config {
    bootstrap = "true"
  }
}

step "run_adapter" {
  target = adapter.noop.demo
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
`

// cri125VarWorkflow is like cri125NoopWorkflow but also declares a variable
// with a default and an output, so a resumed run's applied var overrides are
// observable in the run.outputs event.
const cri125VarWorkflow = `
workflow {
  name          = "cri125_vars"
  version       = "0.1"
  initial_state = "run_adapter"
  target_state  = "done"
}

variable "greeting" {
  type    = string
  default = "hello"
}

output "greeting" {
  value = var.greeting
}

adapter "noop" "demo" {
  config {
    bootstrap = "true"
  }
}

step "run_adapter" {
  target = adapter.noop.demo
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
`

func TestRunIdentityFingerprint(t *testing.T) {
	wf := writeWorkflowFile(t, cri125NoopWorkflow)
	varFile := filepath.Join(t.TempDir(), "vars.chcl")
	if err := os.WriteFile(varFile, []byte("greeting = \"world\"\n"), 0o600); err != nil {
		t.Fatalf("write var file: %v", err)
	}

	base := runIdentityFingerprint(wf, "", nil, nil)
	if base == "" {
		t.Fatal("fingerprint must not be empty for a valid workflow path")
	}

	// Same inputs → same digest.
	if again := runIdentityFingerprint(wf, "", nil, nil); again != base {
		t.Fatalf("fingerprint not deterministic: %q vs %q", base, again)
	}
	// Relative vs absolute path resolves to the same digest.
	rel, err := filepath.Rel(cwd(t), wf)
	if err != nil {
		t.Fatalf("rel: %v", err)
	}
	if rel == wf {
		t.Skip("workflow file already addressed relatively")
	}
	if got := runIdentityFingerprint(rel, "", nil, nil); got != base {
		t.Fatalf("relative and absolute paths must produce the same fingerprint: %q vs %q", got, base)
	}
	// Different server URL → different digest.
	if got := runIdentityFingerprint(wf, "https://srv.example", nil, nil); got == base {
		t.Fatal("different server URL must produce a different fingerprint")
	}
	// Different var-file content → different digest.
	otherVar := filepath.Join(t.TempDir(), "vars.chcl")
	if err := os.WriteFile(otherVar, []byte("greeting = \"other\"\n"), 0o600); err != nil {
		t.Fatalf("write var file: %v", err)
	}
	if got := runIdentityFingerprint(wf, "", []string{varFile}, nil); got == base {
		t.Fatal("var-file inputs must affect the fingerprint")
	}
	if got := runIdentityFingerprint(wf, "", []string{otherVar}, nil); got == runIdentityFingerprint(wf, "", []string{varFile}, nil) {
		t.Fatal("different var-file content must produce different fingerprints")
	}
	// Unreadable var file → empty (never-match) fingerprint.
	if got := runIdentityFingerprint(wf, "", []string{filepath.Join(t.TempDir(), "missing.chcl")}, nil); got != "" {
		t.Fatalf("unreadable var file must yield an empty fingerprint, got %q", got)
	}
	// Empty workflow path → empty (never-match) fingerprint.
	if got := runIdentityFingerprint("", "", nil, nil); got != "" {
		t.Fatalf("empty workflow path must yield an empty fingerprint, got %q", got)
	}
}

func TestCRI125_EventsFileAppendOnlyAcrossRuns(t *testing.T) {
	cri125SetupAdapter(t)
	stateDir := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", stateDir)

	wf1 := writeWorkflowFile(t, cri125NoopWorkflow)
	wfDir := t.TempDir()
	wf2 := filepath.Join(wfDir, "workflow.hcl")
	if err := os.WriteFile(wf2, []byte(cri125NoopWorkflow), 0o600); err != nil {
		t.Fatalf("write second workflow: %v", err)
	}

	eventsFile := filepath.Join(t.TempDir(), "events.ndjson")

	if err := runApply(context.Background(), applyOptions{workflowPath: wf1, eventsPath: eventsFile}); err != nil {
		t.Fatalf("first runApply: %v", err)
	}
	if err := runApply(context.Background(), applyOptions{workflowPath: wf2, eventsPath: eventsFile}); err != nil {
		t.Fatalf("second runApply: %v", err)
	}

	events, err := parseNDJSON(eventsFile)
	if err != nil {
		t.Fatalf("parse events: %v", err)
	}
	started, completed := cri125StartedAndCompleted(events)
	if started != 2 {
		t.Fatalf("expected 2 RunStarted events in the shared events file, got %d", started)
	}
	if completed != 2 {
		t.Fatalf("expected 2 RunCompleted events in the shared events file, got %d", completed)
	}
	if runIDs := cri125DistinctRunIDs(events); len(runIDs) != 2 {
		t.Fatalf("expected two distinct run_ids in the shared events file, got %v", runIDs)
	}
}

func TestCRI125_LocalRestartResumesOriginalRun(t *testing.T) {
	cri125SetupAdapter(t)
	stateDir := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", stateDir)

	wfPath := writeWorkflowFile(t, cri125NoopWorkflow)
	eventsFile := filepath.Join(t.TempDir(), "events.ndjson")
	fp := runIdentityFingerprint(wfPath, "", nil, nil)

	// A checkpoint from a crashed run of the same invocation identity.
	writeCheckpointDirect(t, stateDir, &StepCheckpoint{
		RunID:        "cri125-orig",
		Workflow:     "cri125_noop",
		WorkflowPath: wfPath,
		CurrentStep:  "run_adapter",
		Attempt:      0,
		StartedAt:    time.Now().UTC(),
		Fingerprint:  fp,
	})

	if err := runApply(context.Background(), applyOptions{workflowPath: wfPath, eventsPath: eventsFile}); err != nil {
		t.Fatalf("runApply after restart: %v", err)
	}

	events, err := parseNDJSON(eventsFile)
	if err != nil {
		t.Fatalf("parse events: %v", err)
	}
	started, completed := cri125StartedAndCompleted(events)
	if started != 0 {
		// RunFrom (the resume entrypoint) does not emit RunStarted; any
		// RunStarted here would mean a fresh second run was forked.
		t.Fatalf("restart must not fork a second run: got %d RunStarted events", started)
	}
	if completed != 1 {
		t.Fatalf("expected exactly 1 RunCompleted for the resumed run, got %d", completed)
	}
	if ids := cri125DistinctRunIDs(events); len(ids) != 1 || ids[0] != "cri125-orig" {
		t.Fatalf("all events must carry the original run_id, got %v", ids)
	}
	hasResumed := false
	for _, evt := range events {
		if evt["payload_type"] == "StepResumed" {
			hasResumed = true
			break
		}
	}
	if !hasResumed {
		t.Fatalf("expected a StepResumed event in: %v", events)
	}
	checkpoints, err := ListStepCheckpoints()
	if err != nil {
		t.Fatalf("list checkpoints: %v", err)
	}
	if len(checkpoints) != 0 {
		t.Fatalf("checkpoint must be consumed after resume, got %v", checkpoints)
	}
}

func TestCRI125_DifferentFingerprintDoesNotSuppress(t *testing.T) {
	cri125SetupAdapter(t)
	stateDir := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", stateDir)

	wfPath := writeWorkflowFile(t, cri125NoopWorkflow)
	eventsFile := filepath.Join(t.TempDir(), "events.ndjson")

	// A checkpoint from a different invocation identity (e.g. a different
	// ticket sharing the same runner state dir).
	writeCheckpointDirect(t, stateDir, &StepCheckpoint{
		RunID:        "cri125-other",
		Workflow:     "cri125_noop",
		WorkflowPath: wfPath,
		CurrentStep:  "run_adapter",
		Attempt:      0,
		StartedAt:    time.Now().UTC(),
		Fingerprint:  "some-other-invocation",
	})

	if err := runApply(context.Background(), applyOptions{workflowPath: wfPath, eventsPath: eventsFile}); err != nil {
		t.Fatalf("runApply: %v", err)
	}

	events, err := parseNDJSON(eventsFile)
	if err != nil {
		t.Fatalf("parse events: %v", err)
	}
	started, completed := cri125StartedAndCompleted(events)
	if started != 1 {
		t.Fatalf("expected exactly 1 fresh RunStarted, got %d", started)
	}
	if completed != 2 {
		// One for the resumed (unmatched) in-flight run and one for the fresh
		// run that was correctly allowed to proceed.
		t.Fatalf("expected 2 RunCompleted (resumed + fresh), got %d", completed)
	}
	if ids := cri125DistinctRunIDs(events); len(ids) != 2 {
		t.Fatalf("expected both run_ids in the events file, got %v", ids)
	}
	freshSeen := false
	for _, evt := range events {
		if evt["payload_type"] == "RunStarted" && evt["run_id"] != "cri125-other" {
			freshSeen = true
		}
	}
	if !freshSeen {
		t.Fatal("a fingerprint-mismatched invocation must start its own fresh run")
	}
}

func TestCRI125_UnresumableCheckpointLetsFreshRunProceed(t *testing.T) {
	cri125SetupAdapter(t)
	stateDir := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", stateDir)

	wfPath := writeWorkflowFile(t, cri125NoopWorkflow)
	eventsFile := filepath.Join(t.TempDir(), "events.ndjson")
	fp := runIdentityFingerprint(wfPath, "", nil, nil)

	// Fingerprint matches, but the recorded workflow file is gone (e.g. the
	// tmpfs it lived on vanished with the pod): the checkpoint is unusable and
	// the fresh run must proceed instead of stalling forever.
	writeCheckpointDirect(t, stateDir, &StepCheckpoint{
		RunID:        "cri125-lost",
		Workflow:     "cri125_noop",
		WorkflowPath: filepath.Join(stateDir, "gone", "workflow.hcl"),
		CurrentStep:  "run_adapter",
		Attempt:      0,
		StartedAt:    time.Now().UTC(),
		Fingerprint:  fp,
	})

	if err := runApply(context.Background(), applyOptions{workflowPath: wfPath, eventsPath: eventsFile}); err != nil {
		t.Fatalf("runApply: %v", err)
	}

	events, err := parseNDJSON(eventsFile)
	if err != nil {
		t.Fatalf("parse events: %v", err)
	}
	started, _ := cri125StartedAndCompleted(events)
	if started != 1 {
		t.Fatalf("expected the fresh run to proceed with 1 RunStarted, got %d", started)
	}
	for _, evt := range events {
		if evt["run_id"] == "cri125-lost" {
			t.Fatalf("abandoned checkpoint's run must not execute, saw: %v", events)
		}
	}
	checkpoints, err := ListStepCheckpoints()
	if err != nil {
		t.Fatalf("list checkpoints: %v", err)
	}
	if len(checkpoints) != 0 {
		t.Fatalf("unresumable checkpoint must be abandoned, got %v", checkpoints)
	}
}

func TestCRI125_ResumedRunAppliesVarOverrides(t *testing.T) {
	cri125SetupAdapter(t)
	stateDir := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", stateDir)

	wfPath := writeWorkflowFile(t, cri125VarWorkflow)
	eventsFile := filepath.Join(t.TempDir(), "events.ndjson")
	varFile := filepath.Join(t.TempDir(), "vars.chcl")
	if err := os.WriteFile(varFile, []byte("greeting = \"world\"\n"), 0o600); err != nil {
		t.Fatalf("write var file: %v", err)
	}
	fp := runIdentityFingerprint(wfPath, "", []string{varFile}, nil)

	// The checkpoint stores no variable scope, so the resumed engine must
	// receive the restarted invocation's CLI variable inputs. The restarted
	// runner replays the same CLI arguments (--var-file), so drive the full
	// runApply path.
	writeCheckpointDirect(t, stateDir, &StepCheckpoint{
		RunID:        "cri125-vars",
		Workflow:     "cri125_vars",
		WorkflowPath: wfPath,
		CurrentStep:  "run_adapter",
		Attempt:      0,
		StartedAt:    time.Now().UTC(),
		Fingerprint:  fp,
	})

	if err := runApply(context.Background(), applyOptions{
		workflowPath: wfPath,
		eventsPath:   eventsFile,
		varFiles:     []string{varFile},
	}); err != nil {
		t.Fatalf("runApply after restart: %v", err)
	}

	events, err := parseNDJSON(eventsFile)
	if err != nil {
		t.Fatalf("parse events: %v", err)
	}
	var greeting string
	for _, evt := range events {
		if evt["payload_type"] != "run.outputs" {
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
					greeting = v
				}
			}
		}
	}
	if greeting != `"world"` {
		t.Fatalf("resumed run must apply --var-file overrides, got greeting=%q", greeting)
	}
	if _, completed := cri125StartedAndCompleted(events); completed != 1 {
		t.Fatalf("expected 1 RunCompleted for the resumed run, got %d", completed)
	}
}

func TestCRI125_ServerRestartResumesOriginalRun(t *testing.T) {
	requireNoGoroutineLeak(t)
	cri125SetupAdapter(t)
	stateDir := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", stateDir)

	fake := applytest.New(t)
	wfPath := writeWorkflowFile(t, cri125NoopWorkflow)
	fp := runIdentityFingerprint(wfPath, fake.URL(), nil, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	log := discardLogger()
	src, graph, loader, err := compileForExecution(ctx, wfPath, log, false, false)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	defer func() { _ = loader.Shutdown(context.WithoutCancel(ctx)) }()

	copts := servertrans.Options{TLSMode: servertrans.TLSDisable}
	// First invocation: fresh run (no checkpoints yet), which registers a
	// client and creates the run.
	client, runID, resumed, err := setupServerRun(ctx, log, graph, src, fake.URL(), "cri125-server", &copts, cancel, nil, fp)
	if err != nil {
		t.Fatalf("first setupServerRun: %v", err)
	}
	if resumed {
		t.Fatal("first setupServerRun unexpectedly resumed")
	}
	client.Close()

	// Simulate a runner restart: the server still reports the run as
	// in-flight, and the checkpoint written by the crashed runner is found
	// in the (persistent) state dir.
	fake.SetReattachState(runID, "running", "run_adapter", 0, "", "")
	writeRunCheckpoint(log, runID, graph.Name, wfPath, fake.URL(), fp, "run_adapter", 0, client.CriteriaID(), client.Token(), nil)

	client2, runID2, resumed2, err := setupServerRun(ctx, log, graph, src, fake.URL(), "cri125-server-restart", &copts, cancel, nil, fp)
	if err != nil {
		t.Fatalf("second setupServerRun: %v", err)
	}
	defer client2.Close()
	if !resumed2 {
		t.Fatal("matching checkpoint must be resumed on restart")
	}
	if runID2 != "" {
		t.Fatalf("a resumed invocation must not create a second run, got run_id %q", runID2)
	}
	if fake.CreatedRunCount() != 1 {
		t.Fatalf("CreateRun must be called exactly once across the restart, got %d", fake.CreatedRunCount())
	}
}

func TestCRI125_ServerRestartDifferentFingerprintProceeds(t *testing.T) {
	requireNoGoroutineLeak(t)
	cri125SetupAdapter(t)
	stateDir := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", stateDir)

	fake := applytest.New(t)
	wfPath := writeWorkflowFile(t, cri125NoopWorkflow)
	fp := runIdentityFingerprint(wfPath, fake.URL(), nil, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	log := discardLogger()
	src, graph, loader, err := compileForExecution(ctx, wfPath, log, false, false)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	defer func() { _ = loader.Shutdown(context.WithoutCancel(ctx)) }()

	copts := servertrans.Options{TLSMode: servertrans.TLSDisable}
	client, runID, resumed, err := setupServerRun(ctx, log, graph, src, fake.URL(), "cri125-server", &copts, cancel, nil, fp)
	if err != nil {
		t.Fatalf("first setupServerRun: %v", err)
	}
	if resumed {
		t.Fatal("first setupServerRun unexpectedly resumed")
	}
	client.Close()

	// The checkpoint belongs to a different invocation identity, so the
	// restarted invocation must proceed with a fresh run rather than
	// silently absorbing the unrelated in-flight run.
	fake.SetReattachState(runID, "running", "run_adapter", 0, "", "")
	writeRunCheckpoint(log, runID, graph.Name, wfPath, fake.URL(), "unrelated-fingerprint", "run_adapter", 0, client.CriteriaID(), client.Token(), nil)

	client2, runID2, resumed2, err := setupServerRun(ctx, log, graph, src, fake.URL(), "cri125-server-fresh", &copts, cancel, nil, fp)
	if err != nil {
		t.Fatalf("second setupServerRun: %v", err)
	}
	defer client2.Close()
	if resumed2 {
		t.Fatal("an unmatched checkpoint must not suppress a fresh run")
	}
	if runID2 == "" || runID2 == runID {
		t.Fatalf("expected a fresh run_id, got %q (original %q)", runID2, runID)
	}
}

// cri125SetupAdapter installs the noop adapter binary into a fresh adapter
// directory and points CRITERIA_ADAPTERS at it.
func cri125SetupAdapter(t *testing.T) {
	t.Helper()
	adapterBin := buildNoopAdapterBinary(t)
	adapterDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(adapterDir, "criteria-adapter-noop"), mustRead(t, adapterBin), 0o755); err != nil {
		t.Fatalf("write adapter binary: %v", err)
	}
	t.Setenv("CRITERIA_ADAPTERS", adapterDir)
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

func cwd(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return dir
}

func cri125StartedAndCompleted(events []map[string]interface{}) (started, completed int) {
	for _, evt := range events {
		switch evt["payload_type"] {
		case "RunStarted":
			started++
		case "RunCompleted":
			completed++
		}
	}
	return started, completed
}

func cri125DistinctRunIDs(events []map[string]interface{}) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(events))
	for _, evt := range events {
		id, _ := evt["run_id"].(string)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}
