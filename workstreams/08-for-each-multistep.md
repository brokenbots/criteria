# Workstream 8 — `for_each` multi-step iteration

**Owner:** Workstream executor · **Depends on:** [W01](01-flaky-test-fix.md), [W02](02-golangci-lint-adoption.md), [W03](03-god-function-refactor.md), [W04](04-split-oversized-files.md) · **Unblocks:** users who currently flatten executor/reviewer/cleanup chains into a single step. Source feedback: [user_feedback/04-make-for-each-safe-for-multi-step-chains-user-story.txt](../user_feedback/04-make-for-each-safe-for-multi-step-chains-user-story.txt).

## Context

The current `for_each` implementation in
[internal/engine/engine.go:215–226](../internal/engine/engine.go)
treats any step transition that is not `_continue` and not back to
the for_each node itself as **early-exit**:

```go
// If a per-iteration step exits via a non-_continue target while
// Iter is active, abort the loop: clear the cursor and follow the
// step's transition target directly (early-exit semantics).
if st.Iter != nil && st.Iter.InProgress && st.Current != st.Iter.NodeName {
    iterName := st.Iter.NodeName
    st.Iter = nil
    st.Vars = workflow.ClearEachBinding(st.Vars)
    e.sink.OnScopeIterCursorSet("") // cursor cleared
    deps.Sink.OnForEachOutcome(iterName, "any_failed", next)
    st.Current = next
    continue
}
```

This forces the `do` step to return `_continue` directly, so the
realistic shape — `for_each → execute → review → cleanup → _continue`
— is impossible. The first transition (`execute → review`) clears
the `each.*` bindings and aborts the loop with the spurious
`any_failed` outcome.

The user-reported impact: workflow authors flatten the chain into
a single step (concatenating prompts, mixing concerns) or
duplicate the loop, neither of which is acceptable for production
review chains.

This workstream introduces an **iteration subgraph**: the set of
steps reachable from the `do` step via outcome transitions, up to
and including the step(s) whose outcome transitions to
`_continue`. While the engine is executing any step in the
iteration subgraph, `each.*` stays bound and the loop does not
early-exit. Transitions out of the subgraph (to a step that isn't
part of it) trigger the existing early-exit semantics.

The subgraph is computed at compile time from the outcome graph
and validated against well-formedness rules.

## Prerequisites

- [W03](03-god-function-refactor.md) merged. The runLoop refactor
  isolated `interceptForEachContinue` as a single helper; this
  workstream extends that helper rather than the old
  inline-in-runLoop logic.
- [W04](04-split-oversized-files.md) merged. Compile-time
  validation lives in `workflow/compile_steps.go` /
  `workflow/compile_validation.go` post-split.
- `make ci` green on `main`.

## In scope

### Step 1 — Define semantics

**Iteration subgraph (compile-time concept).** Given a for_each
node `F` with `do = "S"`:

1. Start at step `S`.
2. For each outcome of `S` whose `transition_to` is **not**
   `_continue`:
   - If the target is another step `T`, add `T` to the subgraph
     and recurse from `T`.
   - If the target is a state, the iteration cannot advance
     through it — record this as a leaf "exit" of the subgraph.
   - If the target is the for_each node `F` itself, that is
     equivalent to `_continue` (legacy form; accept it).
3. The closure of all reachable steps via this walk is the
   iteration subgraph for `F`.

**Well-formedness rules** (compile errors if violated):

- Every step in the subgraph must have at least one outcome path
  (possibly transitive) that reaches `_continue`. A subgraph
  with a step that can only reach a state without going through
  `_continue` is a structural error: the iteration would
  mathematically never advance and the loop would either never
  terminate or always early-exit.
- A step cannot belong to two distinct for_each subgraphs. If
  the user wants nested loops, the inner loop is itself a
  for_each node within the outer subgraph (next phase
  consideration; this phase forbids the overlap).
- Cycles within the subgraph are allowed (e.g. a review-loop
  that goes back to execute on `changes_requested`), provided
  every cycle has at least one exit edge to `_continue` or to
  outside the subgraph.

**Runtime behavior changes.**

