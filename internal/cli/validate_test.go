package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateDiagJSON_ValidWorkflow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "workflow.hcl")
	src := `
workflow {
  name = "test"
  version = "1.0"
  initial_state = "hello"
  target_state = "hello"
}

adapter "noop" "default" {
  config {}
}

step "hello" {
  target = adapter.noop.default
  input { command = "echo hi" }
  outcome "success" { next = state.hello }
}
`
	err := os.WriteFile(path, []byte(src), 0o644)
	require.NoError(t, err)

	out := captureOutput(t, func() {
		ok := validatePath(context.Background(), path, nil, true, false)
		assert.True(t, ok)
	})

	var diags []validateDiagnostic
	err = json.Unmarshal([]byte(out), &diags)
	require.NoError(t, err, "output must be valid JSON: %s", out)
	// The real noop adapter built by TestMain declares no output_schema, so
	// schema resolution now emits a distinct warning while still validating
	// successfully.
	require.Len(t, diags, 1)
	assert.Equal(t, "warning", diags[0].Severity)
	assert.Contains(t, diags[0].Summary, "noop")
	assert.Contains(t, diags[0].Summary, "declares no output schema")
}

func TestValidateDiagJSON_InvalidWorkflow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "workflow.hcl")
	src := `
workflow {
  name = "test"
  version = "1.0"
  initial_state = "hello"
  target_state = "hello"
}

adapter "noop" "default" {
  config {}
}

step "hello" {
  target = adapter.noop.default
  input { command = "echo hi"
  // missing closing brace for input block
}
`
	err := os.WriteFile(path, []byte(src), 0o644)
	require.NoError(t, err)

	out := captureOutput(t, func() {
		ok := validatePath(context.Background(), path, nil, true, false)
		assert.False(t, ok)
	})

	var diags []validateDiagnostic
	err = json.Unmarshal([]byte(out), &diags)
	require.NoError(t, err, "output must be valid JSON: %s", out)
	require.Len(t, diags, 1)
	assert.Equal(t, "error", diags[0].Severity)
	assert.NotEmpty(t, diags[0].Summary)
	assert.Equal(t, path, diags[0].File)
	assert.Greater(t, diags[0].Line, 0)
	assert.Greater(t, diags[0].Col, 0)
}

func TestValidateDiagJSON_WarningOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "workflow.hcl")
	src := `
workflow {
  name = "test"
  version = "1.0"
  initial_state = "hello"
  target_state = "hello"
}

adapter "noop" "default" {
  config {}
}

step "hello" {
  target = adapter.noop.default
  input { command = "echo hi" }
  next = ["bye"]
}
`
	err := os.WriteFile(path, []byte(src), 0o644)
	require.NoError(t, err)

	out := captureOutput(t, func() {
		ok := validatePath(context.Background(), path, nil, true, false)
		assert.False(t, ok) // unresolved next reference is treated as error by current compiler
	})

	var diags []validateDiagnostic
	err = json.Unmarshal([]byte(out), &diags)
	require.NoError(t, err, "output must be valid JSON: %s", out)
	require.GreaterOrEqual(t, len(diags), 1)
	// At least one diagnostic should be present
	for _, d := range diags {
		assert.NotEmpty(t, d.Severity)
		assert.NotEmpty(t, d.Summary)
	}
}

func captureOutput(t *testing.T, fn func()) string {
	t.Helper()
	oldStdout := os.Stdout
	oldStderr := os.Stderr
	defer func() {
		os.Stdout = oldStdout
		os.Stderr = oldStderr
	}()

	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w
	os.Stderr = w

	fn()

	_ = w.Close()
	var buf bytes.Buffer
	_, err = buf.ReadFrom(r)
	require.NoError(t, err)
	_ = r.Close()
	return buf.String()
}

func TestPrintDiagnosticsJSON_MarshalError(t *testing.T) {
	// printDiagnosticsJSON with nil/empty input should emit []
	out := captureOutput(t, func() {
		printDiagnosticsJSON(nil)
	})
	assert.Equal(t, "[]\n", out)
}

func TestValidateDiagnosticJSONFieldNames(t *testing.T) {
	// Ensure field names match the expected JSON contract.
	d := validateDiagnostic{
		Severity: "error",
		File:     "/tmp/test.hcl",
		Line:     3,
		Col:      5,
		EndLine:  3,
		EndCol:   10,
		Summary:  "bad thing",
		Detail:   "more info",
	}
	b, err := json.Marshal(d)
	require.NoError(t, err)
	var m map[string]any
	err = json.Unmarshal(b, &m)
	require.NoError(t, err)

	assert.Equal(t, "error", m["severity"])
	assert.Equal(t, "/tmp/test.hcl", m["file"])
	assert.Equal(t, float64(3), m["line"])
	assert.Equal(t, float64(5), m["col"])
	assert.Equal(t, float64(3), m["end_line"])
	assert.Equal(t, float64(10), m["end_col"])
	assert.Equal(t, "bad thing", m["summary"])
	assert.Equal(t, "more info", m["detail"])
	_, ok := m["extra"]
	assert.False(t, ok)
}

func TestValidateCriteriaVersion_Incompatible(t *testing.T) {
	t.Setenv("CRITERIA_OVERRIDE_VERSION", "0.5.7")

	dir := t.TempDir()
	path := filepath.Join(dir, "workflow.hcl")
	src := `workflow {
  name             = "wf"
  version          = "1"
  criteria_version = ">=0.5.8, <0.6.0"
  initial_state    = "done"
  target_state     = "done"
}

state "done" {
  terminal = true
  success  = true
}
`
	err := os.WriteFile(path, []byte(src), 0o644)
	require.NoError(t, err)

	out := captureOutput(t, func() {
		ok := validatePath(context.Background(), path, nil, false, false)
		assert.False(t, ok)
	})

	assert.Contains(t, out, `workflow "wf" requires Criteria >=0.5.8, <0.6.0`)
	assert.Contains(t, out, "running engine is v0.5.7")
}

func TestValidateCriteriaVersion_Compatible(t *testing.T) {
	t.Setenv("CRITERIA_OVERRIDE_VERSION", "0.5.8")

	dir := t.TempDir()
	path := filepath.Join(dir, "workflow.hcl")
	src := `workflow {
  name             = "wf"
  version          = "1"
  criteria_version = ">=0.5.8, <0.6.0"
  initial_state    = "done"
  target_state     = "done"
}

state "done" {
  terminal = true
  success  = true
}
`
	err := os.WriteFile(path, []byte(src), 0o644)
	require.NoError(t, err)

	out := captureOutput(t, func() {
		ok := validatePath(context.Background(), path, nil, false, false)
		assert.True(t, ok)
	})

	assert.Contains(t, out, "ok")
}
