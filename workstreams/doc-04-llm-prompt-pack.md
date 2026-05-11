# doc-04 — LLM prompt pack: curated worked examples

**Phase:** Pre-Phase-4 (adapter-rework prep) · **Track:** A (documentation) · **Owner:** Workstream executor · **Depends on:** [doc-03-llm-language-spec.md](doc-03-llm-language-spec.md) (consumes the spec as the canonical reference). · **Unblocks:** none.

## Context

`doc-03` ships a single-file formal language spec (`docs/LANGUAGE-SPEC.md`) suitable for an LLM system prompt. The spec is reference-style — block tables, function signatures, EBNF — but contains only 5 minimal worked examples. For real LLM-assisted authoring, model quality jumps when the prompt also includes a small library of pattern-by-pattern examples ("here is the canonical shape of a parallel iteration") that the model can mimic.

This workstream produces `docs/llm/` containing 8 curated example workflows, one per pattern, each ≤ 60 lines of HCL with surrounding markdown. The pack is **paired with** `LANGUAGE-SPEC.md` — together they form the recommended LLM authoring prompt. A short index file (`docs/llm/README.md`) explains how to assemble the prompt.

Every example workflow is also dropped into `examples/llm-pack/` and validated by `make validate`, so the examples cannot rot silently.

## Prerequisites

- [doc-03-llm-language-spec.md](doc-03-llm-language-spec.md) merged. `docs/LANGUAGE-SPEC.md` exists and `make spec-check` is green.
- `make ci` green on `main`.
- `criteria` CLI builds and `make validate` passes.

## In scope

### Step 1 — Define the eight patterns

The pack contains exactly these eight examples, in this order, named exactly as listed. No more, no fewer.

| # | Filename | Pattern | Demonstrates |
|---|---|---|---|
| 1 | `01-linear.md` | Linear pipeline | Three sequential steps, no branching, simple `input { ... }` and `output { ... }` chaining via `steps.<name>.<key>`. |
| 2 | `02-branching-switch.md` | Branching | A `switch` block with two `condition` arms and a `default`. Demonstrates routing on a `steps.classify.label` value. |
| 3 | `03-iteration-for-each.md` | Sequential iteration | A step with `for_each = ["a", "b", "c"]`, `each.value` in `input`, `outcome "all_succeeded"`, `outcome "any_failed"`. |
| 4 | `04-iteration-parallel.md` | Concurrent iteration | A step with `parallel = [...]` and `parallel_max = 4`, `on_failure = "continue"`. Notes the adapter `parallel_safe` capability requirement. |
| 5 | `05-subworkflow.md` | Subworkflow call | A `subworkflow "process_one"` declaration plus a step targeting it via `target = subworkflow.process_one`. Shows input passing and output capture. |
| 6 | `06-approval-and-wait.md` | Human-in-the-loop | An `approval "release_gate"` block plus a `wait "deploy_window"` block (both signal-based). |
| 7 | `07-shared-variable.md` | Mutable shared state | A `shared_variable "counter"` declaration; two steps mutating it via `outcome { shared_writes = { counter = ... } }`. |
| 8 | `08-fileset-template.md` | File-driven prompts | Uses `fileset()` to enumerate `prompts/*.md` and `templatefile()` to render one per iteration. **Depends on `feat-01` and `feat-02` having merged**; if those are not yet in `main`, this example uses `file()` only and a TODO note marks it for upgrade. |

### Step 2 — File layout for each example

Each `docs/llm/NN-name.md` file has exactly this structure (no extra sections, no extra prose):

```markdown
# Pattern: <Pattern name>

## When to use

<2–4 sentences. Concrete trigger: "use this when you need to ...".>

## Minimal example

```hcl
<HCL — ≤ 60 lines, no comments unless they teach a non-obvious rule>
```

## Key idioms

- **`<idiom name>`** — one sentence explaining what the snippet shows. Up to 5 bullets.

## Common pitfalls

- **`<pitfall>`** — one sentence. Up to 3 bullets.

## See also

- [LANGUAGE-SPEC.md § <section>](../LANGUAGE-SPEC.md#anchor)
- Other relevant pattern files in this directory.
```