- The `interceptForEachContinue` helper (W03-extracted) is
  renamed `routeForEachStep` and broadened. Its responsibilities:
  - On `next == "_continue"` while the current step is in an
    active iteration subgraph: advance the cursor (existing
    behavior), clear `each.*` bindings, route to `Iter.NodeName`.
  - On `next == <step in same iteration subgraph>`: keep
    `each.*` bound, do not advance the cursor, do not early-exit.
  - On `next == <step outside the subgraph or a state>`: treat
    as early-exit (existing behavior).
- `each.value` and `each.index` remain in `st.Vars` for the full
  duration of an iteration — from when the `do` step is
  dispatched until either `_continue` or early-exit clears the
  binding.

**Compile-time validation message format:**

```
for_each "review_loop": iteration step "cleanup" has no outcome
  path that reaches _continue or transitions out of the
  iteration body.
  Iteration body: execute → review → cleanup
  Suggested fix: add an outcome to "cleanup" with
  transition_to = "_continue".
```

The diagnostic is tied to the source range of the offending
step's `step` block, not the for_each block.

### Step 2 — Compile-time changes

In `workflow/compile_steps.go` (post-W04 location):

1. Compute the iteration subgraph for every for_each node after
   step compilation completes (i.e. after every step's outcomes
   are bound). Store the subgraph on the for_each node:

   ```go
   type ForEachNode struct {
       // ...existing fields...
       IterationSteps map[string]struct{} // step names in the subgraph
   }
   ```

2. Validate well-formedness per the rules in Step 1. Emit HCL
   diagnostics.

3. Tag each StepNode with its owning for_each (if any):

   ```go
   type StepNode struct {
       // ...existing fields...
       IterationOwner string // empty if not part of any for_each subgraph
   }
   ```

   Reject overlap (a step appearing in two distinct subgraphs)
   with a diagnostic.

4. Validate that any expression in any step in a subgraph that
   references `each.*` does not appear in steps outside the
   subgraph (catches the common mistake of moving an `each.value`
   reference into a follow-up step that isn't actually part of
   the loop).

The iteration-subgraph computation is a fixed-point walk over
the outcome graph; cap depth at the total step count to prevent
runaway iteration in pathological inputs.

### Step 3 — Runtime changes

In `internal/engine/engine.go` (post-W03 layout):

1. Replace `interceptForEachContinue` with `routeForEachStep`.
   Signature:

   ```go
   func (e *Engine) routeForEachStep(st *RunState, next string) (string, action)
   ```

   where `action` is one of:
   - `actionAdvance` — `_continue` reached, advance cursor and
     route back to `Iter.NodeName`.
   - `actionStayInLoop` — transition to another step in the
     same iteration subgraph; keep `each.*` bound; route to
     `next`.
   - `actionExitLoop` — transition out of the subgraph; clear
     cursor, clear `each.*`, route to `next`.
   - `actionPassthrough` — not in an iteration; behave as before.

2. The decision uses `e.graph.Steps[st.Current].IterationOwner`
   and the for_each node's `IterationSteps` map. No string
   parsing at runtime.

3. `each.*` is cleared **only** on `actionAdvance` (between
   iterations) or `actionExitLoop`.

4. Preserve every existing event emission. The
   `OnForEachIteration` event continues to fire only on entry
   to the do-step at iteration start, not on every step within
   the iteration. Add a new event:

   ```go
   // OnForEachStep is emitted when the engine routes to a step
   // within an active iteration subgraph (other than the do
   // step at iteration start).
   OnForEachStep(node string, index int, step string)
   ```

   The event lets observers (the SDK, UIs, the standalone
   output) reflect "we're in step `review`, iteration index 3"
   without inferring it from the step name alone.

### Step 4 — Schema changes

No HCL schema changes. The semantics change is a behavior fix:
the existing for_each block, do attribute, and `_continue`
keyword all retain their syntax. Existing workflows that
already happen to use `do = "single_step"` with `transition_to = "_continue"`
continue to work unchanged.

