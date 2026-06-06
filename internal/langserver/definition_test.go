package langserver

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

func TestBuildIndexAndResolve(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.hcl")
	src := `workflow {
  name = "test"
  version = "1"
  initial_state = "fetch"
  target_state = "done"
}

step "fetch" {
  target = adapter.noop.default
  input { command = "echo hi" }
  outcome "success" { next = state.done }
}

state "done" { terminal = true }
`
	err := os.WriteFile(path, []byte(src), 0o644)
	require.NoError(t, err)

	idx, err := buildIndex(dir)
	require.NoError(t, err)

	loc, ok := idx.get("step", "fetch")
	require.True(t, ok)
	require.Equal(t, uri.File(path), loc.URI)

	loc, ok = idx.get("state", "done")
	require.True(t, ok)
	require.Equal(t, uri.File(path), loc.URI)
}

func TestExtractTraversalAt(t *testing.T) {
	tests := []struct {
		line     string
		col      int
		wantKind string
		wantName string
	}{
		{"next = step.greet", 10, "step", "greet"},
		{"next = state.done", 10, "state", "done"},
		{"target = adapter.noop.default", 15, "adapter", "noop.default"},
		{"value = var.name", 12, "variable", "name"},
		{"value = local.greeting", 14, "local", "greeting"},
		{"value = data.internal.counter", 18, "data", "internal.counter"},
		{"next = subworkflow.child", 15, "subworkflow", "child"},
		{"next = wait.pause", 10, "wait", "pause"},
		{"next = approval.signoff", 14, "approval", "signoff"},
		{"next = switch.branch", 13, "switch", "branch"},
		{"value = steps.fetch.stdout", 14, "step", "fetch"},
		{"return", 1, "", ""},
	}

	for _, tc := range tests {
		t.Run(tc.line, func(t *testing.T) {
			kind, name := extractTraversalAt(tc.line, tc.col)
			require.Equal(t, tc.wantKind, kind)
			require.Equal(t, tc.wantName, name)
		})
	}
}

func TestResolveDefinition(t *testing.T) {
	idx := make(symbolIndex)
	idx.add("step", "greet", protocol.Location{URI: "file:///test.hcl"})
	idx.add("variable", "name", protocol.Location{URI: "file:///test.hcl"})
	idx.add("output", "result", protocol.Location{URI: "file:///test.hcl"})
	idx.add("environment", "prod", protocol.Location{URI: "file:///test.hcl"})

	loc := resolveDefinition(idx, "next = step.greet", 10)
	require.NotNil(t, loc)
	require.Equal(t, uri.URI("file:///test.hcl"), loc.URI)

	loc = resolveDefinition(idx, "value = var.name", 12)
	require.NotNil(t, loc)
	require.Equal(t, uri.URI("file:///test.hcl"), loc.URI)

	loc = resolveDefinition(idx, "value = output.result", 12)
	require.NotNil(t, loc)
	require.Equal(t, uri.URI("file:///test.hcl"), loc.URI)

	loc = resolveDefinition(idx, "value = environment.prod", 17)
	require.NotNil(t, loc)
	require.Equal(t, uri.URI("file:///test.hcl"), loc.URI)

	loc = resolveDefinition(idx, "return", 1)
	require.Nil(t, loc)
}

func TestResolveDefinition_MultiSegmentSteps(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "workflow.hcl")
	src := `workflow {
  name = "test"
  version = "1"
  initial_state = "fetch"
  target_state = "done"
}

step "fetch" {
  target = adapter.noop.default
  input { command = "echo hi" }
  outcome "success" { next = state.done }
}

state "done" { terminal = true }
`
	err := os.WriteFile(path, []byte(src), 0o644)
	require.NoError(t, err)

	idx, err := buildIndex(dir)
	require.NoError(t, err)

	// steps.fetch.stdout resolves to step "fetch"
	loc := resolveDefinition(idx, "value = steps.fetch.stdout", 14)
	require.NotNil(t, loc)
	require.Equal(t, uri.File(path), loc.URI)
}
