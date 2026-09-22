// CRI-278/CRI-298/CRI-299 regression tests: the once-per-run WorkflowGraphs event.
//
// The event is emitted at the post-compile seam, before the engine starts,
// in both local (ND-JSON events stream) and server (dual-write) mode. The
// local payload is the layers array; the server payload is the
// pb.WorkflowGraphs message (proto oneof field 37) mirrored into the events
// file. The payload is the flat layer list (CRI-299): every layer entry —
// at every depth — is a sibling entry in the repeated field and carries the
// uniform {name, sourcePath, body} shape with body a JSON string containing
// ONLY that layer's own compiled graph; no body string contains a
// subworkflows key. The compile-JSON dialect (snake_case source_path, inline
// bodies, nested subworkflows) stays internal to `criteria compile --format
// json`, whose published shape is unchanged.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
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

// assertCRI299FlatLayer asserts one layer entry's wire shape: exactly the
// three keys name, sourcePath and body; body a JSON string (never an inline
// object, never snake_case source_path); parsing the body yields that
// layer's OWN compiled graph only — no subworkflows key, so no body
// contains another layer (CRI-299: nested layers ride the flat entry list,
// not their parent's body). Returns the layer entry so callers can pin
// entry and body content.
func assertCRI299FlatLayer(t *testing.T, layer map[string]any) map[string]any {
	t.Helper()
	keys := slices.Sorted(maps.Keys(layer))
	if want := []string{"body", "name", "sourcePath"}; !reflect.DeepEqual(keys, want) {
		t.Fatalf("layer entry keys = %v, want exactly %v (uniform wire shape)", keys, want)
	}
	name, _ := layer["name"].(string)
	if name == "" {
		t.Fatalf("layer entry has no string name: %v", layer)
	}
	if _, ok := layer["sourcePath"].(string); !ok {
		t.Fatalf("layer %q sourcePath is %T, want a string (snake_case source_path is the CRI-298 producer bug)", name, layer["sourcePath"])
	}
	body, ok := layer["body"].(string)
	if !ok {
		t.Fatalf("layer %q body is %T, want a JSON string", name, layer["body"])
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("layer %q body does not parse as JSON: %v\nbody: %s", name, err, body)
	}
	if _, hasSubs := parsed["subworkflows"]; hasSubs {
		t.Fatalf("layer %q body carries a subworkflows key (CRI-299: bodies are leaf data, nested layers ride the flat entry list):\nbody: %s", name, body)
	}
	return layer
}

// assertCRI299FlatLayers decodes a wire layers array and asserts the flat
// wire shape of every sibling entry. Returns the layer entries in input
// order.
func assertCRI299FlatLayers(t *testing.T, entries json.RawMessage) []map[string]any {
	t.Helper()
	var layers []map[string]any
	if err := json.Unmarshal(entries, &layers); err != nil {
		t.Fatalf("decode layers array: %v\nraw: %s", err, entries)
	}
	for _, layer := range layers {
		assertCRI299FlatLayer(t, layer)
	}
	return layers
}

// cri299ParseBody parses a flat layer entry's body string into the compiled
// body graph map.
func cri299ParseBody(t *testing.T, entry map[string]any) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal([]byte(entry["body"].(string)), &body); err != nil {
		t.Fatalf("layer %v body does not parse: %v", entry["name"], err)
	}
	return body
}

// cri299CompiledLayers fetches the unchanged compile --format json dialect
// for the fixture and returns its top-level subworkflows array as generic
// entries. The compile JSON keeps its own nested shape (nesting stays
// internal to that published contract).
func cri299CompiledLayers(t *testing.T, parentPath string) []map[string]any {
	t.Helper()
	compileOut, err := compileWorkflowOutput(context.Background(), parentPath, "", "json", nil, false, false)
	if err != nil {
		t.Fatalf("compile --format json: %v", err)
	}
	var compiled struct {
		Subworkflows []map[string]any `json:"subworkflows"`
	}
	if err := json.Unmarshal(compileOut, &compiled); err != nil {
		t.Fatalf("unmarshal compile JSON: %v", err)
	}
	if compiled.Subworkflows == nil {
		t.Fatal("compile JSON has no subworkflows array for a two-level fixture")
	}
	return compiled.Subworkflows
}

// cri299BodyWithoutSubworkflows copies a parsed compiled body map without
// its subworkflows key, so content comparisons between a wire body string
// and the compile-JSON tree isolate the CRI-299 flattening: a wire body is
// exactly the compiled body minus subworkflows.
func cri299BodyWithoutSubworkflows(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	stripped := maps.Clone(body)
	delete(stripped, "subworkflows")
	return stripped
}