This avoids forcing every existing workflow author into an
opt-in flag. If the new semantics break someone (e.g. a workflow
that deliberately relied on early-exit behavior — unlikely but
possible), they get a clear runtime error pointing at the
subgraph membership and they can restructure.

If reviewer or operator feedback during implementation reveals
that the semantics change is too aggressive without an opt-in,
add a temporary `CRITERIA_FOR_EACH_LEGACY=1` env var that
restores the old early-exit behavior. Default behavior is the
new semantics; the env var is an emergency lever, not the
intended path. Document removal in `v0.3.0`.

### Step 5 — Tests

Tests live in two new files:

`workflow/for_each_subgraph_compile_test.go`:

1. Single-step subgraph (`do = "execute"`, execute →
   `_continue`): compiles; `IterationSteps == {"execute"}`.
2. Multi-step subgraph (execute → review → cleanup → `_continue`):
   compiles; `IterationSteps == {"execute","review","cleanup"}`.
3. Branching subgraph (execute → review; review → execute on
   `changes_requested`, → cleanup on `approved`; cleanup →
   `_continue`): compiles; subgraph contains all three.
4. Subgraph with state-only exit (execute → review → "done"
   state, no `_continue`): fails compile with the diagnostic
   from Step 1.
5. Two for_each nodes with overlapping subgraphs (both reference
   `cleanup` in their bodies): fails compile.
6. `each.value` reference in a step outside the subgraph: fails
   compile with a diagnostic naming the step and the
   offending expression range.
7. Subgraph cycle without `_continue` exit (execute → review →
   execute, no cleanup or `_continue`): fails compile.
8. Cycle with `_continue` exit (execute → review → execute on
   request, → `_continue` on approve): compiles.

`internal/engine/node_for_each_multistep_test.go`:

9. Multi-step iteration runs end-to-end: a for_each over `[a, b, c]`
   with `execute → review → cleanup → _continue` produces three
   complete iterations, with `each.value` and `each.index`
   accessible in every step. Asserts the event ordering:
   `OnForEachIteration` (per cycle, on entry to execute) and
   the new `OnForEachStep` for `review` and `cleanup`.
10. Mid-iteration failure outcome: one iteration's `review` step
    returns `failure` instead of `success`; assert `AnyFailed`
    is set, the iteration completes (continues to `cleanup` →
    `_continue`), and the for_each node's final outcome is
    `any_failed`.
11. Early-exit via transition to a step outside the subgraph:
    `review` transitions to a top-level `escalate` step (not in
    the subgraph). Assert the loop early-exits, `each.*` is
    cleared, and `escalate` runs.
12. Crash-resume mid-iteration: cursor is serialized at
    `review` (not at the for_each node); on resume, execution
    re-enters `review` with `each.*` correctly bound.
13. Nested for_each: an outer loop body contains an inner
    for_each. The compile-time overlap check rejects
    accidental sharing; explicitly nested loops compile and
    run correctly.

`workflow/testdata/` gains fixtures for tests 1–8.

`internal/engine/testdata/` gains fixtures for tests 9–13.

`examples/`:

- `examples/for_each_review_loop.hcl` — a copy-pasteable example
  with the canonical `execute → review → cleanup` shape. Replaces
  any existing example whose loop only worked because of the old
  single-step semantics. Validated by `make validate`.

### Step 6 — Crash-resume cursor compatibility

The `IterCursor` struct ([workflow/iter_cursor.go](../workflow/iter_cursor.go))
is JSON-serialized into checkpoints. Adding the iteration-subgraph
behavior does not require new fields on the cursor — the
subgraph is recomputed from the graph on resume.

But: a checkpoint written at a step **within** the subgraph
(e.g. at `review`, mid-iteration) under the new semantics will
appear as a checkpoint of the wrong step under the old semantics
(it would early-exit on resume). Either:

- Bump the cursor JSON's `version` field, or
- Verify on resume that `Iter.NodeName`'s subgraph in the loaded
  graph still contains the resumed step. If not, fail with a
  clear "checkpoint references a step that is no longer in the
  for_each subgraph" error and the operator restarts.

Pick the verification approach (no version bump). It's simpler,
catches the same class of corruption, and works without
coordination between checkpoint writers and readers.

