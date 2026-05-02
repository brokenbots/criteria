# Workstream 19 — `parallel` step modifier (concurrent execution across list items)

**Phase:** 3 · **Track:** D · **Owner:** Workstream executor · **Depends on:** [14-universal-step-target.md](14-universal-step-target.md). · **Unblocks:** none for v0.3.0.

## Context

[proposed_hcl.hcl §4](../../proposed_hcl.hcl):

> `parallel`: A new list modifier to instruct the engine to execute the step concurrently for multiple items.

```hcl
step "fetch_all" {
    parallel = [task1, task2, task3]
    target   = subworkflow.fetcher
    input    = { task = each.value }
}
```

Versus the existing `for_each` (sequential) and `count` (sequential N times):

- `parallel = [...]` evaluates the expression to a list and runs the step **concurrently** for every item.
- `each.value` and `each.index` bind as in `for_each`.
- The engine's existing scheduler TODO at [internal/engine/node.go:47](../../internal/engine/node.go#L47) is the natural plug point.

This is the only Track D workstream that touches engine scheduling. The HCL surface is small; the engine refactor is real but bounded.

## Prerequisites

- [14-universal-step-target.md](14-universal-step-target.md): universal `target` is the routing primitive parallel iterations route through.
- `make ci` green.

## In scope

### Step 1 — Schema

Add `Parallel hcl.Expression` to `StepSpec`. Mutually exclusive with `ForEach` and `Count`. Compile error if multiple are set.

In `StepNode`, the field already exists as `Parallel hcl.Expression` (reserved by [14](14-universal-step-target.md) — confirm and populate).

### Step 2 — Compile validation

In the iteration compile path (per [03](03-split-compile-steps.md): `workflow/compile_steps_iteration.go`):

1. If `step.parallel` is set, capture the expression.
2. Validate via `validateFoldableAttrs` ([07](07-local-block-and-fold-pass.md)) — runtime references allowed (the list is typically `each.value` from an outer loop or a literal list).
3. Mutual exclusion: error if `for_each` or `count` also set.
4. Add a step-level **bound**: `step.parallel_max` optional integer attribute capping concurrent goroutines. Default: `runtime.GOMAXPROCS(0)`. Document the default; tests pin both default and explicit cap.

### Step 3 — Engine concurrency primitive

