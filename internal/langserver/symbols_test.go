package langserver

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.lsp.dev/protocol"
)

func TestFileSymbols(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.hcl")
	src := `workflow {
  name = "test"
  version = "1"
  initial_state = "fetch"
  target_state = "done"
}

adapter "noop" "default" {
  config {}
}

variable "name" {
  type = string
}

local "greeting" {
  value = "hello"
}

data "internal" "counter" {
  type = number
}

output "result" {
  value = local.greeting
}

step "fetch" {
  target = adapter.noop.default
  input { command = "echo hi" }
  outcome "success" { next = state.done }
}

state "done" { terminal = true }

wait "pause" { duration = "1s" }

approval "signoff" {
  approvers = ["alice"]
  reason = "please approve"
}

switch "branch" {}

subworkflow "child" {
  source = "./child"
}
`
	err := os.WriteFile(path, []byte(src), 0o644)
	require.NoError(t, err)

	syms, err := fileSymbols(path)
	require.NoError(t, err)

	want := map[string]protocol.SymbolKind{
		"noop.default":     protocol.SymbolKindClass,
		"name":             protocol.SymbolKindVariable,
		"greeting":         protocol.SymbolKindConstant,
		"internal.counter": protocol.SymbolKindObject,
		"result":           protocol.SymbolKindProperty,
		"fetch":            protocol.SymbolKindFunction,
		"done":             protocol.SymbolKindEnum,
		"pause":            protocol.SymbolKindEvent,
		"signoff":          protocol.SymbolKindEvent,
		"branch":           protocol.SymbolKindInterface,
		"child":            protocol.SymbolKindModule,
	}

	got := make(map[string]protocol.SymbolKind, len(syms))
	for _, s := range syms {
		got[s.Name] = s.Kind
	}

	for name, kind := range want {
		require.Contains(t, got, name, "expected symbol %s", name)
		require.Equal(t, kind, got[name], "symbol %s kind mismatch", name)
	}
}

func TestFileSymbolsSortsByPosition(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.hcl")
	src := `step "beta" {}
step "alpha" {}
`
	err := os.WriteFile(path, []byte(src), 0o644)
	require.NoError(t, err)

	syms, err := fileSymbols(path)
	require.NoError(t, err)
	require.Len(t, syms, 2)
	require.Equal(t, "beta", syms[0].Name)
	require.Equal(t, "alpha", syms[1].Name)
}
