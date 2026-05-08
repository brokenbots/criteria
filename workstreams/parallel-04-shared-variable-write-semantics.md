# parallel-04 — Shared variable write semantics for parallel steps

**Owner:** Workstream executor · **Depends on:** parallel-01 and parallel-02 (for accurate docs) · **Coordinates with:** none

## Context

`aggregateParallelResults` applies `shared_writes` from per-iteration outcomes
**after all goroutines complete**, iterating over results **in declaration
order** (index 0, 1, 2, …). The writes from each iteration are applied
serially by calling `applyIterationSharedWrites` → `applySharedWrites` →
`SharedVarStore.SetBatch`.

Before any goroutine launches, the engine takes a snapshot of the current
variable state. Every goroutine reads from this same snapshot — there is no
live-read of updated values between goroutines. This means:

1. **Last-index-wins**: if iteration 0, 1, and 2 all write `counter`, the
   final value is iteration 2's value, regardless of goroutine completion order.
2. **Accumulation is broken**: a pattern like "read `shared.counter`, add 1,
   write it back" will not work — all goroutines read the same pre-parallel
   value and each overwrites with `initial + 1`, not `initial + N`.
3. **Order is deterministic**: even though goroutines complete in arbitrary
   order, writes are applied in index order. This is intentional and correct.

The current code is **correct** — the behavior is deterministic and documented
nowhere. The fix is twofold:

1. **Compile-time warning** when a `parallel` step's per-iteration outcome
   declares `shared_writes`. This guides authors toward using aggregate outcomes
   with an explicit `output = { ... }` projection (where the accumulation
   is done in the projection expression) rather than relying on serial
   per-iteration writes.
2. **Docs update** in `docs/workflow.md`: add a "shared variables in parallel
   steps" section explaining the snapshot semantics and the warning.

The docs also contain a stale sentence (accurate before parallel-01/02)
about session handles being shared across parallel iterations. After parallel-01
and parallel-02 land, that sentence needs updating.

## Prerequisites

- parallel-01 and parallel-02 are merged (for accurate session-sharing docs).
- `make test` passes on the merge of parallel-01 and parallel-02.

The compile warning itself (`Step 1`) is independent — it can be implemented
before parallel-01/02 if needed. The docs section (`Step 2`) should be
written after parallel-01/02 land so the session-sharing statement is accurate.

## In scope

### Step 1 — Compile warning for parallel + per-iteration shared_writes

**File:** `workflow/compile_steps_iteration.go`

Add a `DiagWarning` after `compileOutcomeBlock` runs (line ~90). Check every
compiled outcome on a `parallel` step: if the outcome routes to `_continue`
(per-iteration) and declares `SharedWrites`, emit a warning:

```go
// Warn when a parallel step's per-iteration outcomes use shared_writes.
// Goroutines read a pre-parallel snapshot; writes are applied in index order
// after all iterations complete. Accumulation (counter++) is not safe.
// Authors should use aggregate outcomes with output = { ... } projection
// for parallel shared variable writes.
if parallelExpr != nil {
    for outcomeName, co := range node.Outcomes {
        if co.Next == "_continue" && len(co.SharedWrites) > 0 {
            diags = append(diags, &hcl.Diagnostic{
                Severity: hcl.DiagWarning,
                Summary: fmt.Sprintf(
                    "step %q outcome %q: shared_writes on a parallel step's per-iteration outcome "+
                        "are applied in index order after all iterations complete. "+
                        "All goroutines read a pre-parallel snapshot, so accumulation patterns "+
                        "(e.g. reading shared.x and writing back x+1) are not safe. "+
                        "Last-index-wins applies when multiple iterations write the same variable. "+
                        "Consider using an aggregate outcome with output = { ... } projection.",
                    sp.Name, outcomeName),
            })
        }
    }
}
```

Place this block immediately after the `compileOutcomeBlock` and
`validateIteratingOutcomes` calls, before the `g.Steps[sp.Name] = node`
assignment.

Notes:
- `"_continue"` is the per-iteration continuation sentinel (no constant is
  defined in the workflow package — use the string literal, consistent with
  existing uses in `compile_steps_graph.go` and `compile.go`).
- This is a `DiagWarning`, not `DiagError` — the behavior is deterministic
  and valid; the warning is guidance.
- `for_each` and `count` iterating steps do NOT get this warning — for sequential
  iteration, per-iteration `shared_writes` are applied in order after each
  iteration completes (not in a post-goroutine aggregation pass), so the
  semantics are clear.

---

### Step 2 — Update `docs/workflow.md`

**File:** `docs/workflow.md`

**2a.** In the `### parallel — run iterations concurrently` section, add a
sub-section **"Shared variables in `parallel` steps"** after the existing
`**Adapter concurrency requirements**` paragraph. Content:

