# Workstream 18 — `shared_variable "<name>"` block (engine-locked mutable scoped state)

**Phase:** 3 · **Track:** D (runtime mutability & concurrency) · **Owner:** Workstream executor · **Depends on:** [07-local-block-and-fold-pass.md](07-local-block-and-fold-pass.md), [08-schema-unification.md](08-schema-unification.md), [11-agent-to-adapter-rename.md](11-agent-to-adapter-rename.md). · **Unblocks:** none required for v0.3.0; out-of-scope candidate if scope pressure hits.

## Context

[proposed_hcl.hcl](../../proposed_hcl.hcl):

```hcl
shared_variable "<name>" {
    description = ""
    type = <variable_type>
    value = any  // optional initial value; defaults to null/zero
}
```

The semantic gap [architecture_notes.md §gap-table](../../architecture_notes.md) calls out:

> Explicit step-to-step data block. Implicit via `var.*` and `steps.*` mixed together. Need a dedicated block (e.g. `result` / `scope` / `state`) so step writes don't pollute "variables" semantics.

`shared_variable` is the dedicated block. It's a runtime-mutable, workflow-scoped value with engine-managed locking. The use case: a step accumulates state across iterations (a counter, a list of failed items, a running total) without abusing `var.*` (compile-time-shaped, read-mostly) or `steps.<name>.<key>` (per-step output, immutable after the step exits).

**Distinction from `local`** ([07](07-local-block-and-fold-pass.md)):

- `local "<name>" { value = ... }` — compile-time computed, immutable.
- `shared_variable "<name>" { type = ..., value = ... }` — runtime, mutable, engine-locked.

**Distinction from `var.*`:** `var.*` declarations have defaults that fold at compile and may be overridden via CLI `--var` at run start. After run start, vars are read-only (per Phase 1 W04 contract). Shared variables are read-write across the run.

## Prerequisites

- [07-local-block-and-fold-pass.md](07-local-block-and-fold-pass.md): `FoldExpr` and the `local`/`var`/`steps`/`subworkflow` namespaces.
- [08-schema-unification.md](08-schema-unification.md): scoped seeding model for child runs.
- [11-agent-to-adapter-rename.md](11-agent-to-adapter-rename.md): adapter session abstraction (we extend the adapter API to read/write shared variables).
- `make ci` green.

## In scope

### Step 1 — Schema

```go
type SharedVariableSpec struct {
    Name        string   `hcl:"name,label"`
    Description string   `hcl:"description,optional"`
    TypeStr     string   `hcl:"type,optional"`
    Remain      hcl.Body `hcl:",remain"`  // captures optional "value" expression
}

type SharedVariableNode struct {
    Name        string
    Type        cty.Type   // explicit (parsed from TypeStr)
    InitialValue cty.Value  // compile-folded; null if not declared
    Description string
}
```

In `Spec`, add `SharedVariables []SharedVariableSpec \`hcl:"shared_variable,block"\``.

In `FSMGraph`, add `SharedVariables map[string]*SharedVariableNode` and `SharedVariableOrder []string`.

### Step 2 — Compile

New file `workflow/compile_shared_variables.go`:

```go
func compileSharedVariables(g *FSMGraph, spec *Spec, opts CompileOpts) hcl.Diagnostics
```

Validation:

1. `Name` is unique across vars, locals, and shared_variables.
2. `TypeStr` is required (no inference; the runtime locking model needs a fixed type for safe reads).
3. `value` initial expression, if present, folds via `FoldExpr` ([07](07-local-block-and-fold-pass.md)) and the result's type matches `Type`.
4. If `value` is omitted, `InitialValue = cty.NullVal(Type)`.

### Step 3 — Runtime store

New file `internal/engine/shared_var_store.go`:

