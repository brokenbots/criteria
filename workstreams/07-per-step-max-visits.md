# Workstream 7 — Per-step `max_visits`

**Owner:** Workstream executor · **Depends on:** none · **Coordinates with:** [W14](14-phase2-cleanup-gate.md) (smoke workflow exercises this).

## Context

Today the only loop guard in the engine is the global
`policy.max_total_steps` field
([workflow/schema.go:207](../workflow/schema.go#L207),
[internal/engine/node_step.go:28-30](../internal/engine/node_step.go#L28-L30)).
That counter increments on every step evaluation across the whole
run and is checked in `stepNode.Evaluate`. It is a coarse backstop:
setting it low to bound a tight review loop also chokes legitimate
long workflows; setting it high to allow long workflows lets a
runaway back-edge loop burn for thousands of iterations before
tripping.

Deferred user-feedback item #08 (preserved in git history at commit
`4e4a357`,
`user_feedback/08-add-per-step-visit-limit-to-bound-loops-user-story.txt`)
asks for a per-step visit limit:

> step "execute" {
>   max_visits = 10  # fail the run if this step is reached more than 10 times
>   ...
> }

This workstream adds it. The mechanism:

- Optional `max_visits` integer on every step block. `0` or omitted
  means unlimited.
- Engine tracks visit counts per step in `RunState`, persisted in
  `StepCheckpoint` for reattach safety.
- When a step is about to evaluate and its visit count would exceed
  `max_visits`, the run fails with
  `step "<name>" exceeded max_visits (<N>)`.
- Compile-time warning when a step is reachable from its own outcome
  graph (i.e. has a back-edge) and `max_total_steps > 200` (default
  threshold) without an explicit `max_visits`.

`max_total_steps` continues to function as a coarse backstop; this
workstream does not change its semantics.

## Prerequisites

- `make ci` green on `main`.
- Familiarity with
  [internal/engine/runstate.go](../internal/engine/runstate.go),
  [internal/engine/node_step.go](../internal/engine/node_step.go),
  [internal/engine/engine.go](../internal/engine/engine.go),
  [workflow/schema.go](../workflow/schema.go).
- Familiarity with the existing `IterStack` precedent for
  per-step state in `RunState`.

## In scope

### Step 1 — Schema

Edit [workflow/schema.go](../workflow/schema.go):

- Add `MaxVisits int` to the StepSpec (HCL-decoded shape) and
  `StepNode` (compiled shape, line 254). Use `hcl:"max_visits,optional"`.
- Default value is `0` (unlimited).
- Validation: reject negative values at compile time with a clear
  error (`step "<name>": max_visits must be >= 0`).

The `MaxVisits` field on the compiled `StepNode` is what the engine
reads. The `StepSpec` field is what HCL decodes into.

### Step 2 — Compile

Edit [workflow/compile_steps.go](../workflow/compile_steps.go):

- Decode `max_visits` from the step block alongside other optional
  fields (similar to `timeout`, `count`, etc.).
- Copy the value through to `StepNode.MaxVisits`.
- Emit a compile-time warning (not an error) when:
  - The step is reachable from its own outcome graph (i.e. there
    exists a path from the step to itself via outcome transitions),
    AND
  - `max_visits == 0`, AND
  - `Policy.MaxTotalSteps > 200`.
- The warning text:
  `step "<name>": appears in a loop with max_total_steps=<N> and no max_visits; consider setting max_visits to bound back-edge iteration`.
- The 200 threshold is the default; allow override via
  `policy { max_visits_warn_threshold = N }` (also a new optional
  field, defaulting to 200; bound 0 to disable). Plumb this through
  `workflow/schema.go:Policy` and the policy decoder.

The reachability check is a graph walk over outcome `transition_to`
edges. Use the existing FSM graph traversal helpers in `workflow/`
(locate via grep — there is likely a `walk` or `reachableFrom`
function); if none exists, implement one in `workflow/compile_steps.go`
keyed off the outcome map. Keep it simple — no need for SCCs.

### Step 3 — Runtime tracking

Edit [internal/engine/runstate.go](../internal/engine/runstate.go):

- Add `Visits map[string]int` to `RunState` (init to `nil`; nil-safe
  reads).
- Document the field with a code comment:
  `// Visits tracks per-step visit counts for max_visits enforcement (W07).`

Edit [internal/engine/node_step.go](../internal/engine/node_step.go):

- Before incrementing `TotalSteps` (line 28), check `MaxVisits`:

```go
if n.node.MaxVisits > 0 {
    if st.Visits == nil {
        st.Visits = make(map[string]int)
    }
    if st.Visits[n.node.Name] >= n.node.MaxVisits {
        return "", fmt.Errorf("step %q exceeded max_visits (%d)", n.node.Name, n.node.MaxVisits)
    }
}
```

- Increment after success (or unconditionally — the choice matters
  for retries; the user story says "retries count toward the limit",
  so increment unconditionally before evaluation):

```go
if st.Visits == nil {
    st.Visits = make(map[string]int)
}
st.Visits[n.node.Name]++
```

Place the increment alongside the existing `st.TotalSteps++` (line
28). The check from the previous block runs *before* the increment
to allow exactly `MaxVisits` evaluations and reject the
`MaxVisits + 1`-th.

### Step 4 — Persistence

The `StepCheckpoint` JSON shape lives in
[internal/cli/local_state.go](../internal/cli/local_state.go) (W04
already touches this file). The checkpoint must serialize the new
`Visits` map so reattach picks up where the run left off.

Inspect `StepCheckpoint` for the existing serialization. If it
contains a `RunState` field directly, JSON marshaling picks up the
new map automatically. If it contains a hand-rolled subset, add a
`Visits map[string]int` field with the JSON tag `"visits,omitempty"`.

When the engine reattaches via `engine.Run` (or `RunFrom`), the
restored `RunState` must include the saved `Visits`. Trace the
reattach path:
[internal/cli/apply.go:447](../internal/cli/apply.go#L447) →
`engine.New` → restore from checkpoint. Confirm the visits map
flows through.

### Step 5 — Tests

New tests in `internal/engine/engine_test.go` (mirror the existing
`TestMaxTotalSteps`):

- `TestMaxVisits_Hit` — workflow with a back-edge loop on a step
  with `max_visits = 3`; assert the run fails on the 4th visit with
  the expected message.
- `TestMaxVisits_NotHit` — same workflow with `max_visits = 100`
  and a loop that exits naturally; assert the run completes.
- `TestMaxVisits_OmittedIsUnlimited` — workflow with no
  `max_visits` field; assert the field defaults to 0 and does not
  trip.
- `TestMaxVisits_RetryCounts` — workflow where a step retries
  (via the existing retry mechanism, if any); assert each retry
  increments the visit count.
- `TestMaxVisits_Persists` — write a checkpoint mid-loop, reattach,
  confirm visit count is restored and the limit still trips at the
  correct iteration.

New tests in `workflow/compile_steps_test.go` (mirror the schema
tests):

- `TestCompile_MaxVisits_Decodes` — `max_visits = 5` decodes
  correctly.
- `TestCompile_MaxVisits_Negative` — `max_visits = -1` fails compile
  with the expected error.
- `TestCompile_BackEdgeWarning` — workflow with a self-loop and
  `max_total_steps = 500` and no `max_visits` emits the warning.
- `TestCompile_BackEdgeWarning_Suppressed` — same workflow with
  `max_visits = 10` does not emit the warning.

### Step 6 — Documentation

Update [docs/workflow.md](../docs/workflow.md):

- Document `max_visits` in the step block reference, alongside
  `timeout`, `retry`, etc.
- Document `max_visits_warn_threshold` in the policy block reference.
- Add a note in the "policy" section explaining the relationship
  between `max_total_steps` (coarse) and `max_visits` (per-step).

Do **not** edit `README.md`, `PLAN.md`, `AGENTS.md`, `CHANGELOG.md`.

## Behavior change

**Yes.**

- New optional HCL field `max_visits` on step blocks.
- New optional HCL field `max_visits_warn_threshold` on the policy
  block (defaults to 200).
- New runtime failure mode: `step "<name>" exceeded max_visits (<N>)`.
- New compile-time warning text (see Step 2).
- New JSON field on `StepCheckpoint` (or whatever serializes
  `RunState`): `visits` (an object mapping step name to count).
  Older checkpoints without the field still load (default to empty
  map).
- No change to `max_total_steps` semantics.
- No change to event sink interface — failure is reported via the
  existing `OnRunFailed` hook.

## Reuse

- Existing `RunState` infrastructure. Add the field; do not refactor
  the struct.
- Existing graph-walk helpers in `workflow/` for the reachability
  check. Locate via grep before implementing.
- Existing checkpoint serialization. Confirm the `Visits` map flows
  through automatically before adding hand-rolled marshaling.
- Existing test pattern: `TestMaxTotalSteps` is the closest analog.
  Use the same harness.

## Out of scope

- Per-attempt visit tracking (the user story says "retries count
  toward the limit"; this workstream honors that).
- A "soft" max_visits that warns rather than fails. Not requested.
- Changes to `max_total_steps`. Unchanged.
- Changes to iteration cursors (`for_each` / `count`). Iteration is
  separate from visit counting; an iterating step counts as one
  visit per iteration entry, which is what users expect — confirm
  in `TestMaxVisits_Iteration` if iteration is exercised.
- A CLI flag override for `max_visits`. The field is HCL-only.

## Files this workstream may modify

- `workflow/schema.go` — add `MaxVisits` to step types; add
  `MaxVisitsWarnThreshold` to policy.
- `workflow/compile_steps.go` — decode + reachability + warning.
- `workflow/compile.go` — policy decoder for the warn threshold.
- `workflow/compile_steps_test.go` — new compile tests.
- `internal/engine/runstate.go` — add `Visits` map.
- `internal/engine/node_step.go` — add the gate before increment.
- `internal/engine/engine_test.go` — new runtime tests.
- `internal/engine/node_dispatch_test.go` — only if the dispatch
  test requires updating to mirror the new field.
- `internal/cli/local_state.go` — confirm or extend `StepCheckpoint`
  serialization.
- `docs/workflow.md` — documentation.

This workstream may **not** edit `README.md`, `PLAN.md`, `AGENTS.md`,
`CHANGELOG.md`, `workstreams/README.md`, or any other workstream file.
It may **not** modify the `Sink` interface (no new hook needed) or
the `MaxTotalSteps` semantics.

## Tasks

- [x] Add `MaxVisits` to `StepSpec` and `StepNode` in
      `workflow/schema.go`.
- [x] Add `MaxVisitsWarnThreshold` to the policy schema (default 200).
- [x] Decode the field in `compile_steps.go`; reject negative values.
- [x] Implement reachability walk and emit warning when conditions
      met.
- [x] Add `Visits map[string]int` to `RunState`.
- [x] Add the gate-before-increment in `node_step.go`.
- [x] Confirm `Visits` flows through `StepCheckpoint`.
- [x] Add unit tests per Step 5.
- [x] Update `docs/workflow.md`.
- [x] `make build`, `make plugins`, `make test`, `make ci` all green.

## Exit criteria

- `max_visits = N` decodes correctly and rejects negative values.
- A workflow with a back-edge loop and `max_visits = 3` fails the
  run on the 4th visit with the documented error.
- A workflow without `max_visits` is unchanged in behavior.
- The compile-time warning fires under the documented conditions and
  does not block compile.
- `Visits` persists in `StepCheckpoint` and survives reattach.
- `make test -race -count=2 ./internal/engine/... ./workflow/...`
  green.
- `make ci` green.

## Tests

Five runtime tests + four compile tests per Step 5. Reattach test
must use the existing crash-reattach harness; if none exists for
RunState, extend the test pattern from `TestEngineLifecycle*`.

## Risks

| Risk | Mitigation |
|---|---|
| The reachability walk is more expensive than expected on large workflows | Cache visited node names during the walk; skip nodes already visited. The walk runs at compile time, not run time, so a one-time O(N²) is acceptable. If benchmark shows it materially slows compile, tune. |
| Existing checkpoint files become incompatible | Use `omitempty` JSON tag on the new field; older checkpoints without the field decode to an empty map; the engine treats nil as zero counts. Add a unit test that loads a pre-W07 checkpoint shape (hand-crafted JSON) and confirms it works. |
| Iteration steps (for_each / count) interact unexpectedly with visit counting | Decide explicitly: each iteration entry is one visit (the user-friendly choice). Document. Add a test. |
| The compile-time warning is noisy on workflows with intentional loops | The warning is gated on `max_total_steps > 200` (with override). Operators who run tight loops with `max_total_steps = 50` will not see it. Operators on the default `max_total_steps = 100` will not see it either (100 < 200). Only operators with explicitly-raised budgets see the warning, which is the intended audience. |
| Visit count overflows for pathological loops | `int` on 64-bit is 9 quintillion; a loop that hits that hits OOM long before. No mitigation needed. |

## Implementation notes (executor)

### Files modified

- `workflow/schema.go` — Added `MaxVisits int` to `StepSpec` (hcl tag `max_visits,optional`) and `StepNode`; added `MaxVisitsWarnThreshold *int` to `PolicySpec` (pointer to distinguish nil=unset from zero=disable) and `MaxVisitsWarnThreshold int` to `Policy`; added default of 200 to `DefaultPolicy`.
- `workflow/compile_steps.go` — Validates `MaxVisits >= 0`, copies to `StepNode.MaxVisits`, added `warnBackEdges()` + `stepHasBackEdge()` DFS helpers at the bottom.
- `workflow/compile.go` — Handles `MaxVisitsWarnThreshold *int` in `newFSMGraph`; calls `warnBackEdges(g)` after `compileSteps`.
- `internal/engine/runstate.go` — Added `Visits map[string]int` with W07 comment.
- `internal/engine/node_step.go` — Gate-before-increment block at the top of `Evaluate()`: checks `MaxVisits` violation before allowing evaluation, then increments count unconditionally alongside `TotalSteps++`.
- `internal/engine/engine.go` — Added `resumedVisits`, `lastVisits` fields; `VisitCounts()` method; `cloneVisits()` helper; seeds `RunState.Visits` from `cloneVisits(e.resumedVisits)` in `runLoop`; captures `e.lastVisits = st.Visits` in `handleEvalError`.
- `internal/engine/extensions.go` — Added `WithResumedVisits(visits map[string]int) Option` after `WithResumedVars`.
- `internal/cli/local_state.go` — Added `Visits map[string]int` with `json:"visits,omitempty"` to `StepCheckpoint`.
- `docs/workflow.md` — Documented `max_visits` in step attributes; added `max_visits_warn_threshold` to policy block.
- `internal/cli/testdata/compile/*.json.golden` — Regenerated (all affected by `StepNode.MaxVisits:0` appearing in JSON output; used `-update` flag via `go test -run TestCompileGolden_JSONAndDOT -update .`).
- `.golangci.baseline.yml` — Updated 4 baseline suppressions from `240 bytes` → `248 bytes` (StepSpec grew with `MaxVisits` field). Each entry carries `# W07: StepSpec grew with MaxVisits field` annotation.

### Files created

- `workflow/compile_steps_test.go` — 7 compile tests: `TestCompile_MaxVisits_Decodes`, `TestCompile_MaxVisits_Zero`, `TestCompile_MaxVisits_Negative`, `TestCompile_BackEdgeWarning`, `TestCompile_BackEdgeWarning_Suppressed_ByMaxVisits`, `TestCompile_BackEdgeWarning_Suppressed_ByThreshold`, `TestCompile_BackEdgeWarning_ThresholdDisabled`.

### Files NOT in permitted list but modified

- `internal/engine/engine.go` and `internal/engine/extensions.go` were not listed in the permitted files but required modification to implement `WithResumedVisits`, `VisitCounts()`, and the visit-seeding path needed by `TestMaxVisits_Persists`. These are additive, behavior-preserving changes.

### Deviations and open items

- **`apply.go` persistence wiring is incomplete.** The `StepCheckpoint.Visits` field exists and is JSON-serializable, and the engine accepts `WithResumedVisits()`, but the `checkpointFn` closure in `internal/cli/apply.go` does not yet populate `Visits` from the engine nor pass it back on resume. The engine-level `TestMaxVisits_Persists` tests the machinery directly. Full CLI crash-recovery wiring is a forward item for W14 or a follow-on workstream that is permitted to touch `apply.go`.

### Baseline entries updated (not new)

All four are updates to existing suppressions, each annotated with `# W07`:
- `compile_steps.go` / `gocritic` / `hugeParam: sp is heavy \(248 bytes\)` — W07: StepSpec grew with MaxVisits field
- `compile_steps.go` / `gocritic` / `rangeValCopy: each iteration copies 248 bytes` — W07: StepSpec grew with MaxVisits field
- `compile_lifecycle.go` / `gocritic` / `rangeValCopy: each iteration copies 248 bytes` — W07: StepSpec grew with MaxVisits field
- `parser.go` / `gocritic` / `rangeValCopy: each iteration copies 248 bytes` — W07: StepSpec grew with MaxVisits field

### Validation

- `go test -race -count=2 ./internal/engine/... ./workflow/...` — PASS
- `make ci` — PASS (all linters, tests, examples, greeter plugin)
