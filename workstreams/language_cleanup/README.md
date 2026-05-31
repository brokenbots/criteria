# Language Cleanup — Terraform-shaping the Workflow HCL

**Base branch:** `main` (workstreams land in `main` first, then `main` merges into `adapter-v2` after the language work is complete).

## Why

The VSCode extension surfaced several places where the workflow HCL has drifted away from Terraform conventions in ways that hurt ergonomics (e.g. `workflow "name" {}` instead of `workflow { name = ... }`, `type = "string"` strings instead of `type = string` type expressions, magic-string `next = "..."` instead of `next = step.foo` traversals, hand-rolled `shared_variable` instead of `data` blocks). The design goal for this language has always been "Terraform-shaped wherever possible" — same block grammar, same functions, same type expressions — both for user familiarity and for future tooling reuse (HCL editors, linters, autocomplete). This phase fixes the visible drift in one focused pass so it doesn't keep growing.

Migration strategy is **hard break with helpful errors** — same pattern as the v0.3.0 legacy-rejection in [parse_legacy_reject.go](../../workflow/parse_legacy_reject.go). No dual-support window.

## Workstreams

- **[WS01 — mechanical schema cleanup](WS01-mechanical-schema-cleanup.md)** — low-risk, mechanical changes that all touch [workflow/schema.go](../../workflow/schema.go) and so are bundled to avoid merge churn. Reshapes `workflow {}` and nests `policy` under it, replaces type strings with type expressions, replaces `default_outcome` attribute with an `outcome "default" {}` block, converts environment references from quoted strings to traversals, registers `cty/function/stdlib` for the full Terraform-style function set. VSCode grammar updated to match.

- **[WS02 — `data` block and outcome semantics](WS02-data-and-outcome-semantics.md)** — higher-risk semantic changes touching the engine runtime. Outcome `next` becomes a node traversal (`step.foo`, `state.done`) with bare `return`/`continue` keywords replacing magic strings. `shared_variable` is replaced by `data "internal" "name"` (extensible block, ready for future remote data sources). `shared_writes = { ... }` becomes per-target `write { target = ..., value = ... }` blocks inside outcomes. Engine runtime store renamed `SharedVarStore` → `DataStore`. VSCode grammar updated to match.

WS01 may land first to absorb the small mechanical churn; WS02 then lands on a clean schema.go.

WS03–WS06 are a second batch that lands on `main` before the final adapter-v2 rebase. They address engine correctness bugs, a switch syntax inconsistency, eval-context hardening, and CLI quality-of-life improvements:

- **[WS03 — engine bug trio](WS03-engine-bug-trio.md)** — three correctness bugs in the subworkflow execution path: null panic on unwritten `data "internal"` outputs, terminal state success/failure discarded, and stale DataStore snapshot causing output expressions to see pre-write values.

- **[WS04 — switch syntax rename](WS04-switch-syntax-rename.md)** — `condition { match = ... }` → `match { condition = ... }` to match how switch/case reads in mainstream languages. Hard break with migration message; all `.hcl` files migrated.

- **[WS05 — compiler hardening and eval extensions](WS05-compiler-hardening-eval-extensions.md)** — invalid step references are now `DiagError` instead of `DiagWarning`; adds `path.workflow`/`path.root`/`path.cwd` variables and `abspath()`/`dirname()`/`basename()` path functions; adds `hasattr()`, `can()`, and `try()` for runtime error handling.

- **[WS06 — `--var-file` and `.chcl` extension](WS06-var-file-and-chcl-extension.md)** — `--var-file` flag for loading variable overrides from a file; introduces `.chcl` as the criteria-native HCL extension recognized universally alongside `.hcl`.

WS03 and WS04 are independent of each other. WS05 and WS06 are each independent. All four can be developed and reviewed in parallel.

WS07–WS11 are the final batch, closing out the language_cleanup track with documentation alignment, LLM ergonomics, and editor tooling:

- **[WS07 — LANGUAGE-SPEC.md alignment](WS07-language-spec-alignment.md)** — fixes the hand-written sections of the normative spec (EBNF grammar, worked examples, switch prose note, file extension mention) to match the current language. Must land before WS10 (extension) and WS11 (LSP server) so the spec is authoritative.

- **[WS08 — workflow.md and README alignment](WS08-workflow-doc-alignment.md)** — fixes `docs/workflow.md` throughout: workflow header examples, variable type examples, inverted switch attribute names, subworkflow example (old nested format), directory mode `.chcl` mentions, `--var-file` documentation, and data block type examples. Also corrects the `README.md` quickstart version.

- **[WS09 — `criteria spec` command](WS09-criteria-spec-command.md)** — adds `criteria spec` (print the language spec) and `criteria spec --with-patterns` (print spec + all 8 LLM pattern files) for LLM-friendly access. Files are embedded at compile time with `go:embed`.

- **[WS10 — VSCode extension language sync](WS10-vscode-extension-language-sync.md)** — updates `criteria-vscode-extension-v1` for current language syntax (WS01–WS06 changes broke diagnostics, go-to-definition, and workspace index) and adds `.chcl` file extension support throughout.

- **[WS11 — `criteria langserver` (Minimal LSP)](WS11-criteria-langserver-minimal-lsp.md)** — adds `criteria langserver` subcommand that speaks LSP JSON-RPC over stdin/stdout, delivering diagnostics, document symbols, and go-to-definition to Neovim, Emacs, Zed, and any other LSP-capable editor. ~2–3 weeks effort.

Recommended order: WS07 first, then WS08 and WS09 in parallel, then WS10 (depends on WS07 for spec authority), then WS11 (depends on WS09 and WS10).

## Out of scope (this phase)

- Adapter v2 work — separate track on `adapter-v2` branch.
- New language features (loop primitives, error-handling blocks, etc.).
- LSP Standard tier (completions, hover docs, rename symbol) — deferred post WS11.

## References

- Design plan: `~/.claude/plans/now-that-we-have-eager-shore.md` (local).
- Existing legacy-rejection pattern: [workflow/parse_legacy_reject.go](../../workflow/parse_legacy_reject.go).
- Terraform type expressions: [hashicorp/hcl/v2/ext/typeexpr](https://pkg.go.dev/github.com/hashicorp/hcl/v2/ext/typeexpr).
- Terraform-equivalent functions: [zclconf/go-cty/cty/function/stdlib](https://pkg.go.dev/github.com/zclconf/go-cty/cty/function/stdlib).