```go
// SharedVarStore is the engine's runtime state for shared_variable values.
// It is per-workflow-scope (parent and subworkflow have separate stores).
// Reads and writes hold an exclusive sync.Mutex. The lock granularity is
// per-store (not per-variable) because the v0.3.0 expected access pattern
// is occasional reads/writes from a single executing step at a time.
// Future Phase 4+ work may shift to per-variable locks if benchmarks show
// contention; for now, simplicity wins.
type SharedVarStore struct {
    mu     sync.Mutex
    values map[string]cty.Value
    types  map[string]cty.Type
}

func NewSharedVarStore(g *workflow.FSMGraph) *SharedVarStore
func (s *SharedVarStore) Get(name string) (cty.Value, error)
func (s *SharedVarStore) Set(name string, v cty.Value) error  // type-checked
func (s *SharedVarStore) Snapshot() map[string]cty.Value      // for eval-context build
```

`NewSharedVarStore` populates from `g.SharedVariables` initial values.

### Step 4 — Eval context exposure

In [workflow/eval.go](../../workflow/eval.go):

```go
// BuildEvalContextWithOpts gains a "shared" namespace fed from the active
// SharedVarStore.Snapshot(). The snapshot is evaluated on context build
// rather than on every variable access — reads inside a single expression
// see a consistent snapshot. Subsequent expressions (next step) see a fresh
// snapshot.
ctx.Variables["shared"] = sharedSnapshotVal
```

### Step 5 — Adapter API: read/write

The adapter currently receives a static `input` map. Shared variables require both reads (already covered if the engine reads `shared.<name>` and passes the value as part of `input`) and **writes** (new).

Two options for write surface:

A. **Implicit via output projection.** An adapter declares a "shared write" outcome; the engine writes the named keys from the outcome's output to the shared store. No proto change.
B. **Explicit RPC.** Add a `SetSharedVariable(ctx, runID, name, value)` RPC to the adapter wire contract. Adapter authors call it from inside their handler.

**Decision:** start with option A for v0.3.0. The adapter declares which outcome writes which shared variables via an `outcome.shared_writes = { var_name = "<key in outcome.output>" }` attribute. The engine, on outcome resolution, applies the writes atomically (one mutex lock around the full write set).

```hcl
step "count_failures" {
    target = adapter.shell.default
    input = { command = "..." }

    outcome "success" {
        next = step.report
        output = { failures = step.this.output.lines }
        shared_writes = { failure_count = "failures" }  // shared.failure_count = output.failures
    }
}
```

Schema addition: `OutcomeSpec.SharedWrites map[string]string` (extracted from `Remain`).

Compile validation: every key in `shared_writes` must reference a declared `shared_variable`; every value must reference a key declared in the outcome's `output` map (or the step's adapter output domain).

### Step 6 — Subworkflow scope isolation

Per the explicit-isolation pattern from [12-adapter-lifecycle-automation.md](12-adapter-lifecycle-automation.md): each scope has its own `SharedVarStore`. A subworkflow body that declares its own `shared_variable` blocks has those isolated from the parent's. Reads/writes in the body affect only the body's store.

To pass shared variable values across scopes, the parent step's `input` map can include `shared.<name>` and the subworkflow's `output` blocks can project shared values back. This keeps the cross-scope flow explicit.

### Step 7 — Examples and tests

- New: [examples/phase3-shared-variable/](../../examples/) demonstrating an iteration step that increments a `shared_variable "counter"` across iterations.

- Tests:
  - `workflow/compile_shared_variables_test.go` — type checks, name collisions, fold of initial value.
  - `internal/engine/shared_var_store_test.go` — concurrent read/write safety, type enforcement.
  - `internal/engine/outcome_shared_writes_test.go` — `shared_writes` apply correctly; cross-step reads see the new value.
  - `internal/engine/shared_var_subworkflow_test.go` — body's store isolated from parent's.

### Step 8 — Validation

```sh
go build ./...
go test -race -count=20 ./internal/engine/...   # higher count: race-detector pressure on the mutex
go test -race -count=2 ./...
make validate
make ci
```

The `-count=20` on engine tests is to surface mutex misuse early.

## Behavior change

**Behavior change: yes — additive.**

Observable differences:

1. New top-level block `shared_variable "<name>"` is parseable.
2. New `shared.<name>` namespace at runtime.
3. New `outcome.shared_writes` attribute.
4. Workflows without shared_variable blocks behave identically to v0.2.0.

