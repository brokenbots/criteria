# Language Cleanup — Terraform-shaping the Workflow HCL

**Base branch:** `main` (workstreams land in `main` first, then `main` merges into `adapter-v2` after the language work is complete).

## Why

The VSCode extension surfaced several places where the workflow HCL has drifted away from Terraform conventions in ways that hurt ergonomics (e.g. `workflow "name" {}` instead of `workflow { name = ... }`, `type = "string"` strings instead of `type = string` type expressions, magic-string `next = "..."` instead of `next = step.foo` traversals, hand-rolled `shared_variable` instead of `data` blocks). The design goal for this language has always been "Terraform-shaped wherever possible" — same block grammar, same functions, same type expressions — both for user familiarity and for future tooling reuse (HCL editors, linters, autocomplete). This phase fixes the visible drift in one focused pass so it doesn't keep growing.

Migration strategy is **hard break with helpful errors** — same pattern as the v0.3.0 legacy-rejection in [parse_legacy_reject.go](../../workflow/parse_legacy_reject.go). No dual-support window.

## Workstreams

- **[WS01 — mechanical schema cleanup](WS01-mechanical-schema-cleanup.md)** — low-risk, mechanical changes that all touch [workflow/schema.go](../../workflow/schema.go) and so are bundled to avoid merge churn. Reshapes `workflow {}` and nests `policy` under it, replaces type strings with type expressions, replaces `default_outcome` attribute with an `outcome "default" {}` block, converts environment references from quoted strings to traversals, registers `cty/function/stdlib` for the full Terraform-style function set. VSCode grammar updated to match.

- **[WS02 — `data` block and outcome semantics](WS02-data-and-outcome-semantics.md)** — higher-risk semantic changes touching the engine runtime. Outcome `next` becomes a node traversal (`step.foo`, `state.done`) with bare `return`/`continue` keywords replacing magic strings. `shared_variable` is replaced by `data "internal" "name"` (extensible block, ready for future remote data sources). `shared_writes = { ... }` becomes per-target `write { target = ..., value = ... }` blocks inside outcomes. Engine runtime store renamed `SharedVarStore` → `DataStore`. VSCode grammar updated to match.

WS01 may land first to absorb the small mechanical churn; WS02 then lands on a clean schema.go.

## Out of scope (this phase)

- Adapter v2 work — separate track on `adapter-v2` branch.
- New language features (loop primitives, error-handling blocks, etc.).
- Editor tooling beyond the existing TextMate grammar.

## References

- Design plan: `~/.claude/plans/now-that-we-have-eager-shore.md` (local).
- Existing legacy-rejection pattern: [workflow/parse_legacy_reject.go](../../workflow/parse_legacy_reject.go).
- Terraform type expressions: [hashicorp/hcl/v2/ext/typeexpr](https://pkg.go.dev/github.com/hashicorp/hcl/v2/ext/typeexpr).
- Terraform-equivalent functions: [zclconf/go-cty/cty/function/stdlib](https://pkg.go.dev/github.com/zclconf/go-cty/cty/function/stdlib).