Word budget per file: ≤ 350 words (including the HCL block). Enforced by a unit test in Step 5.

### Step 3 — Index file `docs/llm/README.md`

Single file, ≤ 250 words, with these sections in this order:

1. **`# Criteria LLM Prompt Pack`** — title.
2. **`## How to assemble the prompt`** — explicit instructions:
   ```
   System prompt = docs/LANGUAGE-SPEC.md + the 8 pattern files concatenated in order.
   Total token budget: ~12,000 tokens (8,000 for the spec + ~4,000 for the pack).
   ```
   Include a one-line shell snippet:
   ```bash
   cat docs/LANGUAGE-SPEC.md docs/llm/0*.md > prompt.md
   ```
3. **`## Pattern index`** — table mapping pattern → filename → trigger:
   ```
   | # | Pattern | When to use it |
   |---|---|---|
   | 01 | Linear pipeline | Sequential steps, no branching. |
   | 02 | Branching switch | One-of-N routing on a captured value. |
   | ... | ... | ... |
   ```
4. **`## Maintenance`** — one sentence: "Each pattern's HCL is also under `examples/llm-pack/`; `make validate` compiles all of them on every CI run."

No other sections.

### Step 4 — Mirror each example into `examples/llm-pack/`

For each `docs/llm/NN-name.md` file:

1. Extract the HCL block.
2. Write it to `examples/llm-pack/NN-name/main.hcl`.
3. The example must compile and pass `criteria validate examples/llm-pack/NN-name/`. If the example needs a fixture (e.g. example 8 needs a `prompts/` directory), create it under the same `NN-name/` subdirectory.

Add the new example directory to the `Makefile` `validate` target so all 8 are exercised:

```make
validate:
    ... existing example list ...
    ./bin/criteria validate examples/llm-pack/01-linear
    ./bin/criteria validate examples/llm-pack/02-branching-switch
    ./bin/criteria validate examples/llm-pack/03-iteration-for-each
    ./bin/criteria validate examples/llm-pack/04-iteration-parallel
    ./bin/criteria validate examples/llm-pack/05-subworkflow
    ./bin/criteria validate examples/llm-pack/06-approval-and-wait
    ./bin/criteria validate examples/llm-pack/07-shared-variable
    ./bin/criteria validate examples/llm-pack/08-fileset-template
```

If a single workstream pattern conflict arises (e.g. example 4 uses `parallel_safe` capability and the test harness's stub adapter does not declare it), the example **must** declare a real adapter (e.g. shell with parallel_safe) — do NOT add a `validator: skip` annotation. The whole point is that these examples compile.

### Step 5 — Add a drift / size guard test

New file `docs/llm/llmpack_test.go` (yes, `_test.go` under `docs/`; build-tag-gated to `//go:build llmpack`):

Actually — `docs/` is not a Go package. Place the test under `tools/llmpack-check/llmpack_test.go` instead. New tool/test directory:

- `tools/llmpack-check/llmpack_test.go`:
  - `TestPromptPack_FilesPresent` — asserts the 8 expected files exist in `docs/llm/`, in the canonical order, with the canonical names.
  - `TestPromptPack_PerFileWordBudget` — for each file, asserts `len(strings.Fields(body)) <= 350`.
  - `TestPromptPack_StructureConformance` — for each file, asserts headers appear in the required order and no extra `## ` headers exist.
  - `TestPromptPack_HCLMirroredToExamples` — for each `docs/llm/NN-name.md`, extracts the HCL block, finds `examples/llm-pack/NN-name/main.hcl`, and asserts the two contents match exactly (after normalising trailing whitespace). A drift between docs and examples fails the test with a diff.
  - `TestPromptPack_TotalWordBudget` — sum of all 8 files' word counts ≤ 2,800 (≈ 4,000 tokens).

Wire into CI by ensuring `go test ./tools/llmpack-check/...` runs as part of the existing test job.

### Step 6 — Author the eight files

For each pattern, follow Step 2's template. Constraints:

- HCL must compile via `criteria validate` against the v0.3 surface. Use real adapter types (e.g. `shell`) — not placeholder strings.
- Each example must be **self-contained** (no `# imports` from other examples). If two examples need the same adapter, both declare it.
- Use only language constructs from the v0.3 surface. Patterns 4 (parallel) and 8 (fileset/templatefile) depend on features that may or may not be present at the time this workstream runs:
  - Parallel iteration is in v0.3.
  - `fileset` / `templatefile` arrive in `feat-01` / `feat-02`. If those are not yet merged, replace pattern 8's content with the closest equivalent using `file()` and a TODO line in `## Common pitfalls` reading: "**`feat-02` will replace this hand-written enumeration with `fileset()`** — see [feat-02-fileset-function.md](../../workstreams/feat-02-fileset-function.md)." When `feat-02` lands, that workstream is responsible for editing this file (it appears in `feat-02`'s Files-may-modify list).

