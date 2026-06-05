# WS11 — `criteria langserver` (Minimal LSP server)

**Phase:** Language Cleanup · **Track:** Editor tooling · **Owner:** Workstream executor · **Depends on:** WS09 (`criteria spec` embeds files; WS11 can reuse embedded spec), WS10 (extension fixed so we understand what the LSP must replicate). · **Unblocks:** Neovim, Emacs, Zed, and any other LSP-capable editor. · **Base branch:** `main` · **Estimated effort:** 2–3 weeks

## Context

The VSCode extension (WS10) gives VSCode users diagnostics, go-to-definition, and a workspace outline. However it only works in VSCode and is implemented as a TypeScript extension, not a proper LSP server. Every other LSP-capable editor — Neovim, Emacs, Zed, Helix, etc. — gets nothing.

A `criteria langserver` subcommand speaks LSP JSON-RPC over stdin/stdout and delivers the same Minimal-tier capabilities to any compliant client. The Minimal tier is deliberately narrow: diagnostics, document symbols, and go-to-definition. This covers the majority of the editor-support value for workflow authors.

The extension's TypeScript code (diagnostics.ts, definition.ts, workspaceIndex.ts) proves the approach works; this workstream re-implements the same logic in Go, backed by the actual workflow compiler rather than regex heuristics.

### What "Minimal tier" means

| LSP method | Behaviour |
|---|---|
| `textDocument/publishDiagnostics` | On open/save, compile the workflow directory and push errors/warnings to the client |
| `textDocument/documentSymbol` | Return an outline of all named blocks (steps, states, adapters, switches, subworkflows, variables, locals, data, outputs, waits, approvals) |
| `textDocument/definition` | Resolve traversal references (`next = step.foo`, `target = adapter.shell.default`, `var.name`, `data.internal.counter`) to their declaration location |

Not in this WS: completions, hover docs, rename, semantic tokens, formatting.

## Prerequisites

- WS09 merged (not strictly required but keeps the PR diff clean).
- WS10 merged (defines what the LSP must replicate and exposes any edge cases).
- Evaluate LSP library choice before starting (see **Library selection** below).

## Library selection

Two viable options for a Go LSP server:

| Option | Pros | Cons |
|---|---|---|
| `go.lsp.dev/protocol` | Thin LSP type definitions only; full control | More boilerplate (stdio loop, dispatcher, request/response wiring) |
| `github.com/tliron/glsp` | Full server framework; handles stdio, dispatcher, lifecycle | Additional dependency; less control |

**Recommendation:** `go.lsp.dev/protocol` for type definitions + hand-roll the stdio JSON-RPC loop. The loop is ~100 lines and gives full control over goroutine model and cancellation. `glsp` is acceptable if the implementer prefers it.

## In scope

### Step 1 — Cobra command skeleton

**New file: `internal/cli/langserver.go`**

```go
package cli

import (
    "github.com/spf13/cobra"
    "github.com/brokenbots/criteria/internal/langserver"
)

func NewLangserverCmd() *cobra.Command {
    cmd := &cobra.Command{
        Use:    "langserver",
        Short:  "Start the Criteria LSP language server (reads JSON-RPC from stdin)",
        Hidden: false,
        Args:   cobra.NoArgs,
        RunE: func(cmd *cobra.Command, _ []string) error {
            cmd.SilenceUsage = true
            return langserver.Serve()
        },
    }
    return cmd
}
```

Register in `cmd/criteria/main.go`:
```go
root.AddCommand(cli.NewLangserverCmd())
```

### Step 2 — `internal/langserver/` package

Create the package directory. Core files:

#### `internal/langserver/server.go`

The stdio JSON-RPC loop. Handles:
- `initialize` / `initialized` — send `ServerCapabilities` declaring the three supported methods
- `shutdown` / `exit` — clean shutdown
- `textDocument/didOpen`, `textDocument/didSave`, `textDocument/didChange` — trigger diagnostics
- `textDocument/documentSymbol` — return symbol list
- `textDocument/definition` — return definition location

