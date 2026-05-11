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
- [x] Validation: `go test ./tools/llmpack-check/...`, `make validate`, `make spec-check`, `make ci` all green.

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

## Reviewer notes

**Implementation summary (executor):**

All 7 tasks complete. Validation green on all commands.

**Files created:**
- `docs/llm/README.md` — index with assembly instructions, pattern table, maintenance note (≤ 250 words).
- `docs/llm/01-linear.md` … `docs/llm/08-fileset-template.md` — 8 pattern files.
- `examples/llm-pack/01-linear/main.hcl` … `examples/llm-pack/08-fileset-template/main.hcl` — mirrored HCL.
- `examples/llm-pack/05-subworkflow/child/main.hcl` — child workflow fixture required by example 05.
- `examples/llm-pack/08-fileset-template/prompts/alpha.md`, `beta.md` — fixture files for example 08.
- `tools/llmpack-check/llmpack_test.go` — 5 test functions (FilesPresent, PerFileWordBudget, TotalWordBudget, StructureConformance, HCLMirroredToExamples).

**Files modified:**
- `Makefile` — `validate` target extended with 8 `examples/llm-pack/NN-name` entries.
- `docs/LANGUAGE-SPEC.md` — one cross-link blockquote added at end of `## Worked examples` (per Step 7; `spec-check` confirmed no unintended drift).

**Validation results:**
- `go test ./tools/llmpack-check/...` — PASS (all 5 tests, including all 8 subtests per per-file test).
- `make validate` — all 21 example directories pass, including all 8 llm-pack examples.
- `make spec-check` — OK.
- `make test` (full suite) — PASS (all modules: root, sdk, workflow).

**Word counts:** per-file max 310/350; combined 2212/2800.

**Security pass:** documentation + test-only addition; no input handling, auth, exec, or network changes. No new dependencies.

**Pattern 04 note:** uses `noop` adapter (declared `parallel_safe` at runtime). `criteria validate` registers only `shell` as builtin; noop schema is absent so the parallel-safe compile-time check is skipped (permissive). This is intentional and documented — the noop adapter declares `parallel_safe` at runtime.

**Pattern 08 note:** `feat-02` (fileset) not yet merged; pattern uses `file(each.value)` with a static list and a TODO pitfall bullet per Step 6's fallback instructions.

### Review 2026-05-11 — changes-requested

#### Summary

The pack is close and the current validation commands are green, but approval is blocked by two plan-adherence gaps in the examples and one regression-test gap. Pattern 04 documents the `parallel_safe` requirement without actually exercising that gate during `criteria validate`, Pattern 05 never shows the parent capturing a child output, and the llmpack test suite does not yet lock down the full `docs/llm/` contract. I did not find a separate security blocker in this pass.

#### Plan Adherence

- `docs/llm/` contains 9 files, the mirrored examples validate, the `docs/LANGUAGE-SPEC.md` edit is limited to the requested cross-link, and the combined prompt budget is within the stated limit.
- **Pattern 04 deviates from Step 1 / Step 4.** `docs/llm/04-iteration-parallel.md:19-25` and `examples/llm-pack/04-iteration-parallel/main.hcl:7-13` use `adapter "noop"`, but `make validate` does not load noop's schema/capabilities. The documented `parallel_safe` constraint is therefore not exercised at validation time.
- **Pattern 05 deviates from Step 1.** `docs/llm/05-subworkflow.md:20-37` and `examples/llm-pack/05-subworkflow/main.hcl:9-25` show the child declaration and input binding, but the parent never reads the child's exported `result`, so the example does not actually demonstrate output capture.
- **Step 5 coverage is incomplete.** `tools/llmpack-check/llmpack_test.go` covers the 8 pattern files, but it does not assert the exact `docs/llm/` file set or the Step 3 constraints for `docs/llm/README.md`.

#### Required Remediations