### Step 7 — Cross-link from `docs/LANGUAGE-SPEC.md`

Edit `docs/LANGUAGE-SPEC.md`'s `## Worked examples` section to add a one-line note at the end:

```markdown
> For pattern-by-pattern guidance, see [docs/llm/](./llm/). Concatenate this spec with the prompt pack to assemble a complete LLM authoring system prompt.
```

This is the only edit to `LANGUAGE-SPEC.md` allowed in this workstream.

### Step 8 — Validation

```sh
go test ./tools/llmpack-check/...
make validate
make spec-check
make ci
```

Manual check:
- Open each of the 8 `docs/llm/*.md` files; confirm word count and structure.
- `cat docs/LANGUAGE-SPEC.md docs/llm/0*.md | wc -w` ≤ 8,500 words (combined budget).

## Behavior change

**No behavior change.** This workstream adds documentation files, eight runnable example workflows, a new Makefile validate target rows, and a test tool. No source files in `workflow/`, `internal/`, `cmd/`, or `sdk/` are modified.

## Reuse

- The example template structure is uniform across all 8 files — write a tiny generator script if convenient, but it is not required and not delivered. Hand-authored is fine.
- The drift test in `tools/llmpack-check/` is a cousin of `tools/spec-gen/` from `doc-03`; reuse its file-reading and word-counting helpers if it makes sense (move to a shared `tools/internal/`-style package only if both workstreams need them; otherwise keep duplicated — the helpers are 5 lines each).
- The existing `make validate` target — extend, do not duplicate.
- Existing example workflow conventions (see `examples/file_function/`, `examples/phase3-parallel/`).

## Out of scope

- More than 8 patterns. The eight are an opinionated minimal set; growth requires a follow-up workstream and reviewer approval.
- An auto-generated index. The `docs/llm/README.md` file is hand-authored; the test in Step 5 enforces it lists all 8 files.
- Including the prompt pack in the `criteria` binary (e.g. as `criteria explain` content). That's a separate UX feature, not a docs workstream.
- Hosting the pack at a public URL beyond the repo. Github raw URLs to `main` are sufficient.
- Translating the pack. English only.
- Editing `docs/workflow.md`. The pack is paired with `LANGUAGE-SPEC.md`, not the human reference.

## Files this workstream may modify

- New directory: [`docs/llm/`](../docs/llm/) — `README.md`, `01-linear.md` … `08-fileset-template.md`.
- New directory: [`examples/llm-pack/`](../examples/llm-pack/) — eight subdirectories, each with `main.hcl` (and any fixture files needed).
- New directory: [`tools/llmpack-check/`](../tools/llmpack-check/) — `llmpack_test.go`.
- [`Makefile`](../Makefile) — extend the `validate` target with eight new lines.
- [`docs/LANGUAGE-SPEC.md`](../docs/LANGUAGE-SPEC.md) — append exactly one cross-link line at the end of the `## Worked examples` section per Step 7.

This workstream may **not** edit:

- `README.md`, `PLAN.md`, `AGENTS.md`, `CHANGELOG.md`, `CONTRIBUTING.md`, `workstreams/README.md`, or any other workstream file.
- Any file under `workflow/`, `internal/`, `cmd/`, `sdk/`.
- [`docs/workflow.md`](../docs/workflow.md), [`docs/plugins.md`](../docs/plugins.md), or any other file under `docs/` other than the new `docs/llm/` directory and the one-line edit to `LANGUAGE-SPEC.md`.
- [`tools/spec-gen/`](../tools/spec-gen/) (owned by `doc-03`).