Add a test for this:

14. Resume from a checkpoint whose `Iter.NodeName`'s subgraph
    no longer contains the saved current-step (simulated by
    editing the workflow between checkpoint and resume): fails
    with the documented error.

### Step 7 — Documentation

Update **`docs/workflow.md`** with:

- A new "for_each iteration body" subsection under the existing
  for_each section, with the canonical multi-step example.
- A "Migrating from single-step for_each" note: existing
  single-step loops continue to work; the new semantics simply
  permit longer iteration bodies.
- A subsection on `each.*` lifetime: bound from the start of the
  do-step until `_continue` or early-exit.

Add a section to `examples/README.md` (if it exists; create if
not) pointing at `examples/for_each_review_loop.hcl` as the
worked example.

## Out of scope

- Nested for_each as a deliberately-supported pattern. The
  subgraph overlap check rejects accidental nesting. Explicit
  nested loops (one for_each inside another for_each's body)
  work but are tested defensively, not optimized for. A
  deliberate "nested loops" feature is Phase 2.
- Parallel iteration (`for_each_parallel`). Tracked as a Phase 2+
  item per [PLAN.md](../PLAN.md) "Deferred / forward-pointers".
- A `_break` keyword for explicit early-exit. The current
  early-exit-on-transition-out behavior is the de facto break;
  if a future workstream wants explicit `_break`, it is a
  separate feature.
- New event types beyond `OnForEachStep`. The existing
  `OnForEachIteration` and `OnForEachOutcome` carry the
  iteration-level signals.

## Files this workstream may modify

**Created:**

- `workflow/for_each_subgraph_compile_test.go`
- `workflow/testdata/for_each/` (new fixture directory)
- `internal/engine/node_for_each_multistep_test.go`
- `internal/engine/testdata/for_each/` (new fixture directory if
  not present)
- `examples/for_each_review_loop.hcl`
- `examples/README.md` (only if not present)

**Modified:**

- `workflow/compile_steps.go` (post-W04 location; iteration
  subgraph computation + validation)
- `workflow/compile_validation.go` (post-W04 location; the
  `each.*` reference scope check)
- `workflow/schema.go` (add `IterationSteps` to the for_each
  node, `IterationOwner` to the step node)
- `internal/engine/engine.go` (post-W03 location; replace
  `interceptForEachContinue` with `routeForEachStep` and the
  subgraph-aware routing)
- `internal/engine/extensions.go` (add `OnForEachStep` to the
  `Sink` interface)
- `internal/run/sink.go` (or wherever the production `Sink` is
  implemented; emit `OnForEachStep` events to the run stream)
- `internal/cli/reattach.go` (post-W03 location; add the
  resume-time subgraph membership check from Step 6)
- `events/` (new event type if `OnForEachStep` requires a new
  ND-JSON event kind)
- `docs/workflow.md`
- `.golangci.baseline.yml` (delete entries pointed at this
  workstream, if any)

This workstream may **not** edit `README.md`, `PLAN.md`,
`AGENTS.md`, `CHANGELOG.md`, `workstreams/README.md`, or any
other workstream file. It may **not** introduce new exported
SDK types beyond `OnForEachStep`. CHANGELOG entries are deferred
to [W10](10-phase1-cleanup-gate.md).

## Tasks

- [ ] Implement iteration-subgraph computation per Step 2.
- [ ] Implement compile-time validation (well-formedness,
      overlap, `each.*` scope).
- [ ] Refactor `interceptForEachContinue` → `routeForEachStep`
      per Step 3.
- [ ] Add `OnForEachStep` to the Sink interface and emit it
      from the engine; wire through to the production sink and
      ND-JSON event stream.
- [ ] Add resume-time subgraph membership check per Step 6.
- [ ] Add the 14 tests listed in Step 5 and Step 6.
- [ ] Add `examples/for_each_review_loop.hcl` and update
      `make validate`.
- [ ] Update `docs/workflow.md`.
- [ ] `make ci`, `make lint-go`, `make test-conformance`,
      `make validate` all green.
- [ ] CLI smoke: `./bin/criteria apply examples/for_each_review_loop.hcl`
      runs three iterations to completion with the expected
      event ordering.

## Exit criteria

- Multi-step iteration bodies work end-to-end: an iteration with
  `execute → review → cleanup → _continue` runs once per item
  with `each.*` accessible at every step.
- Compile-time validation catches all five error classes in
  Step 5 (single-step OK, multi-step OK, branching OK,
  state-only-exit fails, overlap fails, scope leak fails,
  cycle-without-exit fails, cycle-with-exit OK).
- The 14 tests pass under `go test -race ./workflow/...
  ./internal/engine/...`.
- The new `OnForEachStep` event appears in the ND-JSON event
  stream for multi-step iterations, with the correct `node`,
  `index`, and `step` fields.
- `examples/for_each_review_loop.hcl` validates and runs.
- Crash-resume mid-iteration succeeds when the workflow is
  unchanged, and fails cleanly with the documented error when
  the workflow is edited between checkpoint and resume.
- Existing single-step for_each examples (e.g. any in
  `examples/` today) continue to validate and run unchanged.
- No new entries in `.golangci.baseline.yml`.

## Tests

14 tests listed verbatim across Step 5 and Step 6. All must run
in `make test` and gate CI. Tests 9–13 are the engine-level
integration tests; tests 1–8 are the compile-level tests.

## Risks

| Risk | Mitigation |
|---|---|
| The new semantics break someone's existing workflow | Single-step `do = "X"` with `X → _continue` still works (the subgraph is `{X}`, transitions to `_continue` advance, transitions elsewhere early-exit — same as before). The semantics genuinely changed only for multi-step bodies, which currently don't work at all, so there is no working baseline to break. The `CRITERIA_FOR_EACH_LEGACY=1` env-var lever is documented as the emergency exit. |
| Iteration subgraph computation has a bug that misses a step | The compile-time tests in Step 5 cover single-step, multi-step linear, branching, and cyclic shapes. The state-only-exit and `each.*` scope checks act as cross-validators: a missed step would either appear with `each.*` and trigger the scope error, or appear without `each.*` and fail at runtime with a clear "each is only valid inside for_each" error. |
| Compile-time validation rejects a workflow that worked before | Test 1 (single-step subgraph) is the regression guard. The reviewer must run every example in `examples/` (`make validate`) and assert no diagnostics that weren't there before. |
| Crash-resume corruption when the workflow is edited mid-resume | Step 6's verification check is the documented behavior. The test for it (test 14) covers the edit-then-resume path. Older checkpoints with cursors at the for_each node itself continue to resume cleanly because the cursor's `NodeName` membership is validated, not the resumed step. |
| `OnForEachStep` event kind ripples into the SDK and breaks consumers | The new event is purely additive in the ND-JSON stream. Existing consumers ignore unknown event types. The SDK conformance suite gets a new test asserting the event is present in multi-step runs; existing assertions about single-step runs are unchanged. |
| The runtime helper `routeForEachStep` grows beyond W03's 50-line cap | Extract the action-selection switch into a method on `RunState` (e.g. `(st *RunState) iterationAction(graph, next) action`) so the dispatcher in `runLoop` stays narrow. If still over the cap, split per-action handlers. The funlen lint is the gate. |
| The example workflow `examples/for_each_review_loop.hcl` requires a real adapter (Copilot or shell) and breaks `make validate` in CI | Use the `noop` adapter for the example so it validates anywhere. A second, Copilot-based example can ship as part of a future Copilot-focused workstream. |
| `IterationOwner` overlap check forbids a legitimate "shared cleanup step" pattern | This phase forbids shared steps. If users complain, follow up with explicit nested-loops support or a "shared utility step" feature in Phase 2. The current restriction matches the user-story scope; loosening later is easier than tightening. |
| The new `OnForEachStep` event is verbose enough to drown out signal in long iterations | The event is opt-in for consumers (they choose what to render); the standalone-output workstream (deferred user feedback) is the right place to decide what gets shown by default. This workstream emits the event; it does not change presentation. |