Replace the scheduler TODO at [internal/engine/node.go:47](../../internal/engine/node.go#L47) with a bounded fan-out:

```go
// runParallelIteration runs the step body once per list item with bounded
// concurrency. Each iteration runs in its own goroutine with a fresh
// each.* binding. Errors are aggregated; first error short-circuits the
// remaining iterations IF on_failure = "abort" (default), otherwise all
// iterations complete and errors are collected.
func runParallelIteration(ctx context.Context, n *workflow.StepNode, items []cty.Value, st *RunState, deps Deps) ([]IterationResult, error)
```

Implementation:

1. Bounded channel (semaphore) of size `n.ParallelMax`.
2. Per-item goroutine. Acquire semaphore, run, release.
3. Each goroutine gets a forked `RunState` with its own `each.*` binding. Share `Vars` (read-only) and `SharedVarStore` ([18-shared-variable-block.md](18-shared-variable-block.md)) — the store's mutex serializes writes.
4. Collect results in declaration order (use the index, not channel arrival order).
5. On context cancellation, all goroutines see ctx.Done() and exit.

`on_failure` semantics:

- `abort` (default): on first failure, cancel the per-iteration ctx for outstanding goroutines; return the first error.
- `continue`: collect all results; success/failure per item; the step's overall outcome is the worst (failure if any failed).
- `ignore`: collect all results; the step always reports success regardless of per-item outcomes.

### Step 4 — Output aggregation

The per-iteration outputs aggregate to `steps.<name>.<key>` as a **list** keyed by index. Mirrors the existing `for_each` aggregation; reuse the helper.

### Step 5 — Adapter session sharing

Adapter sessions are scope-bound ([12-adapter-lifecycle-automation.md](12-adapter-lifecycle-automation.md)). Parallel iterations of an adapter step **share the same session** by default. Adapter implementations must be safe for concurrent calls — document this clearly in the [docs/plugins.md](../../docs/plugins.md) adapter-author guide.

If an adapter is not concurrency-safe, the workflow author should set `parallel_max = 1` (effectively serializing — same as `for_each`). Optionally, future work could add `parallel_session = "per_iteration"` to spawn a fresh session per item; out of scope for v0.3.0.

### Step 6 — Subworkflow parallelism

Parallel iterations targeting `subworkflow.<name>` spawn fresh subworkflow scopes per item. This is the **expected** use case (fan-out work into isolated subworkflow runs). Each subworkflow has its own `SharedVarStore`, own adapter sessions, own var seeding.

### Step 7 — Tests

- `workflow/compile_steps_iteration_test.go`:
  - `TestStep_ParallelMutualExclusion_ForEach_Error`.
  - `TestStep_ParallelMutualExclusion_Count_Error`.
  - `TestStep_ParallelMaxAttribute_CompilesAndCaps`.
  - `TestStep_ParallelExpressionFolds`.

- `internal/engine/parallel_iteration_test.go`:
  - `TestParallelIteration_DefaultMax_RunsConcurrently` (use a sync barrier to assert N goroutines reach a given point simultaneously up to ParallelMax).
  - `TestParallelIteration_BoundedByParallelMax`.
  - `TestParallelIteration_AbortOnFirstFailure`.
  - `TestParallelIteration_ContinueOnFailure`.
  - `TestParallelIteration_IgnoreOnFailure`.
  - `TestParallelIteration_OutputAggregationOrder`.
  - `TestParallelIteration_ContextCancellation`.

- End-to-end: `examples/phase3-parallel/` — a workflow that parallel-fetches three items via subworkflow.

### Step 8 — Validation

```sh
go build ./...
go test -race -count=20 ./internal/engine/...   # high count for race-detector pressure
go test -race -count=2 ./...
make validate
make ci
```

`-count=20` on engine tests is mandatory: parallel code must hold under race-detector pressure.

## Behavior change

**Behavior change: yes — additive.**

Observable differences:

1. New step modifier `parallel = [...]` runs the step concurrently across items.
2. New `parallel_max = N` cap.
3. Workflows without `parallel` modifier behave identically to v0.2.0.

No proto change. No SDK change.

## Reuse

- Existing `for_each`/`count` iteration cursor and binding plumbing in [internal/engine/runtime/](../../internal/engine/runtime/).
- Existing `IterCursor`, `WithEachBinding`, `EachBinding`, `routeIteratingStepInGraph`, `finishIterationInGraph` — extend, do not duplicate.
- `FoldExpr` from [07](07-local-block-and-fold-pass.md).
- `SharedVarStore` from [18-shared-variable-block.md](18-shared-variable-block.md) — its mutex serializes parallel writes naturally.
- `runSubworkflow` from [13-subworkflow-block-and-resolver.md](13-subworkflow-block-and-resolver.md) — invoked per parallel item when target is a subworkflow.

## Out of scope

- Per-iteration adapter sessions. Default is shared session. Out of scope for v0.3.0.
- Distributed parallelism across hosts. Single-process only.
- Result aggregation beyond list-by-index. No "fold" or "reduce" operators.
- Streaming partial results to the next step. The next step waits for all parallel items to complete.
- Dynamic `parallel_max` from runtime expressions. Compile-time literal or var.* only.

## Files this workstream may modify

- [`workflow/schema.go`](../../workflow/schema.go) — `Parallel hcl.Expression`, `ParallelMax int`.
- `workflow/compile_steps_iteration.go` — extend with parallel handling.
- [`internal/engine/node.go`](../../internal/engine/node.go) — replace scheduler TODO.
- New: `internal/engine/parallel_iteration.go`.
- [`internal/engine/node_step.go`](../../internal/engine/node_step.go) — dispatch parallel iterations.
- New tests.
- New: [`examples/phase3-parallel/`](../../examples/).
- [`docs/workflow.md`](../../docs/workflow.md) — parallel section, including the adapter-concurrency note.
- [`docs/plugins.md`](../../docs/plugins.md) — adapter-author concurrency-safety guidance.

This workstream may **not** edit:

- `PLAN.md`, `README.md`, `AGENTS.md`, `CHANGELOG.md`, `workstreams/README.md`, or any other workstream file.
- `.proto` files.

## Tasks

- [ ] Schema (Step 1).
- [ ] Compile validation (Step 2).
- [ ] Engine concurrency primitive (Step 3).
- [ ] Output aggregation (Step 4).
- [ ] Adapter session sharing semantics + doc (Step 5).
- [ ] Subworkflow parallelism (Step 6).
- [ ] Tests (Step 7).
- [ ] `make ci` green at `-count=20` (Step 8).

## Exit criteria

- `parallel = [...]` compiles and runs concurrently up to `parallel_max`.
- Mutually-exclusive errors with `for_each` / `count`.
- `on_failure` modes: abort / continue / ignore work as documented.
- Output aggregation maintains declaration index order.
- Race-detector tests at `-count=20` pass.
- Adapter concurrency guidance documented.
- All required tests pass.
- `make ci` exits 0.

## Tests

The Step 7 list. Coverage: parallel-iteration path ≥ 85%; the bounded-fan-out logic ≥ 95%.

## Risks

| Risk | Mitigation |
|---|---|
| Adapter assumes single-threaded execution and panics under parallel calls | Document concurrency requirement in [docs/plugins.md](../../docs/plugins.md). Workflow authors who hit this set `parallel_max = 1`. Future work can add per-iteration sessions. |
| Subworkflow scopes spawned in parallel exhaust the file-descriptor budget for adapter subprocesses | The semaphore caps active iterations. Default `GOMAXPROCS` is conservative on most machines. Document the trade-off. |
| Race detector finds a regression in [`SharedVarStore`](18-shared-variable-block.md) under parallel writes | The store's mutex is exactly the serialization point. Confirm with `TestSharedVar_ParallelWritesSerialize`. |
| Output aggregation order is non-deterministic if collected by channel arrival | Use index-keyed slice, not channel arrival. Test `TestParallelIteration_OutputAggregationOrder`. |
| `parallel_max = 0` is ambiguous (unlimited? error?) | Reject 0 at compile; require ≥ 1 or the default. Test `TestStep_ParallelMaxZero_Error`. |
| Context cancellation propagation leaks goroutines | Every per-iteration goroutine listens on `ctx.Done()`. Use `errgroup.WithContext` for cancellation discipline. Add `goleak.VerifyNone(t)` in TestMain. |