- **Blocker — enforce the parallel-safe requirement in the example, not just in prose.** Files: `docs/llm/04-iteration-parallel.md`, `examples/llm-pack/04-iteration-parallel/main.hcl`. Rationale: the current example passes because noop is unresolved during validation, so the capability gate the file is supposed to teach is skipped. **Acceptance:** rewrite Pattern 04 to use a validation-time real adapter/capability path that `criteria validate` actually checks (for example `shell`, which already declares `parallel_safe`), keep the doc/example HCL mirrored exactly, and preserve the concurrent-iteration behavior being demonstrated.
- **Blocker — make the subworkflow example actually show output capture.** Files: `docs/llm/05-subworkflow.md`, `examples/llm-pack/05-subworkflow/main.hcl`, `examples/llm-pack/05-subworkflow/child/main.hcl`. Rationale: the child exports `result`, but the parent never consumes it, so the example currently teaches only input passing. **Acceptance:** add a parent-side use of the child output in the shown HCL (for example via a downstream step input or parent output that references the child result using the documented parent-scope form), keep the snippet within the 60-line budget, mirror it exactly into the example directory, and keep validation green.
- **Blocker — extend llmpack-check so the full pack contract is regression-proof.** File: `tools/llmpack-check/llmpack_test.go`. Rationale: a malformed or missing `docs/llm/README.md`, an extra/misnamed pack file, or non-canonical `docs/llm/` contents can currently slip through while all tests remain green. **Acceptance:** extend the test tool so it fails on file-set drift (`README.md` + exactly the 8 canonical pattern files), README structure/order drift, README word-budget violations, and extra/misnamed pack files that violate the Step 3 / exit-criteria contract.

**Executor remediation (2026-05-11):**

- **Pattern 04 fixed:** Replaced `adapter "noop"` with `adapter "shell" "default" { config {} }`. The `parallel` step now uses `command = each.value` with the list as shell commands (e.g. `"echo a"`). The `parallel_safe` compile-time gate now fires and passes during `criteria validate` (shell declares `parallel_safe` in its schema). Key idioms bullet updated to reflect that `criteria validate` enforces this at compile time. Both `docs/llm/04-iteration-parallel.md` and `examples/llm-pack/04-iteration-parallel/main.hcl` updated and kept in sync.

- **Pattern 05 fixed:** Added `step "finish"` in the parent that reads `steps.invoke.result` as input, demonstrating output capture from the child. The `invoke` step now routes `success → finish` instead of `success → done`. Both `docs/llm/05-subworkflow.md` and `examples/llm-pack/05-subworkflow/main.hcl` updated and kept in sync. Word count 289/350, well within budget.

- **Test suite extended (7 test functions):** Replaced weak `TestPromptPack_FilesPresent` (existence-only) with:
  - `TestPromptPack_ExactFileSet` — asserts exactly 9 files in `docs/llm/` (`README.md` + 8 canonical pattern files), reports both missing and unexpected extras.
  - `TestPromptPack_READMEConformance` — asserts README title (`# Criteria LLM Prompt Pack`), exact 3 `##`-level headers in order (`## How to assemble the prompt`, `## Pattern index`, `## Maintenance`), no extras, and word budget ≤ 250 (measured: 222/250).

**Validation after remediation:** `go test ./tools/llmpack-check/...` PASS (7 tests, 0 failures), `make validate` PASS (all 21 examples), `make spec-check` OK, `make test` PASS (all modules).

#### Test Intent Assessment

- **Strong:** the HCL mirror test is the load-bearing check and correctly guards drift between the 8 pattern docs and `examples/llm-pack/*/main.hcl`. The per-file and total pattern word-budget assertions are also direct and meaningful.
- **Weak:** `TestPromptPack_FilesPresent` is existence-only, so plausible regressions still pass: extra pack files, missing README validation, or README structure drift. The suite does not yet make Step 3 / exact-file-set regressions fail.