// TestWorkflowGraphs_LocalFlatShapeRegression is the CRI-299 regression
// gate (superseding the CRI-298 nested-uniform pin): applying a three-level
// subworkflow workflow locally with ND-JSON events output emits exactly 6
// envelopes with WorkflowGraphs at index 0 (before RunStarted) carrying
// payload_type exactly "WorkflowGraphs", and the payload is the flat layer
// list: every layer at every depth is a sibling entry in the top-level
// array — child and grand side by side — every entry carries the uniform
// {name, sourcePath, body} shape with body a JSON string, and no body
// string contains a subworkflows key (bodies are leaf data only). The
// byte-level assertions pin the absence of both the escaped subworkflows
// key and snake_case source_path anywhere in the payload. The compiled body
// content still matches criteria compile --format json for the same
// fixture; the compile JSON itself keeps its own nested shape (its own
// published contract, unchanged).
func TestWorkflowGraphs_LocalFlatShapeRegression(t *testing.T) {
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

	if contains := "source_path"; bytes.Contains(first.Payload, []byte(contains)) {
		t.Fatalf("wire payload contains %q (snake_case producer bug): %s", contains, first.Payload)
	}
	// Byte-level flat pin (CRI-299): a body carrying a subworkflows key
	// would surface in the payload bytes as the escaped-in-string JSON key —
	// the top-level layers array itself has no subworkflows key, so the
	// escaped spelling can only come from inside a body string.
	if contains := `\"subworkflows\"`; bytes.Contains(first.Payload, []byte(contains)) {
		t.Fatalf("wire payload contains %q (CRI-299: a body still carries nested layers): %s", contains, first.Payload)
	}

	entries := assertCRI299FlatLayers(t, first.Payload)
	if len(entries) != 2 {
		t.Fatalf("payload has %d sibling layers, want 2 (every layer its own entry, at every depth)", len(entries))
	}

	// The wire layer entries carry the compile-JSON layer values unchanged:
	// name and sourcePath match the compile-JSON source_path dialect.
	compileLayers := cri299CompiledLayers(t, parentPath)
	if len(compileLayers) != 1 {
		t.Fatalf("compile JSON has %d top-level subworkflows, want 1 (the compile JSON keeps its own nested shape)", len(compileLayers))
	}
	compileGrandLayer := compileLayers[0]["body"].(map[string]any)["subworkflows"].([]any)[0].(map[string]any)

	childEntry := entries[0]
	if got := childEntry["name"]; got != compileLayers[0]["name"] {
		t.Fatalf("top-level layer name = %v, want %v", got, compileLayers[0]["name"])
	}
	if got := childEntry["sourcePath"]; got != compileLayers[0]["source_path"] {
		t.Fatalf("top-level layer sourcePath = %v, want compile source_path %v", got, compileLayers[0]["source_path"])
	}

	// The child wire body is the compiled child graph with the subworkflows
	// key stripped: exactly the compile-JSON body minus subworkflows, with
	// no key re-spelling anywhere (camelCase sourcePath lives on the layer
	// entry, not inside the body).
	childBody := cri299ParseBody(t, childEntry)
	if !reflect.DeepEqual(childBody, cri299BodyWithoutSubworkflows(t, compileLayers[0]["body"].(map[string]any))) {
		t.Fatalf("child wire body differs from leaf compile-JSON body\n wire: %v\n compile: %v", childBody, compileLayers[0]["body"])
	}

	// Depth 2: grand is its own sibling entry right after child, in the same
	// wire shape, with its compiled body content preserved (grand is a leaf
	// in the compile JSON too, so the bodies match exactly).
	grandEntry := entries[1]
	if got := grandEntry["name"]; got != compileGrandLayer["name"] {
		t.Fatalf("sibling layer name = %v, want %v", got, compileGrandLayer["name"])
	}
	if got := grandEntry["sourcePath"]; got != compileGrandLayer["source_path"] {
		t.Fatalf("sibling layer sourcePath = %v, want compile source_path %v", got, compileGrandLayer["source_path"])
	}
	grandBody := cri299ParseBody(t, grandEntry)
	if !reflect.DeepEqual(grandBody, compileGrandLayer["body"].(map[string]any)) {
		t.Fatalf("grand wire body differs from compile-JSON body\n wire: %v\n compile: %v", grandBody, compileGrandLayer["body"])
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
	if len(serverGraphs.Subworkflows) != 2 {
		t.Fatalf("server payload has %d sibling layers, want 2 (every layer its own entry, at every depth)", len(serverGraphs.Subworkflows))
	}
	if got := serverGraphs.Subworkflows[0].GetName(); got != "child" {
		t.Fatalf("sibling layer 0 name = %q, want %q", got, "child")
	}
	if got := serverGraphs.Subworkflows[1].GetName(); got != "grand" {
		t.Fatalf("sibling layer 1 name = %q, want %q", got, "grand")
	}

	// CRI-299: the flat wire shape holds on the server payload — every layer
	// at every depth is a sibling entry and every body is a JSON string
	// carrying only that layer's own compiled graph, with no snake_case
	// source_path, no object bodies, and no subworkflows key inside any body
	// anywhere in the wire.
	protoLayerBytes := mustProtojsonMarshal(t, serverGraphs)
	if bytes.Contains(protoLayerBytes, []byte("source_path")) {
		t.Fatalf("protojson payload contains %q (snake_case producer bug): %s", "source_path", protoLayerBytes)
	}
	if bytes.Contains(protoLayerBytes, []byte(`\"subworkflows\"`)) {
		t.Fatalf("protojson payload contains an escaped subworkflows key (CRI-299: a body still carries nested layers): %s", protoLayerBytes)
	}
	var protoLayers struct {
		Subworkflows []map[string]any `json:"subworkflows"`
	}
	if err := json.Unmarshal(protoLayerBytes, &protoLayers); err != nil {
		t.Fatalf("decode protojson subworkflows: %v", err)
	}
	if len(protoLayers.Subworkflows) != 2 {
		t.Fatalf("protojson payload has %d sibling layers, want 2", len(protoLayers.Subworkflows))
	}
	childEntry := assertCRI299FlatLayer(t, protoLayers.Subworkflows[0])
	childBody := cri299ParseBody(t, childEntry)
	if got := childBody["name"]; got != "cri278_child" {
		t.Fatalf("child body graph name = %v, want cri278_child", got)
	}
	grandEntry := assertCRI299FlatLayer(t, protoLayers.Subworkflows[1])
	grandBody := cri299ParseBody(t, grandEntry)
	if got := grandBody["name"]; got != "cri278_grand" {
		t.Fatalf("grand body graph name = %v, want cri278_grand", got)
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

// TestWorkflowGraphs_EmittersAgreeOnFlatShape pins CRI-299 across both
// emitters: the local NDJSON builder (raw layers array) and the server
// builder (pb.WorkflowGraphs via protojson) serialize the SAME compiled
// graph into canonically identical flat payloads — every layer a sibling
// entry, bodies leaf data only.
func TestWorkflowGraphs_EmittersAgreeOnFlatShape(t *testing.T) {
	parentPath := writeCRI278TwoLevelWorkflow(t)
	_, graph, err := parseCompileForCli(context.Background(), parentPath, nil, false, false)
	if err != nil {
		t.Fatalf("compile fixture: %v", err)
	}

	localJSON, err := workflowGraphsLayersJSON(graph)
	if err != nil {
		t.Fatalf("local layers JSON: %v", err)
	}
	payload, err := buildWorkflowGraphsPayload(graph)
	if err != nil {
		t.Fatalf("server payload: %v", err)
	}
	serverJSON := mustProtojsonMarshal(t, payload)

	var serverLayers struct {
		Subworkflows []map[string]any `json:"subworkflows"`
	}
	if err := json.Unmarshal(serverJSON, &serverLayers); err != nil {
		t.Fatalf("decode server protojson: %v", err)
	}
	// The local payload IS the layers array; the server payload is the
	// WorkflowGraphs message wrapping the same array in its subworkflows
	// field. The arrays must be canonically identical.
	if !reflect.DeepEqual(canonicalJSON(t, localJSON), canonicalJSON(t, mustJSON(t, serverLayers.Subworkflows))) {
		t.Fatalf("local and server payloads differ for the same graph\n local: %s\n server: %s", localJSON, serverJSON)
	}
	entries := assertCRI299FlatLayers(t, localJSON)
	if len(entries) != 2 {
		t.Fatalf("flat payload has %d sibling layers, want 2", len(entries))
	}
}

// mustJSON re-marshals a value to compact JSON bytes (test helper).
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %T: %v", v, err)
	}
	return b
}

func mustProtojsonMarshal(t *testing.T, msg *pb.WorkflowGraphs) []byte {
	t.Helper()
	b, err := protojson.Marshal(msg)
	if err != nil {
		t.Fatalf("protojson marshal: %v", err)
	}
	return b
}