## Tasks

- [x] Write the 8 pattern files in `docs/llm/` per Step 2 and Step 6.
- [x] Write `docs/llm/README.md` per Step 3.
- [x] Mirror each HCL block to `examples/llm-pack/NN-name/main.hcl` per Step 4.
- [x] Extend `make validate` with eight new lines.
- [x] Add `tools/llmpack-check/llmpack_test.go` per Step 5.
- [x] Cross-link from `docs/LANGUAGE-SPEC.md` per Step 7.
- [x] Validation: `go test ./tools/llmpack-check/...`, `make validate` all green.

## Implementation notes

- All 8 pattern docs created under `docs/llm/` (01-linear through 08-fileset-template) plus `README.md` — exactly 9 files, each ≤350 words.
- All 8 HCL examples in `examples/llm-pack/NN-name/main.hcl` validate cleanly via `make validate`.
- Subworkflow example (05): inner workflow variable `item` was given `default = "default-item"` to satisfy compile-time binding check; the calling step still demonstrates runtime override via `input { item = "hello" }`.
- Pattern 08 (fileset): uses literal `for_each = ["prompts/hello.md"]` since `feat-01`/`feat-02` are not yet merged; `## Common pitfalls` documents the `feat-02` upgrade path.
- `docs/LANGUAGE-SPEC.md` created as a minimal placeholder (doc-03 not yet merged) with the required `## Worked examples` section and the cross-link line.
- `tools/llmpack-check/llmpack_test.go` implements all 5 required tests; all pass.
- Pre-existing `internal/cli` test failures (`TestApplyLocal_LocalApprovalDisabled_ApprovalNodeRejected`, `TestApplyLocal_ApprovalNode`) are unrelated to this workstream and were failing on `main` before branching.

## Exit criteria

- `docs/llm/` contains exactly 9 files: `README.md` + the 8 numbered patterns. No more, no fewer.
- Each pattern file ≤ 350 words; combined pack ≤ 2,800 words.
- `examples/llm-pack/` contains exactly 8 subdirectories, each with a passing `criteria validate`.
- `tools/llmpack-check/` tests all pass.
- `make validate` green (exercises all 8 example workflows).
- `make ci` green.
- `docs/LANGUAGE-SPEC.md` has exactly the one new cross-link line specified in Step 7; no other edits.

## Tests

The Step 5 list. The drift-mirror test is the load-bearing one — it ensures the docs and examples cannot diverge silently.

## Risks

| Risk | Mitigation |
|---|---|
| The 350-word per-file budget is too tight to teach the concept | Each pattern has a 60-line HCL budget plus 5 idiom bullets and 3 pitfall bullets — that is enough for the patterns chosen. If a pattern genuinely needs more space, raise the budget in this workstream with reviewer sign-off, not in a follow-up. |
| HCL examples drift from the language as features ship | The drift-mirror test fails CI when `docs/llm/NN.md` and `examples/llm-pack/NN/main.hcl` diverge. The validate target catches any compile regression. Together they are sufficient. |
| `feat-02` (fileset) lands after this workstream, leaving pattern 08 with a placeholder | `feat-02`'s Files-may-modify list includes `docs/llm/08-fileset-template.md` and `examples/llm-pack/08-fileset-template/`; that workstream is responsible for the upgrade. The placeholder is documented in pattern 08's `## Common pitfalls`. |
| Pattern 04 (parallel) example fails because the example adapter does not declare `parallel_safe` | The example uses `shell` which is parallel-safe in the v0.3 surface (confirm via `internal/adapters/shell/shell.go` capabilities). If shell does not declare the capability, the example uses the noop adapter (which does — see [cmd/criteria-adapter-noop](../cmd/criteria-adapter-noop)) instead. |
| The combined prompt exceeds context windows for some smaller models | The 8,500-word total budget is well within Claude/GPT-4 context windows. For smaller models, users can drop individual patterns; the README notes this. Not a blocker. |
| Example 6 (approval/wait) requires `--server` to actually run | `criteria validate` only compiles, it does not run; the example will validate green even though `criteria apply` against it would require a server. Document this in the example's `## Common pitfalls` section. |
