# doc-03 — Single-file formal language spec for LLMs

**Phase:** Pre-Phase-4 (adapter-rework prep) · **Track:** A (documentation) · **Owner:** Workstream executor · **Depends on:** none. · **Unblocks:** [doc-04](doc-04-llm-prompt-pack.md) (consumes the new spec as the canonical reference).

## Context

Today the canonical workflow language reference is [docs/workflow.md](../docs/workflow.md), ~1,250 lines of structured prose. It is excellent for human readers but unsuitable as an LLM system-prompt drop-in: too long, mixes prose and reference, and cannot be auto-checked against the schema for drift. Several internal experiments (LLM-assisted workflow authoring, copilot-driven HCL repair) all hit the same problem — the model needs a dense, complete, self-contained spec under ~8,000 tokens that lists every block, every attribute, every namespace, every function, and every outcome rule with no narrative noise.

This workstream produces `docs/LANGUAGE-SPEC.md` as the canonical machine-and-human reference. It is **hybrid**: a generator emits the reference tables (blocks, attributes, function signatures) from the schema and function-registration sources of truth; the surrounding prose (grammar, namespace semantics, outcome model, iteration semantics, error model, worked syntax examples) is hand-authored.

The generator and a CI drift check guarantee the reference tables stay in lockstep with [workflow/schema.go](../workflow/schema.go) and [workflow/eval_functions.go](../workflow/eval_functions.go). Subsequent feature workstreams (`feat-01..04`) extend the spec by editing the prose and re-running the generator; CI fails if any block kind defined in `schema.go` is missing from the spec.

## Prerequisites

- `make ci` green on `main`.
- `v0.3.0` shipped — `docs/workflow.md` reflects the v0.3 surface (W11/W12/W14/W15 closed).
- Local Go toolchain matches the version pinned in [go.mod](../go.mod).

## In scope

### Step 1 — Create the generator under `tools/spec-gen/`

New directory `tools/spec-gen/` containing:

- `tools/spec-gen/main.go` — `package main` entry point. CLI:
  ```
  spec-gen [-check] [-out docs/LANGUAGE-SPEC.md]
  ```
  - Default mode: regenerate `docs/LANGUAGE-SPEC.md` in place. Reads the **whole** existing file, replaces only the content **between matched marker pairs**, writes back.
  - `-check`: parse `docs/LANGUAGE-SPEC.md`, regenerate the marked sections in memory, compare; exit non-zero with a diff if they differ. Used by CI.
