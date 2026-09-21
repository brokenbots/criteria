// CRI-278 regression tests: the once-per-run WorkflowGraphs event.
//
// The event is emitted at the post-compile seam, before the engine starts,
// in both local (ND-JSON events stream) and server (dual-write) mode. The
// local payload is the compile-JSON-shaped layers array; the server payload
// is the pb.WorkflowGraphs message (proto oneof field 37) mirrored into the
// events file.
package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/brokenbots/criteria/internal/cli/applytest"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

const cri278TwoLevelParentHCL = `
workflow {
  name          = "cri278_parent"
  version       = "0.1"
  initial_state = "setup"
  target_state  = "done"
}

adapter "noop" "default" {}

subworkflow "child" {
  source = "./child"
}

step "setup" {
  target = adapter.noop.default
  outcome "success" { next = state.done }
  outcome "failure" { next = state.fail }
}

state "done" {
  terminal = true
  success  = true
}
state "fail" {
  terminal = true
  success  = false
}
`

// child and grand are declared two levels deep but never invoked by a step,
// mirroring the triage two-level fixture: the compiled subworkflow layers
// carry nested bodies while the run executes only the parent's own step.
const cri278ChildHCL = `
workflow {
  name          = "cri278_child"
  version       = "0.1"
  initial_state = "execute"
  target_state  = "done"
}

adapter "noop" "default" {}

subworkflow "grand" {
  source = "./grand"
}

step "execute" {
  target = adapter.noop.default
  outcome "success" { next = state.done }
  outcome "failure" { next = state.done }
}

state "done" {
  terminal = true
  success  = true
}
`

const cri278GrandHCL = `
workflow {
  name          = "cri278_grand"
  version       = "0.1"
  initial_state = "execute"
  target_state  = "done"
}

adapter "noop" "default" {}

step "execute" {
  target = adapter.noop.default
  outcome "success" { next = state.done }
  outcome "failure" { next = state.done }
}

state "done" {
  terminal = true
  success  = true
}
`

// writeCRI278TwoLevelWorkflow writes a two-level subworkflow fixture
// (parent -> child -> grand) and returns the parent file path.
func writeCRI278TwoLevelWorkflow(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "parent.hcl"), []byte(cri278TwoLevelParentHCL), 0o600); err != nil {
		t.Fatalf("write parent: %v", err)
	}
	childDir := filepath.Join(dir, "child")
	if err := os.MkdirAll(filepath.Join(childDir, "grand"), 0o755); err != nil {
		t.Fatalf("create child dirs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(childDir, "main.hcl"), []byte(cri278ChildHCL), 0o644); err != nil {
		t.Fatalf("write child: %v", err)
	}
	if err := os.WriteFile(filepath.Join(childDir, "grand", "main.hcl"), []byte(cri278GrandHCL), 0o644); err != nil {
		t.Fatalf("write grand: %v", err)
	}
	return filepath.Join(dir, "parent.hcl")
}

const cri278SingleHCL = `
workflow {
  name          = "cri278_single"
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
}

adapter "noop" "default" {}

step "run" {
  target = adapter.noop.default
  outcome "success" { next = state.done }
  outcome "failure" { next = state.fail }
}

state "done" {
  terminal = true
  success  = true
}
state "fail" {
  terminal = true
  success  = false
}
`

// runCRI278LocalApply installs the noop adapter, runs a local apply with
// ND-JSON events output, and returns the events stream.
func runCRI278LocalApply(t *testing.T, workflowPath string) string {
	t.Helper()
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

	eventsPath := filepath.Join(t.TempDir(), "events.ndjson")
	if err := runApply(context.Background(), applyOptions{workflowPath: workflowPath, eventsPath: eventsPath}); err != nil {
		t.Fatalf("runApply: %v", err)
	}
	raw, err := os.ReadFile(eventsPath)
	if err != nil {
		t.Fatalf("read events file: %v", err)
	}
	return string(raw)
}