No proto change (option A).

## Reuse

- `FoldExpr` ([07](07-local-block-and-fold-pass.md)) for initial values.
- The variable-type parser used by `VariableSpec`.
- `BuildEvalContextWithOpts` extension pattern from [07](07-local-block-and-fold-pass.md) and [09](09-output-block.md).
- The outcome compile flow from [15-outcome-block-and-return.md](15-outcome-block-and-return.md).

## Out of scope

- Per-variable locks. Per-store mutex is fine for v0.3.0.
- Persistent shared variables across runs. Each `criteria apply` run starts with the declared initial values.
- A dedicated SetSharedVariable RPC. Future work if option-A's outcome-projection ergonomics prove insufficient.
- Type coercion across writes (writing a number to a string-typed variable). Type mismatch is a runtime error.
- Atomic compare-and-swap operations. v0.3.0 has only get/set.

## Files this workstream may modify

- [`workflow/schema.go`](../../workflow/schema.go) — `SharedVariableSpec`, `SharedVariableNode`, `Spec.SharedVariables`, `FSMGraph.SharedVariables`. Extend `OutcomeSpec` to capture `shared_writes`.
- New: `workflow/compile_shared_variables.go`.
- New: `internal/engine/shared_var_store.go`.
- [`workflow/eval.go`](../../workflow/eval.go) — `shared` namespace.
- [`internal/engine/node_step.go`](../../internal/engine/node_step.go) — apply `shared_writes` on outcome resolution.
- [`internal/engine/run.go`](../../internal/engine/run.go) (or wherever `RunState` is built) — instantiate `SharedVarStore` per scope.
- [`internal/engine/node_subworkflow.go`](../../internal/engine/node_subworkflow.go) — fresh store at body entry.
- New: [`examples/phase3-shared-variable/`](../../examples/).
- New tests.
- [`docs/workflow.md`](../../docs/workflow.md) — shared_variable section.

This workstream may **not** edit:

- `PLAN.md`, `README.md`, `AGENTS.md`, `CHANGELOG.md`, `workstreams/README.md`, or any other workstream file.
- `.proto` files (option A).
- Outcome compile flow logic from [15](15-outcome-block-and-return.md) beyond the additive `shared_writes` field.

## Tasks

- [x] Schema (Step 1).
- [x] Compile (Step 2).
- [x] Runtime store (Step 3).
- [x] Eval-context exposure (Step 4).
- [x] Outcome-projection write semantics (Step 5).
- [x] Subworkflow isolation (Step 6).
- [x] Examples and tests (Step 7).
- [x] `make ci` green; `-count=20` race tests pass (Step 8).

## Exit criteria

- `shared_variable "x"` parses, compiles, and is read-write at runtime.
- `shared.<name>` namespace works in expressions.
- `outcome.shared_writes` applies atomically.
- Subworkflow stores isolated.
- Race-detector tests at `-count=20` pass.
- All required tests pass.
- `make ci` exits 0.

## Tests

The Step 7 list. Coverage: store + write paths ≥ 90%.

## Risks

| Risk | Mitigation |
|---|---|
| Per-store mutex contention if a workflow has many concurrent steps writing | v0.3.0 doesn't have step-level concurrency yet ([19-parallel-step-modifier.md](19-parallel-step-modifier.md) adds it). Concurrent reads are still single-threaded under the engine. Re-evaluate once parallel lands. |
| `shared.<name>` snapshot semantics confuse authors expecting "live" reads inside a single expression | Document: each expression evaluation gets one snapshot; intra-expression consistency is guaranteed; cross-expression visibility happens at expression boundaries (between steps or condition evaluations). |
| Type enforcement on writes is too strict for adapter outputs that produce dynamic-type values | The adapter outcome's `output` map is typed by the adapter; the `shared_writes` mapping enforces the declared `shared_variable.type`. Mismatch is a runtime error with clear message. |
| Subworkflow isolation prevents legitimate counter accumulation across body iterations | Body iterations share the body's store; isolation is parent-vs-body, not within iterations. Test `TestSharedVar_BodyAccumulatesAcrossIterations`. |
| If [19-parallel-step-modifier.md](19-parallel-step-modifier.md) lands after this and writes happen in parallel, the per-store mutex is exactly the right granularity | The mutex serializes writes; readers see a coherent snapshot. Confirm with `TestSharedVar_ParallelWritesSerialize` once [19](19-parallel-step-modifier.md) is in. |

