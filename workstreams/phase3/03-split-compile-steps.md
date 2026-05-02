# Workstream 03 — Split `workflow/compile_steps.go` along step-kind lines

**Phase:** 3 · **Track:** A · **Owner:** Workstream executor · **Depends on:** [01](01-lint-baseline-burndown.md) (the `gocognit`/`gocyclo`/`funlen` baseline entries on `compileSteps` are deferred to this workstream — must run after 01's cap drop). · **Unblocks:** every Track B and C workstream that adds new step shapes (universal target, return outcome, switch/if, parallel modifier, return-to-caller). The 622-LOC monolith is the worst place to land them.

## Context

[workflow/compile_steps.go](../../workflow/compile_steps.go) is 622 LOC and houses every step-kind compiler in one file. Per the function inventory:

| Function | Line | Responsibility |
|---|---:|---|
| `compileSteps` | 31 | Top-level dispatcher, walks every `StepSpec` and routes by step type |
| `compileWorkflowBody` | 325 | Dispatcher between inline and `workflow_file` body forms |
| `compileWorkflowBodyFromFile` | 350 | Loads child workflow Spec via `SubWorkflowResolver` |
| `compileWorkflowBodyInline` | 394 | Compiles inline child body via `WorkflowBodySpec` |
| `validateBodyHasContinuePath` | 433 | Reachability check on child body |
| `buildBodySpec` | 450 | Synthesizes a child `Spec` from `WorkflowBodySpec` (the asymmetry [B2](08-schema-unification.md) deletes) |
| `allowToolsForStep` | 503 | Adapter tool-allowlist projection |
| `warnBackEdges` | 519 | Loop-detection diagnostic pass |
| `nodeTargets` | 553 | Graph traversal helper |
| `stepHasBackEdge` | 595 | Cycle detection on a single step |

[TECH_EVALUATION-20260501-01.md](../../tech_evaluations/TECH_EVALUATION-20260501-01.md) §2 explicitly recommends decomposing `compileSteps` into step-kind specific compilers. Track B and C of Phase 3 add four new step-shape concerns:

- Universal step `target` (replaces step-kind dispatch) — [14](14-universal-step-target.md).
- `outcome` block + reserved `return` — [15](15-outcome-block-and-return.md).
- `switch`/`if` flow control — [16](16-switch-and-if-flow-control.md), which deletes the branch-block path entirely.
- `parallel` modifier — [19](19-parallel-step-modifier.md).

Landing those into a 622-LOC file is hostile to review and to the next contributor. Split first.

## Prerequisites

- [01](01-lint-baseline-burndown.md) merged: lint cap dropped to ≤ 50; complexity entries on `compileSteps`/`compileWaits`/`compileBranches`/`compileForEachs` still in baseline (this workstream removes them by removing the function complexity).
- `make ci` green on `main`.

## In scope

### Step 1 — Establish the new file layout

The split is **by step kind**, not by responsibility class. Each new file contains the full compile flow for one step kind:

| New file | Responsibility | Functions to move |
|---|---|---|
| `workflow/compile_steps.go` (kept, slimmed) | Top-level dispatcher only — `compileSteps` walks `spec.Steps` and routes per kind | `compileSteps` (slim it down to the dispatch loop only) |
| `workflow/compile_steps_adapter.go` | Adapter step compile (the `agent`/`adapter`-targeted step kind) | Adapter-specific compile branches extracted from `compileSteps` body; `allowToolsForStep` |
| `workflow/compile_steps_workflow.go` | `workflow`-typed step compile (the inline + `workflow_file` body case) | `compileWorkflowBody`, `compileWorkflowBodyFromFile`, `compileWorkflowBodyInline`, `validateBodyHasContinuePath`, `buildBodySpec` |
| `workflow/compile_steps_iteration.go` | `for_each` / `count` modifier handling | The iteration-binding compile branches extracted from `compileSteps` |
| `workflow/compile_steps_graph.go` | Graph helpers used by every step-kind compiler | `warnBackEdges`, `nodeTargets`, `stepHasBackEdge` |

The `compileSteps` function in [compile_steps.go](../../workflow/compile_steps.go) becomes a thin dispatcher (~50 LOC):

```go
func compileSteps(g *FSMGraph, spec *Spec, schemas map[string]AdapterInfo, opts CompileOpts) hcl.Diagnostics {
    var diags hcl.Diagnostics
    for i := range spec.Steps {
        sp := &spec.Steps[i]
        switch {
        case sp.WorkflowBody != nil || sp.WorkflowFile != "":
            diags = append(diags, compileWorkflowStep(g, sp, schemas, opts)...)
        case sp.ForEach != nil || sp.Count != nil:
            diags = append(diags, compileIteratingStep(g, sp, schemas, opts)...)
        default:
            diags = append(diags, compileAdapterStep(g, sp, schemas, opts)...)
        }
    }
    diags = append(diags, warnBackEdges(g)...)
    return diags
}
```

Names `compileWorkflowStep`, `compileIteratingStep`, `compileAdapterStep` are the new per-kind compilers extracted from the current `compileSteps` body. Pick those exact names — they are shorter than the full `compile_steps_<kind>.go` filename and read cleanly at the call site.

### Step 2 — Extract per-kind compile bodies

Walk the current `compileSteps` body (lines 31–323) and identify the per-kind branches. Each branch becomes a new function with the signature:

```go
func compileAdapterStep(g *FSMGraph, sp *StepSpec, schemas map[string]AdapterInfo, opts CompileOpts) hcl.Diagnostics
func compileWorkflowStep(g *FSMGraph, sp *StepSpec, schemas map[string]AdapterInfo, opts CompileOpts) hcl.Diagnostics
func compileIteratingStep(g *FSMGraph, sp *StepSpec, schemas map[string]AdapterInfo, opts CompileOpts) hcl.Diagnostics
```

If a code path in `compileSteps` is shared across two kinds (e.g. outcome wiring), promote it to a private helper inside the most-relevant new file (or inside `compile_steps_graph.go` if it touches `FSMGraph` shape).

**Do not** modify any compile logic during this carve. Bug fixes, validation broadening, and behavior changes belong to siblings ([07](07-local-block-and-fold-pass.md), [14](14-universal-step-target.md), [15](15-outcome-block-and-return.md), etc.). This workstream is **pure motion**.

### Step 3 — Preserve the `WorkflowBodySpec` path intact

[B2 (08)](08-schema-unification.md) deletes `WorkflowBodySpec` and `buildBodySpec`. Until 08 merges, this workstream **keeps the function alive** in [compile_steps_workflow.go](../../workflow/compile_steps_workflow.go) — same signature, same body. 08 will then delete it cleanly from a known-isolated file rather than from a 622-LOC monolith. That is the entire point of this workstream's sequencing.

### Step 4 — Update intra-package callers

Functions in `package workflow` that reference the moved symbols continue to work without import changes. Run:

```sh
go build ./workflow/...
```

If a build error surfaces, a moved function referenced an unexported helper that did not move — move the helper to the most-relevant new file.

### Step 5 — Move tests adjacent to the moved code

Tests in [workflow/compile_steps_test.go](../../workflow/compile_steps_test.go) (and any `compile_*_test.go` siblings) cover the current monolith. Inventory:

```sh
grep -ln 'compileSteps\|compileWorkflowBody\|buildBodySpec\|warnBackEdges\|nodeTargets\|stepHasBackEdge' workflow/*_test.go
```

For each test:

- If it tests a single kind (`TestCompileWorkflowStep_*`), move to the matching `compile_steps_<kind>_test.go`.
- If it tests dispatch (`TestCompileSteps_*`), keep in [compile_steps_test.go](../../workflow/compile_steps_test.go).
- If it tests graph helpers (`TestWarnBackEdges_*`), move to `compile_steps_graph_test.go`.

**Never rename a test function.** Test names are stable CI identifiers.

### Step 6 — Validation

```sh
go build ./workflow/...
go test -race -count=2 ./workflow/...
make lint-go
make lint-baseline-check
make ci
```

All exit 0. The baseline entries on `compileSteps` (`gocognit`, `gocyclo`, `funlen`) **must drop** because the function is now thin. **Remove the corresponding lines from [`.golangci.baseline.yml`](../../.golangci.baseline.yml)** — leaving them stale violates the cap-stays-flat contract from [01](01-lint-baseline-burndown.md). Re-measure cap.txt and lower if the count dropped further.

If new findings appear on the extracted functions, prefer extracting an obvious helper (e.g. a 30-line lookup loop becomes its own function) rather than adding a baseline entry. Pure code motion + obvious extracts only.

### Step 7 — Snapshot LOC delta

```sh
wc -l workflow/compile_steps.go workflow/compile_steps_*.go
```

Document in reviewer notes:

- Before: `compile_steps.go` 622 LOC.
- After: `compile_steps.go` ≤ 100 LOC; four siblings each ≤ 200 LOC.

If any sibling crosses 250 LOC, the carve is too coarse — re-split before submitting.

## Behavior change

**No behavior change.** Pure code motion + obvious extracts. The signal:

- Existing `make test ./workflow/...` covers all paths.
- Compile golden files in [internal/cli/testdata/compile/](../../internal/cli/testdata/compile/) lock in the compile output.
- `make validate` for every example HCL runs against the moved code.

If any test fails, the carve was not pure — investigate the function that pulled in implicit state and fix the move.

## Reuse

- Same naming pattern as [02](02-split-cli-apply.md) (`<base>_<concern>.go`).
- Existing test infrastructure under [workflow/](../../workflow/).
- Lint baseline tooling — do not reimplement.

## Out of scope

- Deleting `WorkflowBodySpec` / `buildBodySpec` (Phase 3 [08](08-schema-unification.md) handles this).
- Wiring `SubWorkflowResolver` into the CLI (Phase 3 [13](13-subworkflow-block-and-resolver.md)).
- Adding new step kinds (every Track B/C workstream that does this lands AFTER this split).
- Changing any compile validation, error messages, or diagnostic positions.
- Renaming any function.

## Files this workstream may modify

- [`workflow/compile_steps.go`](../../workflow/compile_steps.go) — reduce to ≤ 100 LOC.
- `workflow/compile_steps_adapter.go` — new.
- `workflow/compile_steps_workflow.go` — new.
- `workflow/compile_steps_iteration.go` — new.
- `workflow/compile_steps_graph.go` — new.
- `workflow/compile_steps_*_test.go` files — only to move test functions, never to rename.
- [`.golangci.baseline.yml`](../../.golangci.baseline.yml) — only to remove now-stale entries on `compileSteps`/`compileWaits`/`compileBranches`/`compileForEachs`. **Never add entries.**
- [`tools/lint-baseline/cap.txt`](../../tools/lint-baseline/cap.txt) — lower the cap to the new measured count.
- [`docs/contributing/lint-baseline.md`](../../docs/contributing/lint-baseline.md) — append a Phase 3 W03 note recording the cap drop.

This workstream may **not** edit:

- `PLAN.md`, `README.md`, `AGENTS.md`, `CHANGELOG.md`, `workstreams/README.md`, or any other workstream file.
- Anything outside `workflow/` (the carve is intra-package).
- [`.golangci.yml`](../../.golangci.yml).
- Generated files.

## Tasks

- [ ] Carve [compile_steps.go](../../workflow/compile_steps.go) into the five files per Step 1.
- [ ] Extract per-kind compile functions per Step 2.
- [ ] Preserve `WorkflowBodySpec` path intact for [08](08-schema-unification.md) (Step 3).
- [ ] `go build ./workflow/...` clean (Step 4).
- [ ] Move test functions adjacent to their target functions (Step 5).
- [ ] Remove now-stale complexity baseline entries on the moved functions and lower `cap.txt` (Step 6).
- [ ] `go test -race -count=2 ./workflow/...` green.
- [ ] `make lint-go`, `make lint-baseline-check`, `make ci` green.
- [ ] Snapshot LOC before/after in reviewer notes.

## Exit criteria

- [`workflow/compile_steps.go`](../../workflow/compile_steps.go) ≤ 100 LOC.
- Four new sibling files exist per Step 1 layout, each ≤ 250 LOC.
- Stale complexity entries on `compileSteps`/`compileWaits`/`compileBranches`/`compileForEachs` removed from [`.golangci.baseline.yml`](../../.golangci.baseline.yml).
- `cap.txt` lowered to the new measured count.
- `WorkflowBodySpec` and `buildBodySpec` still exist (deferred to [08](08-schema-unification.md)).
- All tests pass on `-race -count=2`.
- `make validate` passes for every example HCL.
- `make ci` exits 0.

## Tests

This workstream does not add tests. Existing tests in [workflow/](../../workflow/) lock in behavior. Compile/plan goldens in [internal/cli/testdata/](../../internal/cli/testdata/) verify the dispatch is unchanged.

## Risks

| Risk | Mitigation |
|---|---|
| Extracting a per-kind compile function reveals state leaked between kinds via a shared local slice | Promote the slice to a struct field on a new helper type, or restructure the dispatcher to thread it explicitly. Do not silently rely on shared package-level state. |
| The `gocognit` measurement on the new per-kind compilers exceeds the threshold | Extract one obvious helper per overage. Do not add baseline entries — that violates [01](01-lint-baseline-burndown.md)'s contract. |
| Tests for `WorkflowBodySpec` paths fail because the file move broke a relative-path assumption (`opts.WorkflowDir`) | The function bodies don't change; if a test fails, root-cause is almost certainly an import path drift, not a path-resolution change. Confirm before changing test code. |
| `make validate` fails on an example that previously worked | An example must compile identically before/after. If a diagnostic message moved (different file:line in the error), update the example's golden if one exists; otherwise root-cause the carve. |
| The `WorkflowBodySpec` preservation in Step 3 makes [08](08-schema-unification.md) harder | [08](08-schema-unification.md) is explicitly designed to delete the surface this workstream preserves. The deferred deletion is intentional. |