func unmarshalCRI278Envelopes(t *testing.T, ndjson string) []ndjsonEnvelope {
	t.Helper()
	lines := splitNDJSONLines(ndjson)
	if len(lines) == 0 {
		t.Fatal("events stream is empty")
	}
	envs := make([]ndjsonEnvelope, 0, len(lines))
	for _, line := range lines {
		var evt ndjsonEnvelope
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			t.Fatalf("unmarshal event %q: %v", line, err)
		}
		envs = append(envs, evt)
	}
	return envs
}

func cri278EnvelopeKinds(envs []ndjsonEnvelope) []string {
	kinds := make([]string, 0, len(envs))
	for _, env := range envs {
		kinds = append(kinds, env.PayloadType)
	}
	return kinds
}

// canonicalJSON decodes JSON bytes into a generic value so comparisons are
// key-order-independent (the canonical compare method established for
// CRI-278 in run-stable.md).
func canonicalJSON(t *testing.T, b []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("canonical decode: %v", err)
	}
	return v
}

// TestWorkflowGraphs_LocalTwoLevelRegression is the CRI-278 regression gate:
// applying a two-level subworkflow workflow locally with ND-JSON events
// output emits exactly 6 envelopes with WorkflowGraphs at index 0 (before
// RunStarted) carrying payload_type exactly "WorkflowGraphs", and the
// payload's layers array is canonically identical to the subworkflows array
// of compile --format json for the same fixture.
func TestWorkflowGraphs_LocalTwoLevelRegression(t *testing.T) {
	parentPath := writeCRI278TwoLevelWorkflow(t)
	envs := unmarshalCRI278Envelopes(t, runCRI278LocalApply(t, parentPath))

	if len(envs) != 6 {
		t.Fatalf("stream has %d envelopes, want 6: %v", len(envs), cri278EnvelopeKinds(envs))
	}
	first := envs[0]
	if first.PayloadType != "WorkflowGraphs" {
		t.Fatalf("first envelope payload_type = %q, want %q", first.PayloadType, "WorkflowGraphs")
	}
	if envs[1].PayloadType != "RunStarted" {
		t.Fatalf("second envelope payload_type = %q, want RunStarted", envs[1].PayloadType)
	}

	compileJSON, err := compileWorkflowOutput(context.Background(), parentPath, "", "json", nil, false, false)
	if err != nil {
		t.Fatalf("compile --format json: %v", err)
	}
	var compiled struct {
		Subworkflows json.RawMessage `json:"subworkflows"`
	}
	if err := json.Unmarshal(compileJSON, &compiled); err != nil {
		t.Fatalf("unmarshal compile JSON: %v", err)
	}
	if compiled.Subworkflows == nil {
		t.Fatal("compile JSON has no subworkflows array for a two-level fixture")
	}
	want := canonicalJSON(t, compiled.Subworkflows)
	got := canonicalJSON(t, first.Payload)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("WorkflowGraphs payload differs from compile JSON subworkflows\n event: %s\n compile: %s", first.Payload, compiled.Subworkflows)
	}

	var layers []any
	if err := json.Unmarshal(first.Payload, &layers); err != nil {
		t.Fatalf("payload is not a layers array: %v", err)
	}
	if len(layers) != 1 {
		t.Fatalf("payload has %d top-level layers, want 1 (child, with grand nested inline)", len(layers))
	}
}

// TestWorkflowGraphs_LocalNoSubworkflowControl pins the control case: a
// workflow without subworkflows still emits exactly one WorkflowGraphs
// envelope with an empty layers array, on top of the existing 5 envelope
// kinds — and nothing else.
func TestWorkflowGraphs_LocalNoSubworkflowControl(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "single.hcl")
	if err := os.WriteFile(path, []byte(cri278SingleHCL), 0o600); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
	envs := unmarshalCRI278Envelopes(t, runCRI278LocalApply(t, path))

	if len(envs) != 6 {
		t.Fatalf("stream has %d envelopes, want 6: %v", len(envs), cri278EnvelopeKinds(envs))
	}
	if envs[0].PayloadType != "WorkflowGraphs" {
		t.Fatalf("first envelope payload_type = %q, want WorkflowGraphs", envs[0].PayloadType)
	}
	if envs[1].PayloadType != "RunStarted" {
		t.Fatalf("second envelope payload_type = %q, want RunStarted", envs[1].PayloadType)
	}

	var layers []any
	if err := json.Unmarshal(envs[0].Payload, &layers); err != nil {
		t.Fatalf("payload is not a layers array: %v", err)
	}
	if len(layers) != 0 {
		t.Fatalf("control payload has %d layers, want empty array", len(layers))
	}

	kinds := map[string]int{}
	for _, env := range envs {
		kinds[env.PayloadType]++
	}
	for _, want := range []string{"RunStarted", "StepEntered", "StepOutcome", "StepTransition", "RunCompleted"} {
		if kinds[want] != 1 {
			t.Errorf("envelope kind %s appears %d times, want exactly 1", want, kinds[want])
		}
	}
	if kinds["WorkflowGraphs"] != 1 {
		t.Errorf("envelope kind WorkflowGraphs appears %d times, want exactly 1", kinds["WorkflowGraphs"])
	}
}