```markdown
**Shared variables in `parallel` steps:**

When a `parallel` step's per-iteration outcomes declare `shared_writes`, the
engine applies them **after all iterations complete**, in declaration order
(index 0, 1, 2, …). Every goroutine reads a **snapshot of shared variables
taken before any goroutine starts** — there is no live-read between goroutines.

Consequences:

- **Last-index-wins**: when multiple iterations write the same variable, the
  value after the step is the value written by the highest-index iteration that
  reached that outcome.
- **Accumulation is broken**: a pattern that reads `shared.counter`, increments
  it, and writes it back will not produce `initial + N` — every goroutine reads
  the same snapshot value, so the result is `initial + 1` regardless of N.

For safe parallel accumulation, collect results into indexed outputs and compute
the final value in an aggregate outcome's `output = { ... }` projection:

<!-- validator: fragment -->
```hcl
step "fetch_all" {
  target       = adapter.noop.default
  parallel     = var.items
  parallel_max = 4

  outcome "success" {
    next = "_continue"
    # No shared_writes here — collect in aggregate
  }

  # After all goroutines complete, aggregate in the output projection.
  outcome "all_succeeded" {
    next   = "done"
    output = {
      total = length(steps.fetch_all.outputs)
    }
    shared_writes = { item_count = "total" }
  }
}
```

The compiler emits a warning when `shared_writes` appears on a `parallel`
step's per-iteration outcome (`next = "_continue"`).
```

**2b.** Update the stale sentence in the same `parallel` section. After
parallel-01 and parallel-02 land, the following sentence is no longer accurate:

> Session handles (from `OpenSession`) are shared across parallel iterations for
> the same step; adapter authors should treat them as read-only or protect writes.

Replace with:

```markdown
Adapters that are safe for concurrent `Execute` calls must declare the
`"parallel_safe"` capability in their `InfoResponse.Capabilities`. The engine
rejects `parallel = [...]` steps that target an adapter lacking this
declaration — at compile time when the adapter binary is resolvable, at runtime
otherwise. See [docs/plugins.md](plugins.md) for details on declaring
capabilities.

Subworkflow steps that use `parallel` receive fully isolated adapter sessions
per iteration — each goroutine's subworkflow opens and closes its own sessions
independently.
```

---

### Step 3 — Tests

**File:** `workflow/compile_steps_iteration_test.go`

```
TestStep_Parallel_PerIterationSharedWrites_Warning
```
- A `parallel` step with an `outcome "success" { next = "_continue"; shared_writes = { ... } }` block
  → compile returns exactly one `DiagWarning` with the correct summary.

```
TestStep_ForEach_PerIterationSharedWrites_NoWarning
```
- Same step shape but with `for_each` instead of `parallel`
  → no warning emitted.

```
TestStep_Parallel_AggregateSharedWrites_NoWarning
```
- A `parallel` step with `shared_writes` only on `all_succeeded` / `any_failed`
  (not `_continue`) → no warning.

---

## Behavior change

**Yes (compile-time only).** Existing parallel workflows that declare
`shared_writes` on `_continue` outcomes will now produce a `DiagWarning` at
compile time. The runtime behavior is unchanged — semantics are as they were
before this workstream.

Authors who see the warning and do nothing are unaffected (warnings do not
fail the compile). The warning is guidance to move toward safe patterns.

## Reuse

- The `"_continue"` check pattern already appears in `compile_steps_graph.go`
  line 47 (`isAggregateIter := isIter && o.Next != "_continue"`) and in
  `compile.go` line 183.
- The diagnostic pattern follows existing `DiagWarning` uses throughout the
  compiler (e.g. missing `any_failed` outcome).

## Out of scope

- Changing the runtime aggregation semantics — the serial index-order apply is
  correct and should not be changed.
- Changing per-iteration `shared_writes` to be visible to subsequent goroutines
  (would require a shared mutex on the var store snapshot; not requested).
- Adding this warning to `for_each` or `count` steps — their sequential
  semantics are clear and accumulation works correctly.
- Any changes to `aggregateParallelResults` or `applyIterationSharedWrites`.

## Files this workstream may modify

- `workflow/compile_steps_iteration.go`
- `workflow/compile_steps_iteration_test.go`
- `docs/workflow.md`

This workstream may **not** edit `README.md`, `PLAN.md`, `AGENTS.md`,
`CHANGELOG.md`, `CONTRIBUTING.md`, `workstreams/README.md`, `sdk/CHANGELOG.md`,
or any other workstream file.

## Tasks

- [ ] Add compile warning for per-iteration `shared_writes` on `parallel` steps in `compileIteratingStep`
- [ ] Write `TestStep_Parallel_PerIterationSharedWrites_Warning` test
- [ ] Write `TestStep_ForEach_PerIterationSharedWrites_NoWarning` test
- [ ] Write `TestStep_Parallel_AggregateSharedWrites_NoWarning` test
- [ ] Add "Shared variables in `parallel` steps" section to `docs/workflow.md` (after parallel-01/02 merge)
- [ ] Update the stale session-sharing sentence in `docs/workflow.md` (after parallel-01/02 merge)
- [ ] Run `make test && make validate` and confirm green

## Exit criteria

- `go test ./workflow/...` passes.
- `TestStep_Parallel_PerIterationSharedWrites_Warning`: one `DiagWarning`
  emitted; summary contains `"parallel"` and `"shared_writes"`.
- `TestStep_ForEach_PerIterationSharedWrites_NoWarning`: no warning emitted.
- `TestStep_Parallel_AggregateSharedWrites_NoWarning`: no warning emitted.
- `make validate` passes (example workflows all validate).
- `docs/workflow.md` accurately describes snapshot-at-entry and last-index-wins
  semantics for `parallel` + `shared_writes`.
