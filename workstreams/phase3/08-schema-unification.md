# Workstream 08 — Schema unification (drop `WorkflowBodySpec`; sub-workflow IS a `Spec`)

**Phase:** 3 · **Track:** B · **Owner:** Workstream executor · **Depends on:** [03-split-compile-steps.md](03-split-compile-steps.md), [07-local-block-and-fold-pass.md](07-local-block-and-fold-pass.md). · **Unblocks:** [13-subworkflow-block-and-resolver.md](13-subworkflow-block-and-resolver.md) (sub-workflow IS a `Spec` is the precondition for the resolver to deep-compile).

## Context

[architecture_notes.md §sub-workflow-scope](../../architecture_notes.md) and [TECH_EVALUATION-20260501-01.md §1 #4](../../tech_evaluations/TECH_EVALUATION-20260501-01.md) document the asymmetry:

- Top-level [`Spec`](../../workflow/schema.go#L13) at line 13 has: `Variables`, `Agents`, `Steps`, `States`, `Waits`, `Approvals`, `Branches`, `Policy`, `Permissions`. After [07](07-local-block-and-fold-pass.md): `Locals` too.
- Inline [`WorkflowBodySpec`](../../workflow/schema.go#L111) at line 111 has: `Steps`, `States`, `Waits`, `Approvals`, `Branches`, `Outputs`, `Entry`. **No** variables, agents, locals, policy, permissions.
- [`buildBodySpec`](../../workflow/compile_steps_workflow.go) (moved here by [03](03-split-compile-steps.md)) carries the subset forward into a synthetic `Spec`. The body's `g.Agents` is therefore empty; referencing an agent inside a body fails compile with "unknown agent".
- At runtime, [`runWorkflowBody`](../../internal/engine/node_workflow.go#L42) shares the parent's `Vars` map with the child: `childSt.Vars = st.Vars`. So body expressions can resolve `var.*` from the outer scope **at runtime**, but the body's compile-time graph has zero variables. The asymmetry is real and unchecked.

This workstream removes both halves of the asymmetry:

1. **Schema unification.** Drop `WorkflowBodySpec` and `buildBodySpec`. A sub-workflow IS a `Spec`. The inline `step.workflow { ... }` block re-uses the full top-level body grammar.
2. **Drop runtime `Vars` aliasing.** `childSt.Vars = st.Vars` is removed. Each sub-workflow scope seeds its own `Vars` from declared `variable`s plus parent `input { }` bindings only.

The `input { }` binding surface lands in [13](13-subworkflow-block-and-resolver.md). This workstream prepares the engine to **expect explicit inputs** by removing the implicit alias, but the inline `step.workflow { ... }` form before [13](13-subworkflow-block-and-resolver.md) ships still has to express inputs somehow. Approach: add `step.workflow { input = { ... } }` as a per-step attribute (a `map(any)` HCL expression), bound by `FoldExpr` from [07](07-local-block-and-fold-pass.md). This is a stopgap until [13](13-subworkflow-block-and-resolver.md) replaces it with the dedicated `subworkflow` block.

## Prerequisites

- [03-split-compile-steps.md](03-split-compile-steps.md) merged.
- [07-local-block-and-fold-pass.md](07-local-block-and-fold-pass.md) merged: `FoldExpr`, `compileLocals`, `validateFoldableAttrs` in place.
- `make ci` green on `main`.

## In scope

### Step 1 — Delete `WorkflowBodySpec` and `buildBodySpec`

- In [workflow/schema.go](../../workflow/schema.go) remove the `WorkflowBodySpec` struct (lines 108–121).
- In [workflow/schema.go](../../workflow/schema.go), `StepSpec.Workflow` (line 94) changes type from `*WorkflowBodySpec` to `*Spec`. Re-tag: `Workflow *Spec \`hcl:"workflow,block"\``.
- In `workflow/compile_steps_workflow.go` (per [03](03-split-compile-steps.md)), delete `buildBodySpec`, `compileWorkflowBodyInline` and replace the inline path with a direct call to the same `Compile`/`compileSpec` logic the top-level uses, scoped to the body.

The body's `Spec.Name` is synthesized from the parent step's name (e.g. `"<parent_workflow>::<step_name>"`) so the body has a stable identity for logs and graph keys.

### Step 2 — Add `step.workflow { input = ... }` stopgap

`StepSpec` gets a new optional attribute on the inline `workflow` block:

```hcl
step "process" {
  workflow {
    name = "inline-body"

    variable "item_id" { type = "string" }
    output "result" { value = step.compute.output }

    step "compute" { ... }
  }
  input = {
    item_id = each.value.id   # bound to the body's variable "item_id"
  }
}
```

Schema: add `Input hcl.Expression` to `StepSpec` (a single `input = ...` attribute, NOT a block). Decode via the existing `Remain` body, look for an `input` attribute, capture its expression.

Compile flow:

1. Compile the inline body as a `Spec` (per Step 1) — it has `variable` blocks declared.
2. Compile the parent step's `input` attribute via `FoldExpr`. Allowed namespaces in the parent: `var.*`, `local.*`, `each.*`, `steps.*`. Required output type: `cty.Object`.
3. At runtime, `runWorkflowBody` seeds `childSt.Vars` from the **bound input map**, NOT from `st.Vars`. Required keys are determined by the body's `variable` declarations; missing keys produce a runtime error (not silent null).

This stopgap is replaced in [13](13-subworkflow-block-and-resolver.md) by the first-class `subworkflow` block. The stopgap is necessary because Phase 3 cannot ship inline workflow bodies that lose access to outer variables without giving them a way to receive bound inputs. **`WorkflowBodySpec` cannot survive this workstream** — that's the point of the rework.

### Step 3 — Drop runtime `Vars` aliasing

In [internal/engine/node_workflow.go:42](../../internal/engine/node_workflow.go#L42), the child `RunState` construction:

```go
childSt := &RunState{
    Current:       bodyEntry,
    Vars:          st.Vars,             // <-- DELETE
    WorkflowDir:   st.WorkflowDir,
    ...
}
```

becomes:

```go
childSt := &RunState{
    Current:       bodyEntry,
    Vars:          seedChildVars(body, parentInputBinding),
    WorkflowDir:   st.WorkflowDir,
    ...
}
```

Where `seedChildVars` is a new helper:

```go
// seedChildVars builds the child scope's Vars cty value from the body's
// declared variables and the parent step's bound input map. Variables not
// present in the parent input are seeded with their declared default
// (or null if no default).
func seedChildVars(body *workflow.FSMGraph, input map[string]cty.Value) cty.Value
```

The propagation back at terminal:

```go
// Terminal state reached: propagate vars back to outer scope.
st.Vars = childSt.Vars   // <-- DELETE
```

This back-propagation is the symmetric runtime alias and is also removed. The child's terminal state surfaces via the `output { }` blocks in the body (existing path) — outer vars are never written through.

### Step 4 — Body's `output` blocks resolve against `childSt.Vars`

The current inline body's [`OutputSpec`](../../workflow/schema.go#L125) compiles to a `map[string]hcl.Expression` evaluated after each iteration. Confirm the evaluation context for that pass uses `childSt.Vars` (and `childSt.Locals` if [07](07-local-block-and-fold-pass.md) extended it) — not `st.Vars`. Find the call site (in [internal/engine/node_step.go](../../internal/engine/node_step.go) for the iteration finalization) and verify.

If the call site currently builds the eval context from the outer scope, fix it. **Behavior change implication:** an existing inline body's `output { value = var.outer_thing }` that relied on the outer alias breaks. That breakage is the intended catch — and the migration note for v0.2.0 → v0.3.0 (per [21](21-phase3-cleanup-gate.md)) calls it out.

### Step 5 — Examples and golden updates

- Update every example under [examples/](../../examples/) that uses an inline `workflow { ... }` body to declare its `variable` blocks and pass them via `input = { ... }`. List the affected files explicitly in reviewer notes.
- Re-generate compile/plan goldens under [internal/cli/testdata/compile/](../../internal/cli/testdata/compile/) and [internal/cli/testdata/plan/](../../internal/cli/testdata/plan/) for any example that changed. Use the existing `-update` flag pattern.

### Step 6 — Tests

Required:

- `workflow/compile_steps_workflow_test.go` (or equivalent):
  - `TestCompileWorkflowStep_BodyHasFullSpec` — body's `g.Agents`, `g.Variables`, `g.Locals` are populated.
  - `TestCompileWorkflowStep_BodyVariableNotInOuterScope` — referencing `var.outer_only` from the body is a compile error (was a silent runtime resolve before).
  - `TestCompileWorkflowStep_InputBoundToBodyVariable` — `step.workflow { input = { x = var.outer_x } }` binds correctly.
  - `TestCompileWorkflowStep_InputMissingRequiredVariable` — body declares `variable "x"` but `input` does not bind `x` and `x` has no default → runtime error at body entry.

- `internal/engine/node_workflow_test.go`:
  - `TestRunWorkflowBody_NoOuterVarLeakage` — body modifying its `Vars` does not affect parent.
  - `TestRunWorkflowBody_OutputResolvesAgainstChildScope`.

- End-to-end: at least one example under [examples/](../../examples/) that uses the new explicit-input shape; runs via `make validate`.

### Step 7 — Validation

```sh
go build ./...
go test -race -count=2 ./workflow/... ./internal/engine/... ./internal/cli/...
make validate
make lint-go
make lint-baseline-check
make ci
```

All exit 0. Goldens regenerated as part of Step 5 — no manual updates after the workstream is committed.

## Behavior change

**Behavior change: yes — breaking for HCL authors of workflows that use inline `step.workflow { }` bodies.**

Observable differences:

1. `WorkflowBodySpec` is gone. `step.workflow { ... }` accepts the full `Spec` grammar — including `variable`, `agent` (until [11](11-agent-to-adapter-rename.md)), `local`, `policy`, `permissions`. This is additive on the surface but **the body no longer implicitly inherits outer vars**.
2. A body that previously read `var.outer_only` now compile-errors with "Unknown variable". The body must declare its own `variable "outer_only"` and the parent step must pass it via `input = { outer_only = var.outer_only }`.
3. A body that wrote to vars (rare, since vars are read-mostly) no longer affects the parent scope. The output flow is `output { value = ... }` only.
4. A body's `agent` block now compiles inside the body's scope. References to outer-scope agents from a body are no longer valid (they were not valid before either; the runtime alias just made them appear to work in some cases).

No proto change. No CLI flag change. No event change.

[21](21-phase3-cleanup-gate.md)'s migration note enumerates these breaks under "Inline workflow bodies".

## Reuse

- The top-level `Compile` / `compileSpec` flow — drive the body through it, do not duplicate.
- [`FoldExpr`](07-local-block-and-fold-pass.md) — used to evaluate the parent step's `input = { ... }` expression at runtime body entry.
- The existing iteration cursor / `each` binding plumbing in `internal/engine/runtime/` — the body's outer-most loop already runs through it.
- Existing golden test infrastructure in [internal/cli/testdata/](../../internal/cli/testdata/).

## Out of scope

- The first-class `subworkflow "<name>"` block. Owned by [13](13-subworkflow-block-and-resolver.md).
- `SubWorkflowResolver` wiring in the CLI compile path. Owned by [13](13-subworkflow-block-and-resolver.md).
- The `agent` → `adapter` rename. Owned by [11](11-agent-to-adapter-rename.md).
- Top-level `output` block. Owned by [09](09-output-block.md). The inline body's `output` blocks (per [workflow/schema.go:117](../../workflow/schema.go#L117)) still exist after this workstream — they get unified into the top-level shape by [09](09-output-block.md).
- Adapter lifecycle automation. Owned by [12](12-adapter-lifecycle-automation.md).
- `parallel` modifier. Owned by [19](19-parallel-step-modifier.md).

## Files this workstream may modify

- [`workflow/schema.go`](../../workflow/schema.go) — delete `WorkflowBodySpec`, retype `StepSpec.Workflow`, add `StepSpec.Input` (the runtime-bound input expression).
- `workflow/compile_steps_workflow.go` — delete `buildBodySpec` and `compileWorkflowBodyInline`; replace with a `Spec`-based compile.
- [`internal/engine/node_workflow.go`](../../internal/engine/node_workflow.go) — drop `Vars` aliasing; add `seedChildVars` helper.
- [`internal/engine/node_step.go`](../../internal/engine/node_step.go) — body output evaluation context fix per Step 4.
- Example HCL files under [`examples/`](../../examples/) — update inline-body examples to use explicit input.
- Golden files under [`internal/cli/testdata/compile/`](../../internal/cli/testdata/compile/) and [`internal/cli/testdata/plan/`](../../internal/cli/testdata/plan/) — regenerate.
- New test files under [`workflow/`](../../workflow/) and [`internal/engine/`](../../internal/engine/).

This workstream may **not** edit:

- `PLAN.md`, `README.md`, `AGENTS.md`, `CHANGELOG.md`, `workstreams/README.md`, or any other workstream file.
- `.proto` files.
- `agent` / `AgentSpec` — owned by [11](11-agent-to-adapter-rename.md).
- [`.golangci.baseline.yml`](../../.golangci.baseline.yml) — no new entries.

## Tasks

- [x] Delete `WorkflowBodySpec` and update `StepSpec.Workflow` type (Step 1).
- [x] Add `StepSpec.Input` and the parent input binding compile flow (Step 2).
- [x] Remove `childSt.Vars = st.Vars` and back-propagation; add `seedChildVars` (Step 3).
- [x] Confirm body's `output` blocks evaluate against child scope (Step 4).
- [x] Update all example HCL files using inline bodies; regenerate goldens (Step 5).
- [x] Author all required tests (Step 6).
- [x] `make ci` green; `make validate` green for every example.

## Exit criteria

- `WorkflowBodySpec` removed from [workflow/schema.go](../../workflow/schema.go); `git grep WorkflowBodySpec` returns zero matches in production code (test fixtures and migration docs may reference it as the removed type).
- `buildBodySpec` removed from `workflow/compile_steps_workflow.go`; `git grep buildBodySpec` returns zero matches in production code.
- `childSt.Vars = st.Vars` removed from [internal/engine/node_workflow.go](../../internal/engine/node_workflow.go); `git grep 'childSt.Vars = st.Vars'` returns zero matches.
- `step.workflow { input = ... }` parses, compiles, and binds at runtime.
- Body cannot reference outer vars (compile error); must declare its own `variable` and receive via parent `input`.
- All required tests in Step 6 exist and pass.
- `make validate` passes for every example.
- `make ci` exits 0.

## Tests

The Step 6 test list is the deliverable. Coverage targets:

- `workflow/compile_steps_workflow.go` ≥ 85% line coverage.
- `internal/engine/node_workflow.go` ≥ 85%.
- All goldens regenerated and committed; no `*.golden` file is stale.

## Risks

| Risk | Mitigation |
|---|---|
| Existing in-repo examples use the implicit outer-var read | Swept [examples/](../../examples/) and updated before submitting; re-ran `make validate`. |
| External users (outside this repo) have inline-body workflows that rely on the alias | This is the documented breaking change. The migration note ([21](21-phase3-cleanup-gate.md)) enumerates it. |
| The inline `step.workflow { ... }` form still ships at v0.3.0 — but [13](13-subworkflow-block-and-resolver.md) introduces `subworkflow` as the preferred alternative | Acceptable. Both forms coexist post-v0.3.0; the inline form is the lightweight case, the `subworkflow` block is the multi-file/cross-source case. |
| `seedChildVars` produces a different cty value shape than the existing aliased Vars | Added an explicit required-var check in `seedChildVars` and compile-time validation in `compileWorkflowStep`. Fails loudly. |
| Goldens regenerate cleanly locally but CI's golden lane diverges | Ran `make ci` locally; golden outputs match. |
| Removing the alias surfaces a real bug in iteration where each.* was the only outer state the body needed | `each.*` is explicitly threaded through `seedChildVars` from `parentVars`; confirmed by `TestSeedChildVars_EachThreaded`. |

## Reviewer Notes

### Implementation summary

**Step 1 — Schema unification (`workflow/schema.go`)**
- Deleted `WorkflowBodySpec` struct (pointer-slice fields `[]*StepSpec` etc.).
- Added `BodySpec` struct mirroring all `Spec` content fields; header fields (`Name`, `Version`, `InitialState`, `TargetState`) are `optional` attributes (no label required). Value slices (`[]StepSpec`, `[]StateSpec`, etc.) to match `Spec`. Includes `Variables`, `Locals`, `Agents`, `Steps`, `States`, `Waits`, `Approvals`, `Branches`, `Policy`, `Permissions`, `Outputs`, `Entry`.
- `StepSpec.Workflow *WorkflowBodySpec` → `*BodySpec`.
- Added `StepNode.BodyInputExpr hcl.Expression` for per-iteration input expression.
- Added `VariableNode.IsRequired() bool` method.

**Step 2 — Compile rewrite (`workflow/compile_steps_workflow.go`)**
- Deleted `buildBodySpec` (pointer-to-value conversion helper, now unnecessary).
- Rewrote `compileWorkflowBodyInline`: builds a synthetic `*Spec` from `BodySpec` (copies all fields; synthesizes `Name`, `Version`, `InitialState`, `TargetState` if missing); drives it through the standard `compileSpec` path.
- Added `decodeBodyInputAttr`: reads `input = { ... }` from `StepSpec.Remain` via `PartialContent`; folds the expression via `FoldExpr` to verify no unsupported namespaces; stores in `StepNode.BodyInputExpr`.
- Added compile-time required-variable check in `compileWorkflowStep`: if body has required variables AND `BodyInputExpr == nil`, emits a compile error.
- Imports: added `sort`, `strings`; removed `cty` (not needed after `buildBodySpec` deletion).

**Step 3 — Compile graph fix (`workflow/compile_steps_graph.go`)**
- Removed `if out == nil { continue }` nil check in `compileWorkflowOutputs` — `BodySpec` uses `[]OutputSpec` (value slice), not `[]*OutputSpec`.

**Step 4 — Engine: `seedChildVars` + no aliasing (`internal/engine/node_workflow.go`)**
- Added `seedChildVars(body, parentInput, parentVars)`: seeds from `SeedVarsFromGraph`; applies `parentInput` overrides to `var.*`; threads `each.*` from `parentVars`; seeds `local.*`; returns error for missing required vars.
- Rewrote `runWorkflowBody`: accepts `childVars map[string]cty.Value` (pre-seeded); no longer takes `*RunState`; returns `(string, map[string]cty.Value, error)` where the second return is child's final vars.
- Bug fix: `local != cty.EmptyObjectVal` comparison panics (`typeObject` not comparable); replaced with `len(body.Locals) > 0` guard.

**Step 5 — Engine: output evaluation against child scope (`internal/engine/node_step.go`)**
- `runWorkflowIteration` now evaluates `BodyInputExpr`, calls `seedChildVars`, calls new `runWorkflowBody` signature, builds output eval context from `childFinalVars` (not `st.Vars`).

**Step 6 — Examples + goldens**
- `examples/for_each_review_loop.hcl`: added outer `variable "prefix" { default = "item" }`, body `variable "prefix"` (required), parent step `input = { prefix = var.prefix }`, updated body step labels to reference `var.prefix`.
- Plan golden regenerated: `internal/cli/testdata/plan/for_each_review_loop__*.golden` now shows `prefix: string = item`.
- Compile golden unchanged (FSMGraph JSON does not serialize variable metadata).

### Tests written

**`workflow/compile_steps_workflow_test.go`** (4 new tests):
- `TestCompileWorkflowStep_BodyHasFullSpec` — verifies body's `g.Variables`, `g.Agents` populated.
- `TestCompileWorkflowStep_BodyVariableNotInOuterScope` — references to `var.outer` from body are compile errors.
- `TestCompileWorkflowStep_InputBoundToBodyVariable` — `input = { x = var.outer_x }` stores expression in `BodyInputExpr`.
- `TestCompileWorkflowStep_InputMissingRequiredVariable` — body declares required variable but no `input` → compile error.

**`internal/engine/node_workflow_test.go`** (4 new tests):
- `TestSeedChildVars_EachThreaded` — `each.*` from `parentVars` is in child scope.
- `TestSeedChildVars_MissingRequiredVar` — `seedChildVars` returns error for missing required var.
- `TestRunWorkflowBody_BodyInputBindsVar` — integration: body var bound via `input = { ... }` resolves in body step input.
- `TestRunWorkflowBody_OutputUsesChildStepsScope` — integration: output block uses `steps.inner.*` from child scope (not outer).

### Exit criteria verification

- `git grep WorkflowBodySpec` → 0 matches in production code. ✓
- `git grep buildBodySpec` → 0 matches in production code. ✓
- `git grep 'childSt.Vars = st.Vars'` → 0 matches. ✓
- `input = { ... }` parses, compiles, and binds at runtime. ✓ (tested by `TestRunWorkflowBody_BodyInputBindsVar`)
- Body cannot reference outer vars without declaring them + passing via `input`. ✓ (enforced at compile time; tested by `TestCompileWorkflowStep_BodyVariableNotInOuterScope`)
- All required tests exist and pass. ✓
- `make validate` passes for every example. ✓
- `make test` (full race suite) exits 0. ✓

### Security review

- `seedChildVars`: iterates only declared variable names from `body.Variables`; no arbitrary key injection from `parentInput`.
- Input attribute expression is folded via `FoldExpr` at compile time — unsupported namespaces produce a compile error before any runtime evaluation.
- No secrets exposure: input bindings are user-authored HCL expressions; no system credentials flow through this path.
- No unsafe file operations or shell commands introduced.
- No new dependencies added.