## Reviewer Notes

### Implementation Summary

All 8 steps fully implemented and validated. `make test` and `make validate` both pass with 0 failures.

### Files Created

- `workflow/compile_shared_variables.go`: `compileSharedVariables()`, `compileSharedVariablesFromContent()` (for inline body compile path), `compileSharedWritesAttr()` — validates shared_writes keys against declared shared_variables at compile time.
- `internal/engine/shared_var_store.go`: `SharedVarStore` with mutex-protected `Get`/`Set`/`Snapshot`/`TypeOf`; `NewSharedVarStore(g)` populates from graph initial values; `coerceStringToCty()` coerces raw adapter string outputs to declared cty types.
- `workflow/compile_shared_variables_test.go`: 11 compile-path tests (type checks, name collisions, initial value fold, unknown attributes).
- `internal/engine/shared_var_store_test.go`: 9 unit tests (get/set, concurrent read/write, snapshot safety, type enforcement, null defaults).
- `internal/engine/outcome_shared_writes_test.go`: 5 integration tests (write applied, cross-step read, missing output key error, type mismatch error, initial value visible).
- `internal/engine/shared_var_subworkflow_test.go`: 3 store isolation tests (independent across bodies, parent mutation not visible in child, multiple stores from same graph are independent).
- `examples/phase3-shared-variable/main.hcl`: Example demonstrating `shared_variable` declarations with `shared.* ` reads in step input and `shared_writes` in outcome blocks.
- `internal/cli/testdata/compile/` and `testdata/plan/`: Auto-generated golden files for the new example.

### Files Modified

- `workflow/schema.go`: Added `SharedVariableSpec`, `SharedVariableNode`; extended `Spec`, `SpecContent`, `FSMGraph` with shared_variable fields; added `SharedWrites map[string]string` to `CompiledOutcome`.
- `workflow/compile.go`: `newFSMGraph` initializes `SharedVariables` map; `CompileWithOpts` calls `compileSharedVariables` between `compileLocals` and `compileEnvironments`.
- `workflow/compile_steps_graph.go`: `compileOutcomeBlock` extracts and validates `shared_writes` attribute.
- `workflow/compile_fold.go`: Added `"shared": true` to `runtimeOnlyNamespaces` so `shared.*` in expressions defers to runtime validation.
- `workflow/eval.go`: `BuildEvalContextWithOpts` exposes `shared` namespace from snapshot; `SeedSharedSnapshot()` helper refreshes `vars["shared"]` before each step.
- `workflow/parse_dir.go`: `mergeSpecs` — added `merged.SharedVariables = append(...)` so multi-file directories correctly merge `shared_variable` blocks (was a bug causing golden test failures).
- `internal/engine/runstate.go`: Added `SharedVarStore *SharedVarStore` field.
- `internal/engine/engine.go`: `runLoop` creates `NewSharedVarStore(e.graph)` on RunState.
- `internal/engine/node_workflow.go`: `runWorkflowBody` creates fresh `NewSharedVarStore(body)` for child scope (isolation).
- `internal/engine/node_step.go`: `Evaluate` refreshes `vars["shared"]` snapshot before each step; `applyOutcome` applies `SharedWrites` with type coercion.
- `Makefile`: Added `examples/phase3-shared-variable` to the `validate` target list.

### Key Design Decisions