// TestWorkflowGraphs_ServerModeDualWriteParity extends the dual-write
// harness to the new event: the published server stream carries exactly one
// WorkflowGraphs payload positioned before/at RunStarted, and the events
// file mirror carries the identical payload content.
func TestWorkflowGraphs_ServerModeDualWriteParity(t *testing.T) {
	requireNoGoroutineLeak(t)
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())

	fake := applytest.New(t)
	eventsFile := filepath.Join(t.TempDir(), "events.ndjson")

	if err := runApplyServer(context.Background(), applyOptions{
		workflowPath: writeCRI278TwoLevelWorkflow(t),
		serverURL:    fake.URL(),
		eventsPath:   eventsFile,
		name:         "cri278-server-graphs",
	}); err != nil {
		t.Fatalf("runApplyServer: %v", err)
	}

	serverEvents := fake.Events()
	graphIndexes := []int{}
	runStartedIndex := -1
	for i, env := range serverEvents {
		if env.GetWorkflowGraphs() != nil {
			graphIndexes = append(graphIndexes, i)
		}
		if env.GetRunStarted() != nil && runStartedIndex == -1 {
			runStartedIndex = i
		}
	}
	if len(graphIndexes) != 1 {
		t.Fatalf("server stream has %d WorkflowGraphs envelopes, want exactly 1", len(graphIndexes))
	}
	if runStartedIndex == -1 {
		t.Fatal("server stream has no RunStarted envelope")
	}
	if graphIndexes[0] > runStartedIndex {
		t.Fatalf("WorkflowGraphs at index %d is after RunStarted at index %d", graphIndexes[0], runStartedIndex)
	}
	serverGraphs := serverEvents[graphIndexes[0]].GetWorkflowGraphs()
	if len(serverGraphs.Subworkflows) != 1 {
		t.Fatalf("server payload has %d top-level layers, want 1 (child, with grand nested inline)", len(serverGraphs.Subworkflows))
	}
	if got := serverGraphs.Subworkflows[0].GetName(); got != "child" {
		t.Fatalf("top-level layer name = %q, want %q", got, "child")
	}

	fileEvents := readNDJSONEnvelopes(t, eventsFile)
	assertPayloadParity(t, serverEvents, fileEvents)

	graphsInFile := 0
	for _, env := range fileEvents {
		if env.PayloadType != "WorkflowGraphs" {
			continue
		}
		graphsInFile++
		if !reflect.DeepEqual(canonicalJSON(t, env.Payload), canonicalJSON(t, mustProtojsonMarshal(t, serverGraphs))) {
			t.Errorf("file mirror WorkflowGraphs payload differs from server payload\n file: %s\n server: %s", env.Payload, mustProtojsonMarshal(t, serverGraphs))
		}
	}
	if graphsInFile != 1 {
		t.Fatalf("events file has %d WorkflowGraphs envelopes, want exactly 1", graphsInFile)
	}
}

func mustProtojsonMarshal(t *testing.T, msg *pb.WorkflowGraphs) []byte {
	t.Helper()
	b, err := protojson.Marshal(msg)
	if err != nil {
		t.Fatalf("protojson marshal: %v", err)
	}
	return b
}
