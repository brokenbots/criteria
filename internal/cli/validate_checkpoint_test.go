package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestValidate_WorkflowWithCheckpointingAdapter is the CRI-203 acceptance
// gate: the built criteria binary's `workflow validate` passes on a workflow
// carried by a checkpointing adapter — an adapter whose InfoResponse declares
// checkpointable state (the conformance stateful fixture, mode blob / schema
// stateful.v1). Validation resolves the adapter through the production
// discovery path, proved by the resolved-adapter warning the stateful
// fixture's missing output schema produces; an unresolved binary would have
// produced the "schema unverified" warning instead.
func TestValidate_WorkflowWithCheckpointingAdapter(t *testing.T) {
	dir := t.TempDir()
	plugins := t.TempDir()
	buildStatefulTestPlugin(t, plugins)
	t.Setenv("CRITERIA_ADAPTERS", plugins)

	path := filepath.Join(dir, "workflow.hcl")
	src := `
workflow {
  name = "ck-validate"
  version = "1.0"
  initial_state = "exec"
  target_state = "done"
}

adapter "stateful" "default" {
  config {}
}

step "exec" {
  target = adapter.stateful.default
  input {}

  outcome "success" { next = state.done }
  outcome "failure" { next = state.failed }
}

state "done"   { terminal = true }
state "failed" {
  terminal = true
  success  = false
}
`
	require.NoError(t, os.WriteFile(path, []byte(src), 0o644))

	criteria := buildValidateCriteriaBinary(t)
	cmd := exec.Command(criteria, "validate", path)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "validating a checkpointing workflow must pass:\n%s", out)
	assert.Contains(t, string(out), path+": ok")
	// The checkpointing adapter was resolved and its handshake (including the
	// blob state declaration) read via Info(): the schemaless-adapter warning
	// only fires on the resolved path.
	assert.Contains(t, string(out), `adapter "stateful" resolved but declares no output schema`)
	assert.NotContains(t, string(out), "schema unverified")
}

// buildStatefulTestPlugin compiles the conformance stateful adapter (the
// fixture whose InfoResponse declares blob state) into dir under the
// conventional by-name install basename so the production discovery
// path resolves it.
func buildStatefulTestPlugin(t *testing.T, dir string) {
	t.Helper()
	bin := filepath.Join(dir, "criteria-adapter-stateful")
	cmd := exec.Command("go", "build", "-o", bin, "./internal/adapter/conformance/testdata/stateful")
	cmd.Dir = cliModuleRoot(t)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "build stateful adapter:\n%s", out)
}

// buildValidateCriteriaBinary compiles the real criteria CLI binary for the
// production-shape `validate` invocation.
func buildValidateCriteriaBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "criteria")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/criteria")
	cmd.Dir = cliModuleRoot(t)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "build criteria binary:\n%s", out)
	return bin
}

func cliModuleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok, "resolve caller path")
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}