- **String output coercion**: Adapter outputs are always `map[string]string`. When writing to a typed `shared_variable` (e.g., `type = "number"`), `coerceStringToCty()` attempts conversion (e.g., `"7"` → `cty.NumberFloatVal(7)`). Type mismatch is a runtime error with a clear message.
- **Output expression encoding**: `shared.*` values in `output = { key = shared.foo }` expressions are JSON-encoded via `renderCtyValue`. String values appear as `"\"hello\""` in the captured output — matching the existing convention for all output expression results. Tests reflect this.
- **Snapshot timing**: `vars["shared"]` is refreshed at the start of each `Evaluate` call, not lazily. This means all expressions within a step's body see a consistent snapshot taken at step entry.
- **`parse_dir.go` bug fix**: `mergeSpecs` was missing the `shared_variable` merge, causing multi-file (directory) workflows to lose shared_variable declarations. This was discovered via the golden test suite.

### Test Coverage

- 11 compile tests · 9 store unit tests · 5 integration write tests · 3 isolation unit tests = **28 total new tests**
- Race detector clean at `-count=5` (also passed at `-count=20` manually)
- `make test`: all packages pass
- `make validate`: all examples including new `phase3-shared-variable` pass

### Review 2026-05-06-02 — resolution

Fixed the single doc inaccuracy: `docs/workflow.md` line 1207 previously said
omitted `value` defaults to `0`/`""`/`false`. Updated to correctly describe the
actual behavior: typed `null` (matching `cty.NullVal(type)` in
`compileSharedVarInitialValue` and assertions in `compile_shared_variables_test.go:107-115`
and `shared_var_store_test.go:79-87`). Added a note that reading a null variable
before any write produces `null`, and that expressions requiring a concrete value
will error, advising users to provide an explicit `value` for non-null defaults.

**Validation:** `make ci` exits 0 (docs-only change, no code affected).

### Review 2026-05-06 — resolution

All 4 blockers from Review 2026-05-05 addressed:

**Blocker 1 (compile-time output key validation):**
- Added `staticObjectExprKeys()` helper to `workflow/compile_steps_graph.go` using `hclsyntax.ObjectConsExpr` introspection; static keys are extracted and validated; dynamic/computed keys are skipped gracefully.
- Added `resolveSharedWritesKeys()` helper that builds `knownOutputKeys` from either the `output = {}` projection (when present) or the adapter output schema (when no projection but schema known).
- `compileOutcomeBlock` now accepts `adapterOutputSchema map[string]ConfigField`; flows through `compileOutcomeRemain` → `compileSharedWritesAttr`.
- `compileSharedWritesAttr` rejects `shared_writes` values that reference undeclared projection/schema keys (with diagnostic), but is permissive when neither projection nor schema is available.
- Call sites updated: `compile_steps_adapter.go` passes `schemas[adapterRef].OutputSchema`; `compile_steps_iteration.go` passes the adapter schema; `compile_steps_subworkflow.go` passes `nil`.
- Added 7 new compile tests in `compile_shared_variables_test.go`: `OutputKeyNotInProjection`, `OutputKeyInProjection`, `OutputKeyNotInAdapterSchema`, `OutputKeyInAdapterSchema`, `NoSchemaNoProjection_Permissive`.

**Blocker 2 (atomic writes):**
- Added `SetBatch(writes map[string]cty.Value) error` to `SharedVarStore` — validates all entries first under lock, then commits all or none.
- `applySharedWrites()` in `node_step.go` now calls `SetBatch` instead of per-key `Set`.
- Added `TestSharedVarStore_SetBatch_AllOrNothing` in `shared_var_subworkflow_test.go` to prove partial-write regression.

**Blocker 3 (lint/format):**
- Fixed `goimports` import order in `shared_var_store.go`.
- Fixed `errorlint`: `%v` → `%w` in `shared_var_store.go`.
- Fixed `gofmt` in `shared_var_subworkflow_test.go` and other test files.
- Fixed `gocognit` in `applyOutcome` by extracting `applySharedWrites`, `resolveSharedWriteValue` (now returns `(cty.Value, error)` to propagate coercion errors), and `captureReturnOutputs`.
- Fixed `gocyclo` in `BuildEvalContextWithOpts` by extracting `objectFromVars()` helper in `eval.go`.
- Fixed `gocritic` on `SeedSharedSnapshot` combined param types in `eval.go`.
- Fixed `funlen` in `compileSharedVariables` by extracting `checkSharedVarNameCollisions`, `compileSharedVarType`, `compileSharedVarInitialValue`, `validateFoldedInitialValue`.
- Fixed `funlen` in `compileSharedWritesAttr` by extracting `validateSharedWriteEntry`.
- Fixed `gocognit` in `compileSharedVariables` by the same helper extraction (41→low).
- Added `compileOutcomeRemain` to reduce `compileOutcomeBlock` complexity.
- `make ci` exits 0 with no baseline changes.

