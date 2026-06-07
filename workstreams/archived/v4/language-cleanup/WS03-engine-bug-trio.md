# WS03 — Engine bug trio: null panic, terminal-state discarded, stale DataStore snapshot

**Phase:** Language Cleanup · **Track:** Engine · **Owner:** Workstream executor · **Depends on:** [WS02](WS02-data-and-outcome-semantics.md) (DataStore, `applyDataWrites`, `SeedDataSnapshot` must be present). · **Unblocks:** reliable subworkflow execution semantics for adapter-v2. · **Base branch:** `main`

## Context

Three correctness bugs in the subworkflow execution path compounded each other while building the most recent stable workflows:

1. **Null panic** — `evaluateSubworkflowStep` in [node_step.go](../../internal/engine/node_step.go) checks `v.IsKnown() && v.Type() == cty.String` before calling `v.AsString()`, but in go-cty a null string satisfies both guards while `AsString()` panics. Any subworkflow output whose `data "internal"` variable was never written (typed-null with no default) triggers this at runtime.

2. **Terminal state discarded** — `runSubworkflow` in [node_subworkflow.go](../../internal/engine/node_subworkflow.go) returns `(outputs, nil)` regardless of which terminal state the callee reached. The caller in [node_step.go](../../internal/engine/node_step.go) sets `outcome = "failure"` only when `runErr != nil`, so a subworkflow whose terminal state has `success = false` is incorrectly reported to the parent as a success.

3. **Stale DataStore snapshot** — `runWorkflowBody` in [node_workflow.go](../../internal/engine/node_workflow.go) returns `childSt.Vars` on `ErrTerminal` without first flushing the DataStore snapshot into it. `applyDataWrites` writes to `DataStore` but does not update `st.Vars`; the snapshot is only refreshed at step entry via `SeedDataSnapshot`. A write performed in the last step before the terminal state is invisible to output evaluation in the parent, causing output expressions against `data.internal.*` to see stale (pre-write) values.

Fixing all three together is correct: they share the same execution path (`runSubworkflow` → `runWorkflowBody` → step entry/exit), the tests form a coherent suite, and the null-panic fix (bug 1) prevents masking the output of bugs 2 and 3 during testing.

## Prerequisites

- WS02 merged (`DataStore`, `SeedDataSnapshot`, `applyDataWrites` all present).

## In scope

### Step 1 — Null guard in `evaluateSubworkflowStep`

**File:** [internal/engine/node_step.go](../../internal/engine/node_step.go) (~line 547)

**Before:**
```go
if v.IsKnown() && v.Type() == cty.String {
    stringOutputs[k] = v.AsString()
    continue
}
```

**After:**
```go
if v.IsKnown() && !v.IsNull() && v.Type() == cty.String {
    stringOutputs[k] = v.AsString()
    continue
}
```

Null strings fall through to `renderCtyValue` (in [eval_run_outputs.go](../../internal/engine/eval_run_outputs.go)), which already handles null by returning `"null"`. The one-line guard change is the complete fix.

### Step 2 — Propagate terminal state through `runSubworkflow`

**File:** [internal/engine/node_subworkflow.go](../../internal/engine/node_subworkflow.go)

Change `runSubworkflow` to return the terminal state name alongside outputs:

```go
// before
func runSubworkflow(...) (map[string]cty.Value, error)

// after
func runSubworkflow(...) (map[string]cty.Value, string, error)
```

Return `terminal` (already available from `runWorkflowBody`) as the second return value. The `ReturnSentinel` branch already has the correct terminal name; normal terminal-state branches pass it through.

**File:** [internal/engine/node_step.go](../../internal/engine/node_step.go) (~lines 532–537)

Update the caller to check terminal state success:

```go
outputs, terminalState, runErr := runSubworkflow(ctx, swNode, st, stepInput, deps)

outcome := "success"
if runErr != nil || !swNode.Body.States[terminalState].Success {
    outcome = "failure"
}
```

The `ReturnSentinel` case: `runSubworkflow` returns `ReturnSentinel` as `terminalState` when the callee used `next = return`; `swNode.Body.States[ReturnSentinel]` will be absent so `States[terminalState].Success` returns the zero value `false`. Guard this: treat `ReturnSentinel` as a success (it is not a terminal failure) by checking the sentinel before the `States` map lookup.

### Step 3 — Flush DataStore snapshot on terminal exit

**File:** [internal/engine/node_workflow.go](../../internal/engine/node_workflow.go) (~line 152)

**Before:**
```go
if errors.Is(err, engineruntime.ErrTerminal) {
    return childSt.Current, nil, childSt.Vars, nil
}
```

**After:**
```go
if errors.Is(err, engineruntime.ErrTerminal) {
    if childSt.DataStore != nil {
        childSt.Vars = workflow.SeedDataSnapshot(childSt.Vars, childSt.DataStore.Snapshot())
    }
    return childSt.Current, nil, childSt.Vars, nil
}
```

This mirrors the pattern already used at step entry in [node_step.go:45](../../internal/engine/node_step.go) and ensures output expressions evaluated by the parent see the data written by the last step before the terminal state.

### Step 4 — Tests

New test cases in the existing engine test files:

- `node_subworkflow_test.go`:
  - Subworkflow that writes a null-string output does not panic; output key receives `"null"` string.
  - Subworkflow reaching a `success = false` terminal state produces `outcome = "failure"` in the parent step.
  - Subworkflow reaching a `success = true` terminal state produces `outcome = "success"` in the parent step (regression guard).

- `node_workflow_test.go` or a new `data_subworkflow_test.go`:
  - Subworkflow where last step writes `data.internal.x.value` before reaching the terminal state; parent output expression `data.internal.x.value` evaluates to the written value (not stale empty string).

All existing tests must continue to pass.

## Out of scope

- Changes to subworkflow `return`-sentinel semantics beyond the guard added in Step 2.
- Multi-level subworkflow nesting (the fix applies at every boundary; no additional work needed).
- Language surface changes — this is a pure engine fix.

## Reuse pointers

- [`renderCtyValue`](../../internal/engine/eval_run_outputs.go) — existing null-handling fallback used by Step 1.
- [`SeedDataSnapshot`](../../workflow/eval.go) — existing function reused verbatim in Step 3.
- [`runWorkflowBody`](../../internal/engine/node_workflow.go) — already returns `(terminal string, returnOutputs, finalVars map[string]cty.Value, err error)`; no signature change needed.

## Behavior change

**User-facing:** subworkflows with null string outputs no longer panic. Subworkflows that exit via a `success = false` terminal state now correctly propagate failure to the parent step outcome. Data writes in the last step before a terminal state are now visible to the parent's output projections.

**Runtime semantics:** unchanged for all currently-working workflows. The null guard and snapshot flush are no-ops when null values are absent and DataStore is empty.

## Tests required

- All existing engine tests pass after changes.
- New tests in Step 4 pass.
- `go vet ./...` clean.
- Manual: run a workflow containing a subworkflow with a `success = false` terminal state; parent step outcome is `failure`.
- Manual: run a workflow containing a subworkflow that writes a data block value in its last step; parent output projection reads the written value.

## Implementation Notes

### Checklist

- [ ] Step 1 — Null guard in `evaluateSubworkflowStep`
- [ ] Step 2 — Return terminal state from `runSubworkflow`; update caller
- [ ] Step 3 — Flush DataStore snapshot on terminal exit
- [ ] Step 4 — Tests

### Reviewer Notes

_To be filled in during review._