- `tools/spec-gen/extract.go` — schema/function extractors:
  - `extractBlocks() []BlockDoc` walks [workflow/schema.go](../workflow/schema.go) using `go/parser` + `go/ast` over the file at build time. Identifies struct types whose `hcl:` tags declare a block (label and body). Emits one `BlockDoc{Name, Labels, Attributes []AttrDoc, NestedBlocks []BlockDoc, SourceLine}`. Pulls doc-comments above each field as the human-readable description.
  - `extractFunctions() []FuncDoc` walks [workflow/eval_functions.go](../workflow/eval_functions.go), specifically the `workflowFunctions(opts FunctionOptions) map[string]function.Function` map literal at [workflow/eval_functions.go:96-104](../workflow/eval_functions.go#L96-L104). For each entry, finds the `function.New(&function.Spec{...})` literal and reads `Params`, `VarParam`, and `Type` to produce `FuncDoc{Name, Params []ParamDoc, ReturnType, SourceLine, Description}`.
- `tools/spec-gen/render.go` — markdown renderer producing the three managed sections (see Step 2).
- `tools/spec-gen/main_test.go` — unit tests for the extractors using a tiny synthetic source under `tools/spec-gen/testdata/` (a 30-line struct + a 20-line function map). Covers the happy path and the "unrecognised tag" / "missing description" failure modes.

The generator must NOT depend on the rest of the `criteria` module (no `import "github.com/brokenbots/criteria/workflow"`). It is a pure source-file analyser. This avoids a dependency cycle and lets the tool run before `go build ./...`.

### Step 2 — Define the three managed sections in `docs/LANGUAGE-SPEC.md`

The spec file uses HTML-comment markers to delimit generator-owned regions. Markers MUST be exactly:

```
<!-- BEGIN GENERATED:blocks -->
... rendered content ...
<!-- END GENERATED:blocks -->

<!-- BEGIN GENERATED:functions -->
... rendered content ...
<!-- END GENERATED:functions -->

<!-- BEGIN GENERATED:namespaces -->
... rendered content ...
<!-- END GENERATED:namespaces -->
```

Generator behavior:

- Read the file.
- For each marker pair, replace the body with freshly rendered content.
- Anything outside markers is preserved byte-for-byte.
- If a marker pair is missing, exit with a clear error listing the missing pair.
- If markers are nested or unbalanced, exit with an error.

The three managed sections render as follows:

**`BEGIN GENERATED:blocks`** — one heading per top-level block type (workflow, variable, local, shared_variable, environment, output, adapter, subworkflow, step, state, wait, approval, switch, policy, permissions). For each block:

```markdown
### `<block-keyword> "<label>" { ... }`

- **Source:** [`workflow/schema.go:LINE`](../workflow/schema.go#LLINE)
- **Labels:** `<label-name>` (or `<type>` `<name>` for two-label blocks).
- **Attributes:**

| Attribute | Type | Required | Description |
|---|---|---|---|
| `version` | string | yes | Schema version. Use "1". |
| ... | | | |

- **Nested blocks:** `outcome`, `input`, `config`, ... (each a link to its own subsection).
```

**`BEGIN GENERATED:functions`** — one row per function. Source line linked.

```markdown
| Function | Signature | Returns | Source | Description |
|---|---|---|---|---|
| `file` | `file(path: string)` | `string` | [eval_functions.go:106](../workflow/eval_functions.go#L106) | Reads the UTF-8 file at `path` (relative to workflow dir). Path-confined; size-capped. |
| `fileexists` | `fileexists(path: string)` | `bool` | [eval_functions.go:148](../workflow/eval_functions.go#L148) | ... |
| ... | | | | |
```

**`BEGIN GENERATED:namespaces`** — short table of evaluation-context namespaces. Sourced by scanning [workflow/eval.go](../workflow/eval.go) for the constants/keys passed into the eval context build:

```markdown
| Namespace | Available in | Description |
|---|---|---|
| `var.*` | all expressions | Read-only typed input variables. |
| `steps.<name>.<key>` | post-completion of `<name>` | Captured outputs from a prior step. |
| `each.value`/`each.key`/`each.index`/`each.first`/`each.last`/`each.total`/`each._prev` | iterating-step expressions only | See iteration semantics. |
| `local.*` | all expressions | Compile-time constants. |
| `shared.*` | all expressions; mutable via `shared_writes` | Runtime-mutable shared values. |
```

(Description text per namespace is hand-curated in the generator's source — these are stable and rarely change. The generator emits the table verbatim from a constant in `tools/spec-gen/render.go`.)

### Step 3 — Hand-author the prose sections of `docs/LANGUAGE-SPEC.md`

Outside the generated regions, the spec contains the following sections **in this exact order**. Targets are stated in tokens (rough cl100k_base, the GPT-4 family tokenizer; aim for ≤ 8,000 total).

1. **`# Criteria Workflow Language — Specification (v0.3)`** — title.
2. **`## Purpose & Audience`** — three sentences. (~80 tokens)
3. **`## File structure`** — single-file vs directory module; the workflow header rule. (~200 tokens)
4. **`## Grammar (EBNF-ish)`** — informal EBNF for the top-level structure. Hand-authored. Example shape:
   ```
   workflow_module    := workflow_block content_decl*
   workflow_block     := "workflow" STRING "{" workflow_attr* "}"
   content_decl       := variable_block | local_block | shared_var_block | environment_block
                       | output_block | adapter_block | subworkflow_block | step_block
                       | state_block | wait_block | approval_block | switch_block
                       | policy_block | permissions_block
   ```
   (~400 tokens)
5. **`## Blocks`** — the `BEGIN GENERATED:blocks` region. (~3,500 tokens after rendering — this is the bulk.)
6. **`## Expressions`** — namespace table (the `BEGIN GENERATED:namespaces` region) plus a short prose subsection on operator precedence and HCL string interpolation rules. (~400 tokens)
7. **`## Functions`** — the `BEGIN GENERATED:functions` region. (~600 tokens)
8. **`## Iteration semantics`** — for_each, count, parallel rules, aggregate outcomes (`all_succeeded`, `any_failed`), per-iteration outcome routing, `each.*` bindings, error semantics for `on_failure` (`continue`/`abort`/`ignore`). (~600 tokens)
9. **`## Outcome model`** — outcome blocks, `next` targeting, `output` projection, `shared_writes`, default-outcome rules, terminal-state routing. (~400 tokens)
10. **`## Error model`** — compile errors vs runtime errors, fatal-error propagation, `on_crash` semantics. (~300 tokens)
11. **`## Worked examples`** — exactly 5 minimal examples (linear, branching switch, for_each iteration, parallel iteration, subworkflow call). Each ≤ 25 lines of HCL. (~1,000 tokens)
12. **`## Versioning`** — single line: spec describes language `version = "1"`; behavior changes are documented per `v0.<minor>.0` release in CHANGELOG. (~50 tokens)

Prose must be tight and reference-style, not tutorial-style. No "you might want to" or "for example, imagine". Numbered rules where applicable. No emojis. No screenshots.

### Step 4 — Wire the generator into `make` and CI

Add to [Makefile](../Makefile):

```make
.PHONY: spec-gen spec-check
spec-gen:
	go run ./tools/spec-gen -out docs/LANGUAGE-SPEC.md

spec-check:
	go run ./tools/spec-gen -check -out docs/LANGUAGE-SPEC.md
```

Add `spec-check` to the existing `lint` target so it runs on every `make lint`/`make ci`:

```make
lint: lint-imports lint-go lint-baseline-check spec-check
```

Add `spec-check` as an explicit step in [.github/workflows/ci.yml](../.github/workflows/ci.yml) under the existing `lint` job (visible as a separate step in the CI log so a drift failure is obvious):

```yaml
- name: spec-check
  run: make spec-check
```

The CI step must fail with a non-zero exit and a unified diff when the spec is out of date. The generator's `-check` mode prints `-` / `+` lines using `diff.Diff` (Go stdlib via `golang.org/x/tools/internal/...` is not allowed; use a tiny inline line-by-line diff or `github.com/google/go-cmp/cmp` which is already a transitive dep).

### Step 5 — Author `docs/LANGUAGE-SPEC.md` and run the generator

1. Hand-write the 12 prose sections per Step 3, with marker pairs in place where the generated regions go.
2. Run `make spec-gen`. Confirm the file is valid markdown (passes `make lint` if a markdown lint exists, or visually).
3. Run `make spec-check`. Confirm exit 0.
4. Open the rendered spec in an editor and verify:
   - Total token count ≤ 8,000 (use `wc -w` × 1.4 as a rough proxy if no tokenizer is at hand; aim ≤ 5,700 words).
   - Every block kind listed in the [docs/workflow.md](../docs/workflow.md) reference has a corresponding heading in the generated `## Blocks` section.
   - Every function in [workflow/eval_functions.go](../workflow/eval_functions.go) `workflowFunctions` map appears in the `## Functions` table.
   - Every namespace in the `BuildEvalContext` keys appears in the namespace table.

### Step 6 — Add a token-budget guard

New file `tools/spec-gen/budget_test.go`:

```go
func TestSpecTokenBudget_UnderEightThousandWords(t *testing.T) {
    data, err := os.ReadFile("../../docs/LANGUAGE-SPEC.md")
    if err != nil { t.Fatal(err) }
    words := len(strings.Fields(string(data)))
    if words > 5700 {
        t.Fatalf("LANGUAGE-SPEC.md is %d words (~%d tokens); budget is 5700 words (~8000 tokens)",
            words, words*14/10)
    }
}
```

This runs as part of `go test ./tools/spec-gen/...` in CI. It is a soft cap on growth — the budget can be raised in a follow-up workstream if a future feature genuinely requires it, but the raise is a reviewable change.

### Step 7 — Validation

```sh
go test ./tools/spec-gen/...
make spec-check
make lint
make test
make ci
```

All five must exit 0. Inspect the rendered `docs/LANGUAGE-SPEC.md` and confirm:

- Word count under 5,700.
- All marker pairs present and balanced.
- No TODO / FIXME / XXX in the spec body.
- The `## Worked examples` HCL snippets all parse: copy each into a temporary file and run `criteria validate <file>` to confirm. (Optional sanity check; not gated by CI in this workstream — `feat-01..04` workstreams may add example files under `examples/` that ARE gated.)

## Behavior change

**No behavior change.** This workstream adds a generator tool, a docs file, a Makefile target, and a CI step. No source files in `workflow/`, `internal/`, or `cmd/` are modified. No HCL surface change. No CLI change. No new errors.

## Reuse

- `go/parser` and `go/ast` from the standard library — do NOT pull in any third-party AST framework.
- `text/template` for rendering the markdown tables.
- `github.com/google/go-cmp/cmp` (already a transitive dep — confirm before adding to `go.mod`) for the `-check` diff output.
- The existing `make lint` target — extend, do not duplicate.
- The existing CI lint job in [.github/workflows/ci.yml](../.github/workflows/ci.yml) — add a step, do not add a new job.
- HTML-comment marker convention: similar tooling exists in many Go projects (e.g. `gomarkdoc`); the format chosen here is intentionally minimal so no third-party tool is needed.

## Out of scope

- Updating [docs/workflow.md](../docs/workflow.md). The two files coexist: `docs/workflow.md` is the human reference, `docs/LANGUAGE-SPEC.md` is the LLM/machine reference. Cross-linking is allowed; rewriting workflow.md is not.
- Adding worked examples beyond the 5 stated in Step 3. The prompt-pack workstream (`doc-04`) owns example proliferation.
- Generating the spec into multiple files. One file is the deliverable.
- Extracting field-level descriptions from `docs/workflow.md`. Doc-comments on the schema structs are the source of truth; if a field has no doc-comment, the generator emits a placeholder `_(no description)_` and `make spec-check` does NOT fail. A follow-up workstream may tighten this to "every field must have a doc-comment".
- Internationalisation. English only.
- A web-rendered version. Plain markdown only.
- Modifying `workflow/schema.go` to add doc-comments where missing. That is a separate workstream (out of scope here; the placeholder is acceptable for the initial drop).

## Files this workstream may modify

- New directory: [`tools/spec-gen/`](../tools/spec-gen/) — `main.go`, `extract.go`, `render.go`, `main_test.go`, `budget_test.go`, `testdata/`.
- New file: [`docs/LANGUAGE-SPEC.md`](../docs/LANGUAGE-SPEC.md).
- [`Makefile`](../Makefile) — add `spec-gen` and `spec-check` targets; extend the `lint` target.
- [`.github/workflows/ci.yml`](../.github/workflows/ci.yml) — add the `spec-check` step under the existing `lint` job.
- [`go.mod`](../go.mod) and [`go.sum`](../go.sum) — only if `github.com/google/go-cmp/cmp` is not already a direct dep (most likely it is; if not, add it).

This workstream may **not** edit:

- `README.md`, `PLAN.md`, `AGENTS.md`, `CHANGELOG.md`, `CONTRIBUTING.md`, `workstreams/README.md`, or any other workstream file.
- Any file under `workflow/`, `internal/`, `cmd/`, `sdk/`.
- [`docs/workflow.md`](../docs/workflow.md), [`docs/plugins.md`](../docs/plugins.md), or anything else under `docs/` other than the new `LANGUAGE-SPEC.md`.
- Generated proto files.

## Tasks

- [ ] Create `tools/spec-gen/` with `main.go`, `extract.go`, `render.go` per Step 1.
- [ ] Add `main_test.go` covering both extractors against `testdata/` synthetic sources.
- [ ] Define the three marker pairs and renderer output per Step 2.
- [ ] Hand-author the 12 prose sections of `docs/LANGUAGE-SPEC.md` per Step 3.
- [ ] Add `spec-gen` and `spec-check` Makefile targets; wire `spec-check` into `lint` per Step 4.
- [ ] Add the `spec-check` step to the CI lint job per Step 4.
- [ ] Run `make spec-gen`; commit the generated content.
- [ ] Add `budget_test.go` per Step 6.
- [ ] Validation: `go test ./tools/spec-gen/...`, `make spec-check`, `make lint`, `make test`, `make ci` all green.

## Exit criteria

- `docs/LANGUAGE-SPEC.md` exists and is ≤ 5,700 words.
- `tools/spec-gen/` compiles and passes its own unit tests.
- `make spec-check` exits 0 on a clean tree.
- `make spec-check` exits non-zero with a unified diff when an attribute is added to a `*Spec` struct in `workflow/schema.go` and the spec is not regenerated. (Demonstrate this once during development, then revert; no permanent test fixture required.)
- The generated `## Blocks` section contains a heading for every block kind whose schema struct lives in `workflow/schema.go`.
- The generated `## Functions` section contains a row for every entry in the `workflowFunctions` map.
- `make ci` exits 0.
- No file outside the "may modify" list is changed.

## Tests

- `tools/spec-gen/main_test.go`:
  - `TestExtractBlocks_FromTestdata` — synthetic schema source under `testdata/schema_sample.go`; assert exact `BlockDoc` slice.
  - `TestExtractFunctions_FromTestdata` — synthetic function-registration source; assert exact `FuncDoc` slice.
  - `TestExtractBlocks_MissingDocComment_EmitsPlaceholder` — confirms the `_(no description)_` placeholder.
  - `TestRenderBlocks_Markdown_StableOutput` — golden file under `testdata/blocks.golden.md`.
  - `TestRenderFunctions_Markdown_StableOutput` — golden file under `testdata/functions.golden.md`.
  - `TestCheckMode_DetectsDrift` — write a copy of `LANGUAGE-SPEC.md` to a temp dir, edit one generated row, run the check; assert non-zero exit and the diff contains the edited line.
  - `TestMarkers_MissingPair_Errors` — feed a file with `BEGIN GENERATED:blocks` but no `END`; assert error message names the missing marker.
  - `TestMarkers_Unbalanced_Errors` — feed nested markers; assert error.
- `tools/spec-gen/budget_test.go`:
  - `TestSpecTokenBudget_UnderEightThousandWords` per Step 6.

## Risks

| Risk | Mitigation |
|---|---|
| The generator's AST walk misses an unusual struct shape (embedded struct, generic type) | The synthetic testdata covers the shapes actually used in `workflow/schema.go`. If `schema.go` adopts new shapes in a later workstream, that workstream extends the testdata before the generator change. The token-budget and drift-check tests catch any silent regression at the boundary. |
| Markdown table widths overflow when a field description is long | Description is one sentence per field per the convention in `workflow/schema.go` doc-comments. If a long description appears, the generator emits it on a single table cell and a markdown viewer wraps it; no rendering hazard. The `make spec-check` fails on whitespace drift, not wrap differences. |
| Hand-authored prose drifts as features ship | Subsequent feature workstreams (`feat-01..04`) explicitly include "update `docs/LANGUAGE-SPEC.md` prose" in their own Files-may-modify list. The drift check covers the generated tables; a `# kept:` style annotation is not needed for prose. |
| Token budget creeps over 8k as the language grows | The 5,700-word soft cap is the regression detector. If a future workstream needs more, it explicitly raises the constant with reviewer sign-off. |
| The generator's HCL-tag parser misclassifies a field with an unusual `hcl:"..."` tag | Add a unit test for each tag form in use in `workflow/schema.go` (label, body, optional, remain) under `testdata/`. The "no description" placeholder is the failure-mode escape hatch — output is wrong but not blocked. |
| The new `lint` dependency on `spec-check` slows down local `make lint` runs noticeably | The generator is a single Go binary doing AST parsing of two files plus markdown rendering. Local runs should be < 200ms. If it ever exceeds 1s, profile and fix; do not split into a separate target. |