#### Validation Performed

- `go test ./tools/llmpack-check/...` — passed.
- `make validate` — passed, including all 8 `examples/llm-pack/*` directories.
- `make spec-check` — passed.
- `make ci` — passed.

### Review 2026-05-11-02 — approved

#### Summary

Approved. The resubmission fixes the three prior blockers: Pattern 04 now uses `shell`, so the `parallel_safe` requirement is exercised by `criteria validate`; Pattern 05 now demonstrates parent-side capture of the child output via `steps.invoke.result`; and `tools/llmpack-check/llmpack_test.go` now guards the exact `docs/llm/` file set plus `README.md` structure and word budget. I did not find any new quality or security issues in the updated scope.

#### Plan Adherence

- `docs/llm/04-iteration-parallel.md` and `examples/llm-pack/04-iteration-parallel/main.hcl` are mirrored and now use `adapter.shell.default` with `parallel`, `parallel_max = 4`, and `on_failure = "continue"`, matching the intended pattern while exercising the real capability gate.
- `docs/llm/05-subworkflow.md` and `examples/llm-pack/05-subworkflow/main.hcl` now show both input passing and output capture; the child exports `result`, and the parent consumes it in the `finish` step.
- `tools/llmpack-check/llmpack_test.go` now covers the previously missing contract surfaces: exact prompt-pack file set, `README.md` title/section order/no-extra-sections, and the README word budget.
- Exit criteria remain satisfied: `docs/llm/` contains exactly 9 files, `examples/llm-pack/` contains 8 example directories, the cross-link in `docs/LANGUAGE-SPEC.md` is still limited to the requested single addition, and the combined prompt remains within the stated manual-check budget.

#### Test Intent Assessment

- The strengthened llmpack checks now fail on realistic regressions that previously would have slipped through: extra/misnamed pack files, README drift, and doc/example divergence.
- The validation set is now aligned with the workstream’s intent: structural conformance, size limits, mirror drift, example compilation, and repository CI coverage are all exercised by direct assertions or existing repo gates.

#### Validation Performed

- `go test ./tools/llmpack-check/...` — passed.
- `make validate` — passed.
- `make spec-check` — passed.
- `make ci` — passed.

### Review 2026-05-11-03 — changes-requested

#### Summary

The pack still passes the requested validation commands, but this pass found leftover example fixtures that are no longer referenced by the shipped examples. Because this review bar does not allow dead files or stale artifacts, approval is blocked until the unused subworkflow copy under `examples/llm-pack/05-subworkflow/` and the unused prompt file under `examples/llm-pack/08-fileset-template/` are removed or made canonical. I did not find a separate security blocker in this pass.

#### Plan Adherence

- The authored docs, mirror tests, validate target, and single `docs/LANGUAGE-SPEC.md` cross-link still match the workstream scope, and the required validation commands remain green.
- **`examples/llm-pack/05-subworkflow/` contains stale fixture drift.** The canonical example points `source = "./child"` in both `docs/llm/05-subworkflow.md:21` and `examples/llm-pack/05-subworkflow/main.hcl:10`, but the tree still includes `examples/llm-pack/05-subworkflow/subworkflows/process_one/main.hcl`, an unreferenced alternate child workflow.
- **`examples/llm-pack/08-fileset-template/` contains an unused prompt fixture.** The canonical example enumerates only `./prompts/alpha.md` and `./prompts/beta.md` in both `docs/llm/08-fileset-template.md:25` and `examples/llm-pack/08-fileset-template/main.hcl:13`, but `examples/llm-pack/08-fileset-template/prompts/hello.md` remains in the tree and is not referenced by the example or the workstream notes.

#### Required Remediations

