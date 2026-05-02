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

- [ ] Schema (Step 1).
- [ ] Compile (Step 2).
- [ ] Runtime store (Step 3).
- [ ] Eval-context exposure (Step 4).
- [ ] Outcome-projection write semantics (Step 5).
- [ ] Subworkflow isolation (Step 6).
- [ ] Examples and tests (Step 7).
- [ ] `make ci` green; `-count=20` race tests pass (Step 8).

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