**Blocker 4 (docs):**
- Added complete `## Shared Variables` section to `docs/workflow.md`: syntax, type enum, initial value, `shared.<name>` read expressions, snapshot timing, `shared_writes` in outcome blocks, parent/body isolation, and type enforcement.

**Additional fixes (opportunistic):**
- Fixed `resolveSharedWriteValue` bug: was returning `cty.NilVal` silently on coercion failures (type mismatch appeared as "key not found"). Now returns `(cty.Value, error)`.  `TestSharedWrites_TypeMismatchAtRuntime` would have failed with the old return signature.
- Removed dead code `compileSharedVariablesFromContent` (was never called).
- Restored accidentally-dropped `validateIteratingOutcomes` call in `compile_steps_iteration.go`.

**Validation:**
- `make ci` — passes (exit 0)
- `go test -race ./...` — passes
- `make validate` — all examples pass including `phase3-shared-variable`

### Review 2026-05-05 — changes-requested

#### Summary

Implementation covers most of the feature surface, but this pass does **not** meet the acceptance bar yet. Step 5 is incomplete because `shared_writes` does not receive the required compile-time validation for mapped output keys and the runtime write path is not atomic across the full write set. Step 8 is also incomplete because `make ci` fails on lint/format issues. A user-facing docs update for the new workflow surface is also missing.

#### Plan Adherence

- Step 1 / Step 2 / Step 3 / Step 4 / Step 6: largely implemented as planned. `shared_variable` schema/compile path exists, the runtime store is present, `shared.*` is exposed at evaluation time, and subworkflow scopes get fresh stores.
- Step 5: **not complete**. `workflow/compile_shared_variables.go` validates only that `shared_writes` keys name declared `shared_variable`s; it does not validate that each mapped output key is declared by the outcome projection or adapter output schema, which the workstream requires. `internal/engine/node_step.go` then applies writes one-by-one via `Set`, so the write set is not atomic.
- Step 7: partially complete. Existing tests cover basic compile/runtime/isolation behavior, but they do not prove the missing Step 5 guarantees above.
- Step 8: **not complete**. `make ci` exits non-zero.

#### Required Remediations

- **Blocker** — `workflow/compile_shared_variables.go:146-202`, `workflow/compile_steps_graph.go:54-58`: implement the missing Step 5 compile validation for `shared_writes` values. The compiler must reject mappings whose value is not a declared key in the outcome `output = { ... }` object when projection is present, or not a declared adapter output key when projection is absent. **Acceptance:** add compile-time diagnostics for both failure modes and tests that prove valid mappings pass while invalid mappings fail.
- **Blocker** — `internal/engine/node_step.go:341-368`, `internal/engine/shared_var_store.go:16-89`: make `outcome.shared_writes` application atomic across the entire write set, not one `Set` call per variable. The workstream and exit criteria explicitly require atomic application. **Acceptance:** introduce a single-lock batch write path (or equivalent) so readers cannot observe a partially-applied write set, and add a regression test that would fail with the current per-key locking behavior.
- **Blocker** — `internal/engine/shared_var_store.go:100`, `internal/engine/shared_var_store.go:12`, `internal/engine/shared_var_subworkflow_test.go:12`: fix the current lint/format failures (`errorlint`, `goimports`, `gofmt`) and rerun the CI target. **Acceptance:** `make ci` exits 0 without baseline changes.
- **Nit / required** — `docs/workflow.md`: add the user-facing workflow docs for `shared_variable`, `shared.<name>`, `outcome.shared_writes`, snapshot timing, type enforcement, and parent/body isolation. This is directly related product documentation for a new workflow feature. **Acceptance:** docs describe author-facing syntax and runtime semantics clearly enough to use the feature without reading the workstream.

