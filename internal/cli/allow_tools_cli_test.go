package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCompile_AllowToolsWarningsSurface verifies that criteria compile emits
// the allow_tools matchability warnings introduced by CRI-27 to stderr, even
// when the adapter is not installed and only compile-time shape checks apply.
func TestCompile_AllowToolsWarningsSurface(t *testing.T) {
	dir := writeAllowToolsRepro(t)

	stderr := captureStderr(t, func() {
		_, err := compileWorkflowOutput(context.Background(), dir, "json", nil, false, false)
		require.NoError(t, err)
	})

	require.Contains(t, stderr, "is not a valid glob pattern", "stderr must surface invalid glob warning")
	require.Contains(t, stderr, "looks like the Claude Code argument-scoping form", "stderr must surface wrong-scoping-form warning")
}

// TestCompile_AllowToolsWarningsAsErrors verifies that --warnings-as-errors
// promotes allow_tools matchability warnings to errors for criteria compile.
func TestCompile_AllowToolsWarningsAsErrors(t *testing.T) {
	dir := writeAllowToolsRepro(t)

	stderr := captureStderr(t, func() {
		_, err := compileWorkflowOutput(context.Background(), dir, "json", nil, true, false)
		require.Error(t, err, "compile must fail when allow_tools warnings are promoted")
	})

	require.Contains(t, stderr, "is not a valid glob pattern", "stderr must surface invalid glob warning as error")
	require.Contains(t, stderr, "looks like the Claude Code argument-scoping form", "stderr must surface wrong-scoping-form warning as error")
}

// TestPlan_AllowToolsWarningsSurface verifies that criteria plan surfaces the
// same allow_tools matchability warnings via parseCompileForCli.
func TestPlan_AllowToolsWarningsSurface(t *testing.T) {
	dir := writeAllowToolsRepro(t)

	stderr := captureStderr(t, func() {
		_, err := renderPlanOutput(context.Background(), dir, nil)
		require.NoError(t, err)
	})

	require.Contains(t, stderr, "is not a valid glob pattern", "stderr must surface invalid glob warning")
	require.Contains(t, stderr, "looks like the Claude Code argument-scoping form", "stderr must surface wrong-scoping-form warning")
}

// TestValidate_AllowToolsWarningsSurface verifies that criteria validate prints
// allow_tools matchability warnings while still succeeding (rc path returns ok).
func TestValidate_AllowToolsWarningsSurface(t *testing.T) {
	dir := writeAllowToolsRepro(t)

	stderr := captureStderr(t, func() {
		ok := validatePath(context.Background(), dir, nil, false, false)
		require.True(t, ok, "validate must succeed when warnings are not promoted")
	})

	require.Contains(t, stderr, "is not a valid glob pattern", "stderr must surface invalid glob warning")
	require.Contains(t, stderr, "looks like the Claude Code argument-scoping form", "stderr must surface wrong-scoping-form warning")
}

// TestValidate_AllowToolsWarningsAsErrors verifies that --warnings-as-errors
// promotes allow_tools matchability warnings to errors for criteria validate.
func TestValidate_AllowToolsWarningsAsErrors(t *testing.T) {
	dir := writeAllowToolsRepro(t)

	stderr := captureStderr(t, func() {
		ok := validatePath(context.Background(), dir, nil, false, true)
		require.False(t, ok, "validate must fail when allow_tools warnings are promoted")
	})

	require.Contains(t, stderr, "is not a valid glob pattern", "stderr must surface invalid glob warning as error")
	require.Contains(t, stderr, "looks like the Claude Code argument-scoping form", "stderr must surface wrong-scoping-form warning as error")
}

// writeAllowToolsRepro creates a temporary workflow module containing
// allow_tools entries that trigger the CRI-27 diagnostics for invalid glob
// syntax and the Claude Code argument-scoping form. The adapter is intentionally
// unresolved so that only shape checks (which do not require a permissions
// vocabulary) fire.
func writeAllowToolsRepro(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := `workflow {
  name          = "allow-tools-repro"
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
}

adapter "claude-agent" "default" {}

step "run" {
  target = adapter.claude-agent.default
  allow_tools = ["Read", "Bash:git *", "Bash(git log:*)", "Bash[unclosed"]
  input {
    prompt = "hello"
  }
  outcome "success" { next = state.done }
}

state "done" { terminal = true }
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.hcl"), []byte(src), 0o600))
	return dir
}
