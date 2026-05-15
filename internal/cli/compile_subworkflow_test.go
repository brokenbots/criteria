package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeCallee writes a minimal callee workflow HCL to dir/name/main.hcl.
// vars maps variable names to whether they have a default (true = has default).
func writeCallee(t *testing.T, parent, name string, vars map[string]bool) string {
	t.Helper()
	dir := filepath.Join(parent, name)
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("create callee dir %q: %v", dir, err)
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("workflow %q {\n  version = \"1\"\n  initial_state = \"done\"\n  target_state  = \"done\"\n}\n\n", name))
	for varName, hasDef := range vars {
		if hasDef {
			sb.WriteString(fmt.Sprintf("variable %q {\n  type    = \"string\"\n  default = \"x\"\n}\n", varName))
		} else {
			sb.WriteString(fmt.Sprintf("variable %q {\n  type = \"string\"\n}\n", varName))
		}
	}
	sb.WriteString("state \"done\" {\n  terminal = true\n  success  = true\n}\n")
	if err := os.WriteFile(filepath.Join(dir, "main.hcl"), []byte(sb.String()), 0o644); err != nil {
		t.Fatalf("write main.hcl in %q: %v", dir, err)
	}
	return dir
}

// writeParent writes HCL content to dir/parent.hcl and returns the file path.
func writeParent(t *testing.T, dir, content string) string {
	t.Helper()
	p := filepath.Join(dir, "parent.hcl")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write parent.hcl: %v", err)
	}
	return p
}