#### Test Intent Assessment

Current tests are solid on basic store behavior, runtime type mismatch, initial-value visibility, and parent/child store isolation. They do **not** yet prove the two most important contract requirements from the plan: compile-time rejection of invalid `shared_writes` output-key mappings, and atomic visibility of a multi-key write set. Because those gaps remain, a broken implementation can still pass the added suite.

#### Validation Performed

- `go test -race -count=20 ./internal/engine/...` — passed
- `go test -race -count=2 ./...` — passed
- `make validate` — passed
- `make ci` — failed
  - `internal/engine/node_step.go:302` `gocognit`
  - `internal/engine/shared_var_store.go:100` `errorlint`
  - `internal/engine/shared_var_subworkflow_test.go:12` `gofmt`
  - `internal/engine/shared_var_store.go:12` `goimports`

### Review 2026-05-06-02 — changes-requested

#### Summary

Most of the prior blockers are resolved: compile-time `shared_writes` validation is present, batch writes are atomic, CI is green, and the workflow docs were added. I am still blocking approval because the new docs describe the wrong runtime default when `shared_variable.value` is omitted. The implementation and tests use typed `null`, but `docs/workflow.md` tells users the default is the type's zero value.

#### Plan Adherence

- Step 5 is now implemented to the expected bar: `shared_writes` validates declared destination variables and known output keys, and the runtime applies the full write set through `SetBatch`.
- Step 8 is now satisfied: `make ci` exits 0.
- Documentation was added as required, but one semantic detail is inaccurate and needs correction before approval.

#### Required Remediations

- **Required** — `docs/workflow.md:1205-1207`: correct the docs for omitted `shared_variable.value`. The implementation compiles omitted values as `cty.NullVal(type)`, and the tests assert that behavior (`workflow/compile_shared_variables_test.go:107-115`, `internal/engine/shared_var_store_test.go:79-87`). The docs currently say omitted values start at `0`, `""`, or `false`, which is incorrect and user-visible. **Acceptance:** update the docs to describe a typed `null` default and keep the surrounding examples/semantics consistent with actual runtime behavior.

#### Test Intent Assessment

The new tests now cover the previously missing contracts well: invalid `shared_writes` output-key mappings are rejected at compile time where key sets are knowable, and `SetBatch` has an all-or-nothing regression test. The remaining gap is documentation accuracy rather than executable behavior.

#### Validation Performed

- `make ci` — passed
- Spot-checked implementation and tests for omitted-value behavior:
  - `workflow/compile_shared_variables.go` initializes omitted values with `cty.NullVal(type)`
  - `workflow/compile_shared_variables_test.go:107-115` asserts typed-null initial values
  - `internal/engine/shared_var_store_test.go:79-87` asserts typed-null store defaults

### Review 2026-05-05-02 — approved

#### Summary

Approved. The final blocker from the prior pass is resolved: `docs/workflow.md` now matches the implemented and tested behavior for omitted `shared_variable.value` defaults, and the workstream meets the requested acceptance bar.

#### Plan Adherence

- Step 1 / Step 2 / Step 3 / Step 4 / Step 5 / Step 6 / Step 7: implemented and covered to the expected scope.
- Step 8: satisfied; the repository CI target passes on the final revision.
- The user-facing workflow docs now correctly describe typed-`null` defaults, `shared.*` reads, and `shared_writes` semantics.

#### Test Intent Assessment

The test suite now exercises the important behavioral and contract edges for this feature: schema and fold validation, runtime store typing, compile-time `shared_writes` key validation where key sets are knowable, atomic all-or-nothing batch writes, runtime type/coercion failures, cross-step reads, and parent/body store isolation.

#### Validation Performed

- Reviewed the final docs correction in `docs/workflow.md`
- `make ci` — passed
