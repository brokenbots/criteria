# WS07 — `LANGUAGE-SPEC.md` alignment

**Phase:** Language Cleanup · **Track:** Documentation · **Owner:** Workstream executor · **Depends on:** WS01–WS06 (all landed). · **Unblocks:** WS10 (extension), WS11 (LSP server), adapter documentation refresh. · **Base branch:** `main`

## Context

`docs/LANGUAGE-SPEC.md` is the normative grammar reference — the document editor plugins, LLMs, and adapter authors consult to understand every block type, every attribute, and every expression rule. Its auto-generated sections (block tables, namespace bindings, function table) are kept current by `make spec-gen`. The hand-written sections — the EBNF grammar, the worked examples, and the prose notes — were not updated as WS01–WS06 landed, so they still describe the old language.

The mismatches are:
- **EBNF** still uses the pre-WS01 `workflow "name" {}` label form and the pre-WS04 `condition { match = … }` switch syntax.
- **Worked examples** (five of them) use `workflow "name" {}`, quoted type strings (`type = "string"`), and one switch example has `next = "deploy_prod"` (old quoted routing) instead of `next = state.deploy_prod`.
- **Prose notes** for the `switch` block invert the WS04-renamed block/attribute names.
- **File structure section** omits `.chcl` as an accepted extension (WS06).

## Prerequisites

- WS01–WS06 merged to `main`.

## In scope

### Step 1 — EBNF grammar (lines 20–57)

Rewrite the stale grammar rules. The grammar is hand-written, not generated.

**`workflow_block` and `workflow_attr`:**
```ebnf
# BEFORE:
workflow_block   := "workflow" STRING "{" workflow_attr* "}"
workflow_attr    := "version" "=" STRING
                  | "initial_state" "=" STRING
                  | "target_state" "=" STRING
                  | "environment" "=" STRING

# AFTER:
workflow_block   := "workflow" "{" workflow_attr* "}"
workflow_attr    := "name" "=" STRING
                  | "version" "=" STRING
                  | "initial_state" "=" STRING
                  | "target_state" "=" STRING
                  | "environment" "=" traversal
                  | policy_block
```

**`switch_block` and child blocks (WS04 rename):**
```ebnf
# BEFORE:
switch_block     := "switch" STRING "{" condition_block* default_block? "}"
condition_block  := "condition" "{" "match" "=" expr "next" "=" traversal "}"

# AFTER:
switch_block     := "switch" STRING "{" match_block* default_block? "}"
match_block      := "match" "{" "condition" "=" expr "next" "=" traversal ("output" "=" expr)? "}"
```

The `default_block` rule and all other rules are correct; leave them unchanged.

Also add `traversal` to the `expr` production to make clear it is a valid expression form:
```ebnf
# BEFORE:
expr             := STRING | NUMBER | BOOL | hcl_template | traversal
                  | func_call | binary_op | unary_op | tuple | object

# AFTER (no change needed — traversal is already listed)
```

### Step 2 — Switch prose note

In the **Notes on specific blocks** section, find the `switch` entry (currently "Conditional routing. `condition` sub-blocks are evaluated in declaration order; the first truthy `match` expression wins.") and rewrite it:

> **`switch`** — Conditional routing. `match` sub-blocks are evaluated in declaration order; the first truthy `condition` expression wins. `default` is the fallback; absence without an exhaustive condition set produces a runtime error.

### Step 3 — Worked examples

Five worked examples live in `## Worked examples`. Update each to use canonical current syntax.

**Example 1 (linear):**
- `workflow "greet" { version = "1" }` → `workflow { name = "greet"  version = "1" }`

**Example 2 (branching switch):**
- `workflow "branch" { version = "1" }` → `workflow { name = "branch"  version = "1" }`
- `variable "env" { type = "string" }` → `variable "env" { type = string }`
- Switch arm: `next  = "deploy_prod"` → `next = state.deploy_prod`
- Default: `next = state.deploy_dev` (already traversal — keep as-is)

**Example 3 (for_each):**
- `workflow "batch" { version = "1" }` → `workflow { name = "batch"  version = "1" }`
- `variable "items" { type = "list(string)" }` → `variable "items" { type = list(string) }`

**Example 4 (parallel):**
- `workflow "parallel" { version = "1" }` → `workflow { name = "parallel"  version = "1" }`
- `variable "ids" { type = "list(string)" }` → `variable "ids" { type = list(string) }`

**Example 5 (subworkflow):**
- `workflow "orchestrate" { version = "1" }` → `workflow { name = "orchestrate"  version = "1" }`

### Step 4 — File structure section

In `## File structure`, update the single-file and directory-module descriptions to mention `.chcl`:

> A workflow module is either:
> 1. **Single-file:** one `.chcl` or `.hcl` file containing all declarations.
> 2. **Directory module:** a directory of `.chcl` and/or `.hcl` files; exactly one must contain a `workflow` header block. All files are merged before compilation.
>
> File names are arbitrary; the `.chcl` extension is preferred for new files (criteria-native tooling uses it for file-type association); `.hcl` is accepted for compatibility.

Also update the encoding note: "File names are arbitrary; the `.hcl` extension is required." → remove the "`.hcl` extension is required" sentence (it is no longer accurate) and replace with the text above.

### Step 5 — Run spec-gen

After editing, run `make spec-gen` to regenerate the auto-generated sections. This will pick up any WS05-registered functions (`abspath`, `dirname`, `basename`, `hasattr`, `can`, `try`) if they were added to `workflow/eval_functions.go`. If they appear in the regenerated table, verify they are also mentioned in the **Function notes** section; add brief notes if missing. If they do not appear, no further action is needed.

### Step 6 — Validate

Run `make validate-docs` to confirm all five worked examples compile cleanly.

## Out of scope

- Changes to `docs/workflow.md` — covered by WS08.
- Changes to `docs/llm/*.md` — those files are current.
- Adding new language features or new examples.

## Reuse pointers

- `make spec-gen` (see `Makefile`) — regenerates `<!-- BEGIN GENERATED:* -->` sections from `workflow/schema.go` and `workflow/eval_functions.go`.
- `make validate-docs` — extracts HCL fenced blocks from `docs/*.md` and validates each with `bin/criteria validate`.
- Reference example files in `examples/` for canonical current syntax: `examples/phase3-fold/fold-demo.hcl`, `examples/phase3-multi-file/workflow.hcl`.

## Behavior change

Documentation only. No code change. No user-visible behavior change.

## Tests required

- `make validate-docs` passes (all five worked examples validate cleanly).
- `make spec-gen` produces no diff after the run (i.e. the auto-generated sections were already current, or the re-run picks up legitimate WS05 additions).

## Implementation Notes

### Checklist

- [ ] Step 1 — EBNF grammar updated (`workflow_block`, `workflow_attr`, `switch_block`, `match_block`)
- [ ] Step 2 — Switch prose note rewritten
- [ ] Step 3 — All five worked examples updated (workflow header, type expressions, switch arm `next`)
- [ ] Step 4 — File structure section updated (`.chcl` mentioned)
- [ ] Step 5 — `make spec-gen` run; new functions noted if present
- [ ] Step 6 — `make validate-docs` passes

### Reviewer Notes

_To be filled in during review._