// compileToMap compiles the workflow at path to JSON and unmarshals it into a
// map so individual fields can be inspected without importing compileJSON.
func compileToMap(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := compileWorkflowOutput(context.Background(), path, "json", nil)
	if err != nil {
		t.Fatalf("compileWorkflowOutput: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("json unmarshal: %v", err)
	}
	return m
}

// TestCompileJSON_SubworkflowStepHasSubworkflowField verifies that a step
// targeting a subworkflow emits "subworkflow": "<name>" and no "adapter" key.
func TestCompileJSON_SubworkflowStepHasSubworkflowField(t *testing.T) {
	dir := t.TempDir()
	writeCallee(t, dir, "inner", nil)

	hcl := `
workflow "parent" {
  version       = "1"
  initial_state = "run_inner"
  target_state  = "done"
}

subworkflow "inner" {
  source = "./inner"
}

step "run_inner" {
  target = subworkflow.inner
  outcome "success" { next = "done" }
}

state "done" {
  terminal = true
  success  = true
}
`
	m := compileToMap(t, writeParent(t, dir, hcl))

	steps, ok := m["steps"].([]any)
	if !ok || len(steps) == 0 {
		t.Fatalf("expected non-empty steps array, got: %v", m["steps"])
	}
	step, ok := steps[0].(map[string]any)
	if !ok {
		t.Fatalf("step is not a JSON object: %v", steps[0])
	}
	if got, _ := step["subworkflow"].(string); got != "inner" {
		t.Errorf(`step["subworkflow"] = %q, want "inner"`, got)
	}
	if _, exists := step["adapter"]; exists {
		t.Errorf(`step["adapter"] should be absent for subworkflow-targeted step, got: %v`, step["adapter"])
	}
}

// TestCompileJSON_SubworkflowStepInputKeys verifies that a subworkflow step with
// a step-level input block emits the correct input_keys (not null).
func TestCompileJSON_SubworkflowStepInputKeys(t *testing.T) {
	dir := t.TempDir()
	writeCallee(t, dir, "inner", map[string]bool{"greeting": true})

	hcl := `
workflow "parent" {
  version       = "1"
  initial_state = "run_inner"
  target_state  = "done"
}

subworkflow "inner" {
  source = "./inner"
}

step "run_inner" {
  target = subworkflow.inner
  input {
    greeting = "hello"
  }
  outcome "success" { next = "done" }
}

state "done" {
  terminal = true
  success  = true
}
`
	m := compileToMap(t, writeParent(t, dir, hcl))

	steps, _ := m["steps"].([]any)
	if len(steps) == 0 {
		t.Fatalf("expected steps, got none")
	}
	step, _ := steps[0].(map[string]any)
	keys, _ := step["input_keys"].([]any)
	if len(keys) == 0 {
		t.Fatalf(`input_keys is empty or null; want ["greeting"]`)
	}
	if got, _ := keys[0].(string); got != "greeting" {
		t.Errorf(`input_keys[0] = %q, want "greeting"`, got)
	}
}

// TestCompileJSON_SubworkflowsArrayPresent verifies that a workflow with a
// subworkflow declaration emits a top-level "subworkflows" array with the
// callee's name, source_path, and body (including steps and states).
func TestCompileJSON_SubworkflowsArrayPresent(t *testing.T) {
	dir := t.TempDir()
	calleeDir := writeCallee(t, dir, "inner", nil)
	// The resolver canonicalises symlinks (e.g. /var → /private/var on macOS).
	if canonical, err := filepath.EvalSymlinks(calleeDir); err == nil {
		calleeDir = canonical
	}

	hcl := `
workflow "parent" {
  version       = "1"
  initial_state = "run_inner"
  target_state  = "done"
}

subworkflow "inner" {
  source = "./inner"
}

step "run_inner" {
  target = subworkflow.inner
  outcome "success" { next = "done" }
}

state "done" {
  terminal = true
  success  = true
}
`
	m := compileToMap(t, writeParent(t, dir, hcl))

	sws, ok := m["subworkflows"].([]any)
	if !ok || len(sws) == 0 {
		t.Fatalf("expected non-empty subworkflows array, got: %v", m["subworkflows"])
	}
	sw, ok := sws[0].(map[string]any)
	if !ok {
		t.Fatalf("subworkflow entry is not a JSON object: %v", sws[0])
	}
	if name, _ := sw["name"].(string); name != "inner" {
		t.Errorf(`subworkflow name = %q, want "inner"`, name)
	}
	srcPath, _ := sw["source_path"].(string)
	if srcPath != calleeDir {
		t.Errorf("source_path = %q, want %q", srcPath, calleeDir)
	}
	body, ok := sw["body"].(map[string]any)
	if !ok {
		t.Fatalf("subworkflow body is not a JSON object: %v", sw["body"])
	}
	if _, ok := body["steps"]; !ok {
		t.Error(`subworkflow body missing "steps" field`)
	}
	if _, ok := body["states"]; !ok {
		t.Error(`subworkflow body missing "states" field`)
	}
}

// TestCompileJSON_NoSubworkflows_SubworkflowsFieldOmitted verifies that a
// workflow with no subworkflow declarations omits the "subworkflows" key entirely.
func TestCompileJSON_NoSubworkflows_SubworkflowsFieldOmitted(t *testing.T) {
	dir := t.TempDir()
	hcl := `
workflow "simple" {
  version       = "1"
  initial_state = "run"
  target_state  = "done"
}

adapter "noop" "default" {}

step "run" {
  target = adapter.noop.default
  input {
    command = "echo hi"
  }
  outcome "success" { next = "done" }
}

state "done" {
  terminal = true
  success  = true
}
`
	m := compileToMap(t, writeParent(t, dir, hcl))
	if _, exists := m["subworkflows"]; exists {
		t.Errorf("expected subworkflows field to be absent for adapter-only workflow, got: %v", m["subworkflows"])
	}
}

// TestCompileJSON_AdapterStepUnchanged is a regression test verifying that an
// adapter-targeted step still emits "adapter", no "subworkflow", and correct
// "input_keys" after the subworkflow changes.
func TestCompileJSON_AdapterStepUnchanged(t *testing.T) {
	dir := t.TempDir()
	hcl := `
workflow "simple" {
  version       = "1"
  initial_state = "run"
  target_state  = "done"
}

adapter "noop" "default" {}

step "run" {
  target = adapter.noop.default
  input {
    command = "echo hi"
  }
  outcome "success" { next = "done" }
}

state "done" {
  terminal = true
  success  = true
}
`
	m := compileToMap(t, writeParent(t, dir, hcl))

	steps, _ := m["steps"].([]any)
	if len(steps) == 0 {
		t.Fatalf("expected steps, got none")
	}
	step, _ := steps[0].(map[string]any)

	if adapter, _ := step["adapter"].(string); adapter == "" {
		t.Errorf(`adapter-targeted step missing "adapter" field; got: %v`, step["adapter"])
	}
	if _, exists := step["subworkflow"]; exists {
		t.Errorf(`adapter step should not have "subworkflow" field, got: %v`, step["subworkflow"])
	}
	keys, _ := step["input_keys"].([]any)
	if len(keys) == 0 {
		t.Fatalf(`input_keys is empty or null for adapter step`)
	}
	if got, _ := keys[0].(string); got != "command" {
		t.Errorf(`input_keys[0] = %q, want "command"`, got)
	}
}

// TestCompileJSON_SubworkflowStepExactContract pins the exact serialised JSON
// for a subworkflow-targeted step with a bound input block. This is an
// end-to-end contract test: it compiles a full workflow through the CLI code
// path and checks the exact bytes of the step object so that any regression
// (dropped "subworkflow", unexpected "adapter", null input_keys, renamed field)
// causes a deterministic failure.
func TestCompileJSON_SubworkflowStepExactContract(t *testing.T) {
	dir := t.TempDir()
	writeCallee(t, dir, "callee", map[string]bool{"greeting": true})

	hcl := `
workflow "contract_test" {
  version       = "1"
  initial_state = "greet"
  target_state  = "done"
}

subworkflow "callee" {
  source = "./callee"
}

step "greet" {
  target = subworkflow.callee
  input {
    greeting = "hello"
  }
  outcome "success" { next = "done" }
}

state "done" {
  terminal = true
  success  = true
}
`
	b, err := compileWorkflowOutput(context.Background(), writeParent(t, dir, hcl), "json", nil)
	if err != nil {
		t.Fatalf("compileWorkflowOutput: %v", err)
	}

	// Parse the output preserving raw JSON bytes so struct field order is
	// maintained (a map[string]any round-trip would sort keys alphabetically).
	var root struct {
		Steps []json.RawMessage `json:"steps"`
	}
	if err := json.Unmarshal(b, &root); err != nil {
		t.Fatalf("json unmarshal: %v", err)
	}
	if len(root.Steps) == 0 {
		t.Fatalf("steps array is empty")
	}

	// Compact both sides to remove whitespace differences, then compare.
	// Key order is preserved from the struct's JSON marshaling.
	const wantStep = `{
  "name": "greet",
  "subworkflow": "callee",
  "input_keys": [
    "greeting"
  ],
  "allow_tools": null,
  "outcomes": [
    {
      "name": "success",
      "next": "done"
    }
  ]
}`
	var gotCompact, wantCompact bytes.Buffer
	if err := json.Compact(&gotCompact, root.Steps[0]); err != nil {
		t.Fatalf("compact got: %v", err)
	}
	if err := json.Compact(&wantCompact, []byte(wantStep)); err != nil {
		t.Fatalf("compact want: %v", err)
	}
	if !bytes.Equal(gotCompact.Bytes(), wantCompact.Bytes()) {
		// Re-indent for readable diff output.
		var gotPretty bytes.Buffer
		_ = json.Indent(&gotPretty, gotCompact.Bytes(), "", "  ")
		t.Fatalf("step JSON contract mismatch\nwant:\n%s\n\ngot:\n%s", wantStep, gotPretty.String())
	}
}