**`ServerCapabilities` to advertise:**
```json
{
  "textDocumentSync": { "openClose": true, "save": true, "change": 0 },
  "documentSymbolProvider": true,
  "definitionProvider": true
}
```

`change: 0` (None) means the server does not need incremental changes — it re-reads from disk on save. This keeps the implementation simple.

#### `internal/langserver/diagnostics.go`

On `didOpen` / `didSave`, run `criteria compile` on the workflow directory (the directory containing the saved file) and publish diagnostics.

Rather than parsing stderr text (the extension's approach), add a `--diag-json` flag to `criteria compile` (or `criteria validate`) in the same PR to emit structured JSON:

```json
{
  "errors": [
    {"file": "/abs/path/file.chcl", "line": 12, "col": 3, "end_line": 12, "end_col": 15, "message": "..."}
  ],
  "warnings": [...]
}
```

This decouples the langserver from text format changes. If adding `--diag-json` is too large for this WS, fall back to parsing stderr with the same regex as `diagnostics.ts` (already proven to work).

```go
func publishDiagnostics(conn Conn, docURI string) error {
    dir := filepath.Dir(uriToPath(docURI))
    diags, err := runCompileDiags(dir)
    if err != nil { /* treat as a single file-level error diagnostic */ }
    // group by file, send textDocument/publishDiagnostics for each affected file
    return conn.Notify("textDocument/publishDiagnostics", ...)
}
```

#### `internal/langserver/symbols.go`

Parse all `.hcl` and `.chcl` files in the workflow directory using `workflow.Parse` (the existing compiler parser). Walk the resulting `*workflow.Spec` to produce a flat `[]DocumentSymbol`:

| Block kind | SymbolKind | Name |
|---|---|---|
| `step` | Function (12) | step name |
| `state` | Enum (10) | state name |
| `adapter` | Class (5) | `<type>.<name>` |
| `switch` | Interface (11) | switch name |
| `variable` | Variable (13) | variable name |
| `local` | Constant (14) | local name |
| `data` | Object (19) | `<kind>.<name>` |
| `output` | Property (7) | output name |
| `wait` | Event (24) | wait name |
| `approval` | Event (24) | approval name |
| `subworkflow` | Module (2) | subworkflow name |

Each symbol includes the file path, start line, and end line (from HCL source ranges, which `workflow.Parse` preserves).

#### `internal/langserver/definition.go`

Given a position in a file, extract the traversal at the cursor and resolve it to a declaration.

**Traversal forms to support:**

| Pattern | Resolution |
|---|---|
| `next = step.<name>` | find `step "<name>"` declaration |
| `next = state.<name>` | find `state "<name>"` declaration |
| `next = switch.<name>` | find `switch "<name>"` declaration |
| `next = wait.<name>` | find `wait "<name>"` declaration |
| `next = approval.<name>` | find `approval "<name>"` declaration |
| `target = adapter.<type>.<name>` | find `adapter "<type>" "<name>"` declaration |
| `target = subworkflow.<name>` | find `subworkflow "<name>"` declaration |
| `var.<name>` | find `variable "<name>"` declaration |
| `local.<name>` | find `local "<name>"` declaration |
| `data.internal.<name>` | find `data "internal" "<name>"` declaration |
| `steps.<name>.*` | find `step "<name>"` declaration |

Approach: build a `*symbolIndex` (map from `(kind, name)` → `(file, line, col)`) by scanning the workflow directory with `symbols.go`. Then for a given position, extract the traversal from the raw HCL source line using the same regex approach as `definition.ts` (simpler than a full HCL parse at edit time).

### Step 3 — `--diag-json` flag on `criteria validate` (optional but recommended)

In `internal/cli/compile.go` (or `validate.go`), add `--diag-json` flag that emits diagnostics as JSON to stdout instead of formatted text to stderr. Use the HCL diagnostic structs which already carry file/line/col information:

```go
type diagJSON struct {
    Severity string `json:"severity"` // "error" or "warning"
    File     string `json:"file"`
    Line     int    `json:"line"`
    Col      int    `json:"col"`
    EndLine  int    `json:"end_line"`
    EndCol   int    `json:"end_col"`
    Summary  string `json:"summary"`
    Detail   string `json:"detail,omitempty"`
}
```

This is a small addition (<50 lines) that makes the langserver robust to any future changes in the human-readable diagnostic format.

### Step 4 — Tests

**`internal/langserver/symbols_test.go`:**
- Given a parsed `*workflow.Spec` with known blocks, `buildSymbols` returns the expected `[]DocumentSymbol` slice.

**`internal/langserver/definition_test.go`:**
- Given a symbol index and a position over `next = step.greet`, `resolveDefinition` returns the location of `step "greet"`.
- Given a position over `var.name`, resolves to `variable "name"`.
- Given a position over `return` (bare keyword), returns nil (no definition).

**`internal/langserver/server_test.go`:**
- `initialize` request returns the expected `ServerCapabilities`.
- `shutdown` followed by `exit` terminates the server loop with code 0.

### Step 5 — Editor configuration docs

Add `docs/editors/neovim.md` and `docs/editors/emacs.md` with minimal config snippets to wire `criteria langserver` into `nvim-lspconfig` and `eglot` respectively. This is documentation only — no code.

**Neovim (nvim-lspconfig):**
```lua
require('lspconfig.configs').criteria = {
  default_config = {
    cmd = { 'criteria', 'langserver' },
    filetypes = { 'hcl', 'criteria-hcl' },
    root_dir = require('lspconfig.util').root_pattern('*.chcl', '*.hcl'),
    settings = {},
  },
}
require('lspconfig').criteria.setup({})
```

**Emacs (eglot):**
```elisp
(add-to-list 'eglot-server-programs
             '((hcl-mode) "criteria" "langserver"))
```

## Out of scope

- **Standard tier** (completions, hover docs, rename) — deferred; the architecture supports adding these later.
- **Incremental sync** (`textDocument/didChange` with deltas) — server declares `change: 0` (None); clients send full text on change, which is fine for small workflow files.
- **Remote workspace support** — local filesystem only in v1.
- **VSCode integration via langserver** — the extension (WS10) continues to use its own subprocess approach until a future WS migrates it to use `criteria langserver` as its backend.

## Reuse pointers

- [`workflow.Parse`](../../workflow/parse.go) — existing parser; returns `*workflow.Spec` with full HCL source ranges.
- [`internal/cli/diags.go`](../../internal/cli/diags.go) — existing diagnostic formatting; reference for extracting `hcl.Diagnostics` into file/line/col structs.
- `src/diagnostics.ts` and `src/definition.ts` in `criteria-vscode-extension-v1` — reference implementations of the same logic in TypeScript.
- `go.lsp.dev/protocol` — LSP type definitions (`InitializeParams`, `ServerCapabilities`, `DocumentSymbol`, `Location`, etc.).

## Behavior change

**New command:** `criteria langserver` — starts an LSP server on stdin/stdout. No existing commands change.

## Tests required

- All unit tests in `internal/langserver/` pass.
- `--diag-json` flag on `criteria validate` emits valid JSON with correct line numbers (if implemented).
- Manual: configure Neovim with the snippet above; open `examples/phase3-fold/fold-demo.chcl`; confirm diagnostics appear on introduction of a syntax error; confirm go-to-definition works on `next = state.done`.
- `go vet ./...` clean.
- `make test` passes.

## Implementation Notes

### Checklist

- [ ] Step 1 — `internal/cli/langserver.go` + register in `main.go`
- [ ] Step 2a — `internal/langserver/server.go` (stdio loop, initialize/shutdown)
- [ ] Step 2b — `internal/langserver/diagnostics.go` (compile + publishDiagnostics)
- [ ] Step 2c — `internal/langserver/symbols.go` (documentSymbol)
- [ ] Step 2d — `internal/langserver/definition.go` (definition provider)
- [ ] Step 3 — `--diag-json` flag on `criteria validate` (optional)
- [ ] Step 4 — Unit tests for symbols, definition, server lifecycle
- [ ] Step 5 — `docs/editors/neovim.md` and `docs/editors/emacs.md`

### Reviewer Notes

_To be filled in during review._
