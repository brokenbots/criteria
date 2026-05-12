# ADR-0002 — `while` Step Iteration Modifier

**Status:** Accepted

**Date:** 2026-05-11

---

## Context

Criteria workflows today support three step-level iteration modifiers:

- `for_each = <list-or-map>` — sequential iteration over a known collection.
- `count = <integer>` — sequential iteration exactly N times.
- `parallel = <list>` — concurrent fan-out over a fixed list.

A common pattern is to **iterate while a condition holds**: drain a queue until
empty, retry until success, poll until ready. Today users approximate this with
`count = N` and an early-exit outcome, or by chaining single-shot steps in a
loop. Both approaches are awkward and require knowing (or guessing) an upper
bound.

This ADR adds `while = <bool expression>` as a fourth iteration modifier.

---

## Decision

### Syntax

```hcl
step "drain" {
  while      = shared.queue_depth > 0
  max_visits = 10            # recommended safety backstop
  target     = adapter.shell.worker
  input { cmd = "process-one" }

  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "failed" }

  outcome "ok" {
    next          = "_continue"
    shared_writes = { queue_depth = shared.queue_depth - 1 }
  }
}
```

### Semantics

1. **Pre-iteration evaluation.** The `while` expression is evaluated against the
   live eval context (including current `shared.*` values) **before** each
   iteration. If `true`, one iteration runs. If `false`, the loop exits.

2. **Zero-iteration case.** When the condition is false on first entry, no
   iterations run and the step exits via the `all_succeeded` aggregate outcome.

3. **Expression type.** Must evaluate to `cty.Bool`. Compile error if the
   expression can be statically determined to be non-bool; runtime error if
   evaluation yields a non-bool, null, or unknown value.

4. **`while.*` namespace.** Each iteration exposes:
   - `while.index` — zero-based iteration counter.
   - `while.first` — `true` on the first iteration.
   - `while._prev` — output of the previous iteration (`cty.NilVal` before the
     first).

5. **Aggregate outcomes.** `all_succeeded` (required) and `any_failed`
   (recommended) work identically to `for_each` and `count`. `on_failure`
   modes (`continue` / `abort` / `ignore`) work identically.

6. **`IterCursor.Total = -1` sentinel.** The cursor's `Total` field uses `-1`
   to signal an unbounded `while` loop. The JSON wire encoding of the integer
   handles this naturally. The `IsWhile()` method (`Total < 0`) distinguishes
   while-cursors from for_each/count cursors.

7. **Crash-resume.** The cursor's `Index` and `AnyFailed` fields are persisted
   as normal. On resume, the engine re-evaluates the `while` condition with the
   restored `shared.*` state; if still true, the next iteration runs.

8. **Mutual exclusion with `parallel`.** `while` and `parallel` are mutually
   exclusive. Concurrent `while` would require a different synchronisation
   model (e.g. bounded fan-out with an external counter), which is out of scope
   for v1. `while` is also mutually exclusive with `for_each` and `count`.

9. **Safety.** `policy.max_total_steps` (default 100) is the absolute backstop.
   Setting `max_visits` on the step is the recommended additional guard. The
   compiler emits a back-edge warning when `max_total_steps` exceeds
   `max_visits_warn_threshold` and the step has no `max_visits`.

---

## Consequences

### `shared.*` re-evaluation

Each iteration rebuilds the eval context with the current `shared.*` snapshot
via `SeedSharedSnapshot`. The `while` condition expression is evaluated against
this fresh context, so mutations from prior iterations (via `shared_writes`) are
visible on the next condition check.

### Crash-resume

The cursor is serialised with `Total = -1` (unbounded). On engine restart, the
cursor is restored from the JSON scope blob; `IsWhile()` returns `true`. The
engine re-evaluates the `while` expression before running the next iteration
rather than advancing through a pre-loaded items slice.

### Parallel-mode incompatibility

`while` and `parallel` are mutually exclusive. Reasons:

1. A `while` loop is inherently sequential: the condition depends on shared
   state that changes per iteration. Running goroutines concurrently would
   require an external coordinator (a counter, a queue handle) that is outside
   the scope of the iteration modifier itself.
2. Users who need concurrent draining can model it as `for_each` over a
   fixed-size list, or use `parallel` with a separate orchestration step.

### Runaway risk

An unconstrained `while = true` loop will hit `policy.max_total_steps`. Authors
are encouraged to set `max_visits` as an explicit per-step limit. The compiler
warns when a loop has no `max_visits` and `max_total_steps` is high.

---

## Alternatives considered

1. **`while { condition = ...; max_iterations = ... }` block.**
   Rejected: the inline block is more verbose. The `max_visits` attribute and
   `policy.max_total_steps` already provide the bound mechanisms; an additional
   `max_iterations` block would duplicate them.

2. **`do { ... } until ...` (post-iteration evaluation).**
   Rejected: pre-iteration evaluation matches user expectation from mainstream
   languages (`while`, `for`) and is easier to reason about for the
   zero-input (empty queue) case: no iterations run and the aggregate outcome
   fires immediately, which is the safe default.

3. **Macro-expand `while` to `count = max_visits` with an early-exit outcome.**
   Rejected: `count` semantics force a known upper bound, which `while`
   deliberately avoids. A macro expansion would require `max_visits` to be
   mandatory; the current design treats it as a recommended (but optional)
   safety guard.

---

## Related

- `for_each` / `count` / `parallel` — existing iteration modifiers.
- `IterCursor` — shared runtime cursor for all iteration types.
- W19 PR 88 — `isIter` predicate and `runStepFromAttempt` lessons applied here.