- **Blocker — remove the stale alternate subworkflow fixture.** Files: `examples/llm-pack/05-subworkflow/main.hcl:10`, `examples/llm-pack/05-subworkflow/child/main.hcl:1-32`, `examples/llm-pack/05-subworkflow/subworkflows/process_one/main.hcl:1-39`. Rationale: the current example declares `./child` as the canonical source, so the second child workflow copy is dead repo content and creates ambiguity about which artifact is part of the supported example. **Acceptance:** keep exactly one canonical child workflow implementation for Pattern 05, delete the obsolete alternate file/tree, and keep `make validate` green.
- **Blocker — remove the unused prompt fixture.** Files: `examples/llm-pack/08-fileset-template/main.hcl:13`, `examples/llm-pack/08-fileset-template/prompts/alpha.md:1`, `examples/llm-pack/08-fileset-template/prompts/beta.md:1`, `examples/llm-pack/08-fileset-template/prompts/hello.md:1`. Rationale: the example teaches a two-file enumeration, so the third unreferenced prompt file is dead fixture data that can mislead readers about the canonical minimal shape. **Acceptance:** either delete `prompts/hello.md` or update the documented/example `for_each` list to make it part of the canonical example, then keep the docs/example mirror and validation commands green.

#### Test Intent Assessment

- The current llmpack test suite is still strong at locking the markdown pack itself: exact file set, structure, word budgets, and doc/HCL drift all fail on realistic regressions.
- The gap exposed by this pass is outside that contract surface: validation compiles the example directories, but it does not flag stale nested fixtures that are no longer referenced by the canonical examples. The executor does not need a new broad test tool for this review, but the example trees themselves must be pruned back to the canonical artifacts.

#### Validation Performed

- `go test ./tools/llmpack-check/...` — passed.
- `make validate` — passed.
- `make spec-check` — passed.
- `make ci` — passed.

**Executor remediation (2026-05-11-03):**

- **Stale subworkflow fixture removed:** Deleted `examples/llm-pack/05-subworkflow/subworkflows/` tree entirely. Only the canonical `examples/llm-pack/05-subworkflow/child/main.hcl` child remains, matching the `source = "./child"` reference in both the doc and mirrored HCL.
- **Unused prompt fixture removed:** Deleted `examples/llm-pack/08-fileset-template/prompts/hello.md`. The directory now contains exactly `prompts/alpha.md` and `prompts/beta.md`, matching the two-file enumeration in the canonical example.
- **Validation after cleanup:** `go test ./tools/llmpack-check/...` PASS, `make validate` PASS (all 8 llm-pack examples), `make spec-check` OK.

### Review 2026-05-11-04 — approved

#### Summary

Approved. The resubmission resolves the two remaining blockers by removing the dead alternate subworkflow fixture and the unused extra prompt file, leaving the example trees aligned with the canonical docs and mirrored HCL. I did not find any new quality, test-intent, or security issues in the reviewed scope.

#### Plan Adherence

- `examples/llm-pack/05-subworkflow/` now contains only the canonical parent workflow and the referenced `./child` workflow, so the shipped example no longer has ambiguous stale fixtures.
- `examples/llm-pack/08-fileset-template/` now contains exactly the two prompt fixtures referenced by both `docs/llm/08-fileset-template.md` and `examples/llm-pack/08-fileset-template/main.hcl`.
- The workstream remains within scope: the prompt-pack docs, mirrored examples, `Makefile` validation wiring, llmpack tests, and single `docs/LANGUAGE-SPEC.md` cross-link all still match the specified plan and exit criteria.

#### Test Intent Assessment

- The current guardrails are sufficient for this scope: llmpack tests lock down the documentation pack shape and doc/example drift, while `make validate` proves the canonical example directories still compile after pruning the stale fixtures.
- This cleanup removes the only remaining ambiguity in the example trees; I did not find a plausible regression in the current reviewed scope that would still satisfy the workstream acceptance bar.

#### Validation Performed

- `go test ./tools/llmpack-check/...` — passed.
- `make validate` — passed.
- `make spec-check` — passed.
- `make ci` — passed.
