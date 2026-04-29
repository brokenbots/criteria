# Workstream 10 — Step-level iteration and the `workflow` step type

**Owner:** Workstream executor · **Depends on:** [W01](01-flaky-test-fix.md), [W02](02-golangci-lint-adoption.md), [W03](03-god-function-refactor.md), [W04](04-split-oversized-files.md), [W08](08-for-each-multistep.md) · **Unblocks:** the [W11 cleanup gate](11-phase1-cleanup-gate.md). **Supersedes** the W08 runtime model: this workstream removes the top-level `for_each "name" { ... }` block entirely.

## Context

[W08](08-for-each-multistep.md) shipped `for_each` as a **top-level workflow node** with a compile-time iteration-subgraph computed by walking outcome transitions from a `do` step until they reach `_continue`. Cross-functional review (architecture, design, product, engineering) is uniformly negative on the resulting syntax and semantics:

- Authors expected `for_each` to be **at the step level** (count-like; useful as a workaround for the missing `count` field, or for retry-with-exit patterns), but W08 placed it at the workflow level.
- Authors expected the iteration body to be a **sub-graph defined inside the iterating block** so they can reason about it locally, but W08 computes it implicitly via outcome reachability.
- The boundary semantics (early-exit when transitioning outside the implicit subgraph vs. advance via `_continue`) are difficult to reason about, especially when reviewing diffs that change a single outcome target.

The joint architecture/design/product/engineering decision unifies both expectations and replaces W08's runtime model:

1. **`for_each` and `count` are step-level fields**, valid on any step (adapter, agent, or the new `workflow` type). This is the Terraform-shaped iteration model.
2. **A new step type `workflow`** holds a nested workflow body — defined inline as a `workflow { ... }` block, or loaded from a file via `workflow_file = "./path.hcl"`.
3. **Outputs are indexed**: numeric keys for list/tuple/`count` sources, string keys for map/object sources.
4. **`count` and `for_each` share one implementation**: `count = N` is sugar for `for_each = range(N)`.
5. **The W08 top-level `for_each` block is ripped out** — schema, compile pass, runtime routing, tests, fixtures, and the W08 example are deleted (no deprecation period).

The W08 user story (`user_feedback/04-make-for-each-safe-for-multi-step-chains-user-story.txt`) remains satisfied: multi-step iteration bodies are still expressible — they live inside the new `workflow { ... }` block.

## Decisions

| Decision | Choice |
|---|---|
| Step type name | `workflow` |
| Iteration scope | `for_each` / `count` allowed on **any** step type |
| W08 top-level `for_each` block | **Rip out**, no deprecation |
| List/tuple iteration index | Numeric (`steps.foo[0]`) |
| Map/object iteration index | String key (`steps.foo["k"]`) |
| Failure handling | `on_failure = "abort" \| "continue" \| "ignore"`, default `continue` |
| `each.*` bindings | `value`, `key`, `_idx`, `_first`, `_last`, `_total`, `_prev` |
| `each._prev` on iter 0 | `null` |
| Step output exposure | Only explicit `output { name=...; value=... }` blocks |

## HCL contract

### Inline nested workflow over a list

```hcl
step "process_items" {
  type     = "workflow"
  for_each = ["alpha", "beta", "gamma"]

  workflow {
    step "execute" {
      adapter = "noop"
      input { label = "execute:${each.value}" }
      outcome "success" { transition_to = "review" }
    }
    step "review" {
      adapter = "noop"
      outcome "success" { transition_to = "cleanup" }
      outcome "failure" { transition_to = "_continue" }
    }
    step "cleanup" {
      adapter = "noop"
      outcome "success" { transition_to = "_continue" }
    }

    output "label" { value = steps.execute.label }
  }

  outcome "all_succeeded" { transition_to = "done" }
  outcome "any_failed"    { transition_to = "failed" }
}
```

### Loaded from file with `count`

```hcl
step "retry_check" {
  type          = "workflow"
  count         = 3
  workflow_file = "./check.hcl"
  outcome "all_succeeded" { transition_to = "done" }
  outcome "any_failed"    { transition_to = "failed" }
}
```

### Iteration on a regular adapter step (no nested workflow)

```hcl
step "fan_out" {
  adapter  = "http_get"
  for_each = var.urls
  input    { url = each.value }
  outcome "success" { transition_to = "summarize" }
  outcome "failure" { transition_to = "fail" }
}
```

### Reduce / scan via `each._prev`

```hcl
step "running_total" {
  adapter  = "compute"
  for_each = var.amounts
  input {
    accumulator = each._first ? 0 : each._prev.total
    addend      = each.value
  }
  outcome "success" { transition_to = "_continue" }
}
```

### `each.*` bindings

| Binding        | Type            | Meaning                                                                 |
|----------------|-----------------|-------------------------------------------------------------------------|
| `each.value`   | any             | Current element value.                                                  |
| `each.key`     | string\|number  | Map key for map iteration; equals `_idx` for list/count.                |
| `each._idx`    | number          | Canonical 0-based loop position (always numeric).                       |
| `each._first`  | bool            | `_idx == 0`.                                                            |
| `each._last`   | bool            | `_idx == _total - 1`.                                                   |
| `each._total`  | number          | Length of iteration source.                                             |
| `each._prev`   | object\|null    | Previous iteration's exposed outputs; `null` on iteration 0.            |

`each._prev` carries the same object the previous iteration exposes via its `output` blocks (workflow-type) or the previous adapter's outputs (adapter/agent steps). It survives crash-resume because the cursor persists it.

### `on_failure`

`on_failure` is a step-level attribute, valid only when the step iterates (rejected at compile time on non-iterating steps):

- **`continue`** *(default)* — every iteration runs; outer outcome is `all_succeeded` if every iteration produced a success outcome, else `any_failed`.
- **`abort`** — stop at first non-success iteration; outer outcome `any_failed`. Remaining iterations do not run.
- **`ignore`** — every iteration runs; outer outcome **always `all_succeeded`**. Per-iteration failure is still observable in `steps.foo[i]` and in events.

### Output exposure

Callers see only outputs declared in `output` blocks. For `type = "workflow"` steps, `output` blocks live inside `workflow { ... }`. For adapter/agent steps, the adapter's natural outputs are the per-iteration object (no `output` block to declare).

Indexed access:

- `count = 3` → `steps.foo[0].x`, `steps.foo[1].x`, `steps.foo[2].x`
- `for_each = ["a","b"]` (list) → `steps.foo[0].x`, `steps.foo[1].x`
- `for_each = { a="x", b="y" }` (map) → `steps.foo["a"].x`, `steps.foo["b"].x`
- Non-iterating step (today's behavior) → `steps.foo.x`

Aggregate metadata is conveyed by the step's outer outcome (`all_succeeded` / `any_failed`); not exposed as fields. `length(steps.foo)` works for users needing a count.

### `each.*` lifetime

- Bound when iteration begins (cursor pushed, step entered).
- Available throughout the iteration body (single adapter call, or every node in a nested workflow).
- Cleared on advance (`_continue`) and on early exit (transition to a target outside the body / to the step's outer outcome).
- `each._prev` is updated between iterations: after iteration `i`'s `output` blocks evaluate, the resulting object is stored on the cursor and bound as `each._prev` at iteration `i+1`'s entry.
- Crash-resume re-evaluates the iteration source and re-binds `each.*` (including `_prev`) from the persisted cursor. Errors are logged via the engine logger (no silent failure — same lesson as W08 review N2).

## Schema contract (Go)

### `workflow/schema.go` — `StepSpec` (parsed) extensions

```go
type StepSpec struct {
    Name      string            `hcl:"name,label"`
    Type      string            `hcl:"type,optional"`        // NEW: "" (default) or "workflow"
    Adapter   string            `hcl:"adapter,optional"`
    Agent     string            `hcl:"agent,optional"`
    Lifecycle string            `hcl:"lifecycle,optional"`
    OnCrash   string            `hcl:"on_crash,optional"`
    OnFailure string            `hcl:"on_failure,optional"`  // NEW
    WorkflowFile string         `hcl:"workflow_file,optional"` // NEW
    Workflow  *WorkflowBodySpec `hcl:"workflow,block"`         // NEW
    Config    map[string]string `hcl:"config,optional"`        // legacy
    Input     *InputSpec        `hcl:"input,block"`
    Timeout   string            `hcl:"timeout,optional"`
    AllowTools []string         `hcl:"allow_tools,optional"`
    Outcomes  []OutcomeSpec     `hcl:"outcome,block"`
    Remain    hcl.Body          `hcl:",remain"`              // captures count, for_each
    LegacyConfigRange *hcl.Range
}

type WorkflowBodySpec struct {
    Steps     []*StepSpec     `hcl:"step,block"`
    States    []*StateSpec    `hcl:"state,block"`
    Branches  []*BranchSpec   `hcl:"branch,block"`
    Waits     []*WaitSpec     `hcl:"wait,block"`
    Approvals []*ApprovalSpec `hcl:"approval,block"`
    Outputs   []*OutputSpec   `hcl:"output,block"`
    Entry     string          `hcl:"entry,optional"`
    Remain    hcl.Body        `hcl:",remain"`
}

type OutputSpec struct {
    Name   string   `hcl:"name,label"`
    Remain hcl.Body `hcl:",remain"` // captures `value = <expr>`
}
```

### `workflow/schema.go` — `StepNode` (compiled) extensions

```go
type StepNode struct {
    Name       string
    Type       string                                // NEW
    Adapter    string
    Agent      string
    Lifecycle  string
    OnCrash    string
    OnFailure  string                                // NEW
    Input      map[string]string
    InputExprs map[string]hcl.Expression
    Timeout    time.Duration
    Outcomes   map[string]string
    AllowTools []string

    // Iteration (NEW)
    Count   hcl.Expression                            // exclusive with ForEach
    ForEach hcl.Expression

    // Nested body (NEW; non-nil when Type == "workflow")
    Body      *FSMGraph
    BodyEntry string
    Outputs   map[string]hcl.Expression               // declared output blocks
}
```

### Deletions

- `ForEachSpec` (`workflow/schema.go` lines 171–183) — removed.
- `ForEachNode` (`workflow/schema.go` lines 311–333) — removed.
- `StepNode.IterationOwner` (W08 addition around lines 234–262) — removed.
- `FSMGraph.ForEachs map[string]*ForEachNode` (around lines 199–215) — removed.

## Prerequisites

- W01 / W02 / W03 / W04 merged.
- W08 merged (this workstream removes its runtime; reference its tests for behavioural expectations on multi-step bodies).
- `make ci` green on `main`.

## In scope

### Step 1 — Schema: extend `StepSpec` and add `WorkflowBodySpec`

**Files**: [workflow/schema.go](../workflow/schema.go)

- [ ] Add fields to `StepSpec`:
  - [ ] `Type string` with tag `hcl:"type,optional"`.
  - [ ] `WorkflowFile string` with tag `hcl:"workflow_file,optional"`.
  - [ ] `Workflow *WorkflowBodySpec` with tag `hcl:"workflow,block"`.
  - [ ] `OnFailure string` with tag `hcl:"on_failure,optional"`.
  - [ ] Ensure `Remain hcl.Body` captures `count` and `for_each` (decoded in compile-step phase).
- [ ] Add `WorkflowBodySpec` type per the schema contract above.
- [ ] Add `OutputSpec` type per the schema contract above.
- [ ] Extend `StepNode` per the schema contract above (compiled fields).
- [ ] Delete `ForEachSpec`, `ForEachNode`, `StepNode.IterationOwner`, and `FSMGraph.ForEachs`.

**Acceptance**:

- [ ] `go build ./workflow/...` clean.
- [ ] `grep -rn 'ForEachSpec\|ForEachNode\|IterationOwner' workflow/` returns no hits in non-test code (test deletion happens in Step 8).

### Step 2 — Compile: nested workflow + iteration validation

**Files**: [workflow/compile.go](../workflow/compile.go), [workflow/compile_steps.go](../workflow/compile_steps.go); **delete** [workflow/compile_foreach_subgraph.go](../workflow/compile_foreach_subgraph.go).

Changes in `compile.go` (`CompileWithOpts`, around lines 45–79):

- [ ] Remove `compileForEachs(g, spec)` call.
- [ ] Remove `computeIterationSubgraphs(g)` call.
- [ ] Remove `validateEachReferenceScope(g)` call (replaced inside `compile_steps.go`).
- [ ] Add `LoadDepth int` and `LoadStack []string` to `CompileOpts` (defaults: 0, empty); used to detect cycles when recursively compiling `workflow_file`.
- [ ] Surface `SubWorkflowResolver` in `CompileOpts` (today on the engine; see [internal/engine/extensions.go:113-118](../internal/engine/extensions.go)). Add a thin parser/resolver path here so compile-time can resolve `workflow_file`.

Changes in `compile_steps.go`:

- [ ] Validate exclusivity: exactly one of `{Adapter != ""}`, `{Agent != ""}`, `{Type == "workflow"}` must hold; otherwise emit a diagnostic.
- [ ] Decode `count` and `for_each` from the step's `Remain` body. Reject if both are present.
- [ ] Reject `on_failure` on non-iterating steps. Default to `"continue"` when omitted on iterating steps. Validate enum: `abort`, `continue`, `ignore`.
- [ ] For `Type == "workflow"`:
  - [ ] Reject simultaneous `Workflow` block and `WorkflowFile` (must be exactly one).
  - [ ] For inline `Workflow`: build a synthetic `Spec`, call `CompileWithOpts` recursively with `LoadDepth+1`. Reject when `LoadDepth > 4` with a "nested-workflow depth limit" diagnostic.
  - [ ] For `WorkflowFile`: resolve via `SubWorkflowResolver`; cycle-check via `LoadStack`; recursively compile.
  - [ ] Validate body has at least one transition target equal to `_continue` when the step iterates (else iteration cannot advance — emit a diagnostic that names the step).
  - [ ] Resolve `BodyEntry`: if `entry` attribute set, validate it names a step in the body; else use the first declared step.
  - [ ] Decode `output` blocks; build `Outputs map[string]hcl.Expression`. Reject duplicate output names.
- [ ] For non-workflow iterating steps (adapter/agent + count/for_each): no body-level `_continue` check (iteration is per-call); the adapter's outcome maps to advance/fail per the existing transition logic.
- [ ] Replace `validateEachReferenceScope`: walk each compiled step's input/transition expressions. If they reference `each.*`, the step must be inside an iterating step's body (or must itself iterate). Emit diagnostic on violation.

**Acceptance**:

- [ ] `go build ./...` clean.
- [ ] `compile_foreach_subgraph.go` deleted; `grep -rn 'computeIterationSubgraphs\|validateEachReferenceScope\|IterationOwner' .` returns no hits.
- [ ] Unit test: workflow with both `count` and `for_each` on one step → diagnostic.
- [ ] Unit test: depth-5 nested `workflow_file` chain → "nested-workflow depth limit" diagnostic.
- [ ] Unit test: `workflow_file` cycle (A loads B loads A) → "cyclic nested workflow" diagnostic.

### Step 3 — Generalize iteration cursor

**Files**: [workflow/iter_cursor.go](../workflow/iter_cursor.go), [internal/engine/runstate.go](../internal/engine/runstate.go).

- [ ] Rename `IterCursor.NodeName` → `StepName`.
- [ ] Add `Key cty.Value` (string for map iteration; numeric matching `Index` for list/count).
- [ ] Add `Total int` (cached length of source).
- [ ] Add `Prev cty.Value` (`cty.NilVal` initially; updated each iteration).
- [ ] Add `OnFailure string` (snapshot from compiled step at cursor creation).
- [ ] `RunState.Iter` becomes `[]IterCursor` (stack); top-of-stack is active.
- [ ] Update serialization: cursor must persist `Index`, `Key`, `Total`, `Prev`, `OnFailure`, `AnyFailed`, `InProgress`, plus the source expression's identity (so `Items` can be re-evaluated on resume). `Items` itself is omitted from checkpoint to keep size bounded; re-evaluated on resume.
- [ ] Add helpers on `RunState`: `pushCursor`, `popCursor`, `topCursor`.

**Acceptance**:

- [ ] `go build ./workflow/... ./internal/engine/...` clean.
- [ ] Cursor serialization round-trip test (write → read) preserves `Prev` and `Key`.

### Step 4 — `each.*` binding helpers

**Files**: [workflow/eval.go](../workflow/eval.go) (around lines 222–293).

- [ ] Replace `WithEachBinding(vars, value, index)` with `WithEachBinding(vars, b EachBinding)` where `EachBinding` carries:
  ```go
  type EachBinding struct {
      Value cty.Value
      Key   cty.Value
      Idx   int
      Total int
      Prev  cty.Value
  }
  ```
- [ ] Build the `each` object as
  ```go
  cty.ObjectVal(map[string]cty.Value{
      "value":  b.Value,
      "key":    b.Key,
      "_idx":   cty.NumberIntVal(int64(b.Idx)),
      "_first": cty.BoolVal(b.Idx == 0),
      "_last":  cty.BoolVal(b.Idx == b.Total - 1),
      "_total": cty.NumberIntVal(int64(b.Total)),
      "_prev":  b.Prev, // cty.NullVal(...) on iter 0
  })
  ```
- [ ] `ClearEachBinding(vars)` — unchanged in shape; remove the `each` key from `vars`.
- [ ] Add `WithIndexedStepOutput(vars, stepName string, key cty.Value, outputs map[string]cty.Value)` for the iterating case. Merge logic:
  - If `vars["steps"][stepName]` does not exist: create as a single-key object `{key: outputs}`.
  - If it exists and is the indexed shape: add the new key.
  - If it exists and is the flat (non-iterating) shape: error (programming bug; should not happen at runtime).
- [ ] Keep `WithStepOutputs(vars, stepName, outputs)` for the non-iterating case (flat `steps[stepName]` object).

**Acceptance**:

- [ ] Unit tests:
  - [ ] List iteration produces numeric-keyed object on `vars["steps"][stepName]`.
  - [ ] Map iteration produces string-keyed object.
  - [ ] Non-iterating step produces flat object.
  - [ ] `each._first`/`_last` correct on boundaries; `_total` matches source length.

### Step 5 — Runtime: per-step iteration

**Files**: [internal/engine/engine.go](../internal/engine/engine.go), [internal/engine/node_step.go](../internal/engine/node_step.go), [internal/engine/node.go](../internal/engine/node.go); **delete** [internal/engine/node_for_each.go](../internal/engine/node_for_each.go); **create** `internal/engine/node_workflow.go`.

- [ ] Delete `node_for_each.go` entirely (the top-level `forEachNode`).
- [ ] In `node.go`: remove the `ForEachs` case from `nodeFor` dispatch (lines ~34–55).
- [ ] In `engine.go`:
  - [ ] Delete `routeForEachStep`, `iterationAction`, the action enum constants, and `rebindEachOnResume` (lines 156–340).
  - [ ] Add `routeIteratingStep(st *RunState, step *workflow.StepNode, next string) string` that handles per-step iteration logic:
    - On step entry: if step has `Count` or `ForEach` and no active cursor on top-of-stack for this step, evaluate source, push cursor, set `each.*`, dispatch to either body entry (workflow type) or the adapter call (adapter/agent type).
    - On a body step's outcome: classify transition target — `_continue` (advance), within-body step (stay), outside-body (early exit).
    - Apply `OnFailure`:
      - `abort`: on first non-success iteration, set `AnyFailed`, pop cursor, route to `any_failed` outer outcome.
      - `continue`: track `AnyFailed`, advance to next iteration.
      - `ignore`: emit per-iteration failure events but never set outer `AnyFailed`.
    - Between iterations: evaluate iterating step's `output` blocks (workflow type) against the body's `Vars` snapshot; store as `Prev` on cursor; merge into outer `vars["steps"][stepName][key|idx]` via `WithIndexedStepOutput`.
    - On loop completion: pop cursor; emit appropriate outer outcome (`all_succeeded` / `any_failed` — or always `all_succeeded` for `ignore`); clear `each.*` from outer scope.
- [ ] In `node_step.go`:
  - [ ] At `stepNode.Evaluate`, check if step iterates and whether a cursor for this step is already active on top-of-stack. If iterating, defer to `routeIteratingStep`. Otherwise existing path (single adapter invocation).
  - [ ] For `Type == "workflow"` non-iterating: dispatch to `BodyEntry` of `Body`; treat the body's `_continue` (or any unrouted exit) as the step's "natural completion" producing declared `Outputs`.
- [ ] Add `internal/engine/node_workflow.go` containing the helper that runs a nested-graph iteration: pushes a body-local `Vars` scope, runs body steps to completion, evaluates `output` blocks, returns the captured object.
- [ ] Iteration source evaluation supports: list, tuple, set (deterministic order = sorted), map, object, plus `count`-as-number (auto-converted to `range(N)`). Mixed-type tuples are accepted.

**Acceptance**:

- [ ] `go build ./...` clean.
- [ ] Engine tests (Step 8) pass.
- [ ] `grep -rn 'forEachNode\|routeForEachStep\|iterationAction\|rebindEachOnResume\|IterationOwner' .` returns no hits outside this workstream's reviewer notes.

### Step 6 — Reattach / resume validation

**Files**: [internal/cli/reattach.go](../internal/cli/reattach.go).

- [ ] Delete `checkIterationSubgraphMembership`.
- [ ] Add `checkIterationCursorValidity(graph *workflow.FSMGraph, iterStack []workflow.IterCursor, current string)`:
  - For each cursor on the stack: verify `StepName` still exists as a step in the relevant graph (parent for the bottom cursor, nested body for higher cursors — walk down the stack, descending into `Body` at each level).
  - For the topmost cursor: if `current` is a body-step, verify it exists in the body of `cursor.StepName`. If body has been modified (step renamed, removed), fail with a clear diagnostic naming both the cursor's step and the missing current step.
- [ ] On resume, the engine re-evaluates iteration source and re-binds `each.*` including `_prev` from the persisted cursor. Log an error (do not silently swallow) if the source expression fails to re-evaluate.

**Acceptance**:

- [ ] Unit tests in `internal/cli/reattach_test.go`:
  - [ ] Cursor whose `StepName` no longer exists → error.
  - [ ] Cursor present, `current` missing from body → error.
  - [ ] Cursor present, all nodes valid → success.

### Step 7 — Events

**Files**: [proto/criteria/v1/events.proto](../proto/criteria/v1/events.proto), [events/types.go](../events/types.go), [internal/run/sink.go](../internal/run/sink.go), [internal/run/console_sink.go](../internal/run/console_sink.go).

- [ ] Repurpose existing `ForEachStep` (proto field 32) as `StepIterationItem` (rename the message; keep the field number to avoid wire-format renumber). Fields: `step_name string; idx int; key string; first bool; last bool; total int`.
- [ ] Repurpose `ForEachIteration` / `ForEachOutcome` similarly; rename to `StepIterationStarted` / `StepIterationCompleted`. Keep their field numbers.
- [ ] Update Go envelope union in `events/types.go` to match.
- [ ] Update sink methods: `OnForEachStep` → `OnStepIterationItem`, `OnForEachIteration` → `OnStepIterationStarted`, `OnForEachOutcome` → `OnStepIterationCompleted`. Update both `internal/run/sink.go` (production) and `internal/run/console_sink.go` (CLI).
- [ ] Console output: rename "for_each" labels in the human-readable stream to "step iteration".
- [ ] Add a comment in the proto file documenting the rename.

**Acceptance**:

- [ ] `make proto-lint` and `make proto-check-drift` pass after regenerating Go bindings.
- [ ] An existing event with field 32 still deserializes as the renamed message (verify with a fixture round-trip if any persisted NDJSON exists in `internal/run/testdata/`; update fixtures if needed).

### Step 8 — Tests and fixtures

**Files**: **delete** [workflow/for_each_subgraph_compile_test.go](../workflow/for_each_subgraph_compile_test.go), [internal/engine/node_for_each_multistep_test.go](../internal/engine/node_for_each_multistep_test.go), [workflow/testdata/for_each/](../workflow/testdata/for_each/), [internal/engine/testdata/for_each/](../internal/engine/testdata/for_each/).

Create new test files & fixtures:

- [ ] `workflow/iteration_compile_test.go`:
  - [ ] `TestStep_TypeWorkflow_InlineBody_Compiles`
  - [ ] `TestStep_TypeWorkflow_FromFile_Compiles`
  - [ ] `TestStep_TypeWorkflow_RecursiveDepthLimit_Fails` (5 levels)
  - [ ] `TestStep_TypeWorkflow_FileCycle_Fails`
  - [ ] `TestStep_BothCountAndForEach_Fails`
  - [ ] `TestStep_OnFailureOnNonIteratingStep_Fails`
  - [ ] `TestStep_OnFailureInvalidValue_Fails`
  - [ ] `TestStep_WorkflowBody_NoContinuePath_Fails`
  - [ ] `TestStep_DuplicateOutputName_Fails`
  - [ ] `TestStep_EachRefOutsideIteratingBody_Fails`
- [ ] `workflow/testdata/iteration/`:
  - [ ] `inline_list.hcl`, `inline_map.hcl`, `count_simple.hcl`
  - [ ] `from_file_parent.hcl` + `from_file_child.hcl`
  - [ ] `cycle_a.hcl` + `cycle_b.hcl`
  - [ ] `depth_5.hcl` (nests 5 deep, should fail)
  - [ ] `bad_both_iter.hcl`, `bad_on_failure_target.hcl`, `bad_no_continue.hcl`, `bad_dup_output.hcl`, `bad_each_outside.hcl`
- [ ] `internal/engine/iteration_engine_test.go`:
  - [ ] `TestIter_Adapter_Count_RunsNTimes` — uses **value-capturing loader** (not noop; same lesson as W08 review R1/R2). Asserts `each._idx ∈ {0,1,2}`, `_first` only on first, `_last` only on last.
  - [ ] `TestIter_Workflow_NestedBody_BindsEachThroughout` — asserts `each.value` reaches every nested step.
  - [ ] `TestIter_Total_AndKey_ForMap` — `each._total` matches map length; `each.key` is the map key.
  - [ ] `TestIter_Prev_NullOnFirst_ObjectAfter` — running-sum reduce test asserts final iteration's accumulator is correct.
  - [ ] `TestIter_OnFailure_Continue_Aggregates` — fail iter 1; iters 0/2 still run; outer `any_failed`.
  - [ ] `TestIter_OnFailure_Abort_StopsAtFirstFailure` — iters after failure don't run.
  - [ ] `TestIter_OnFailure_Ignore_AlwaysSucceeds` — iter 1 fails; outer `all_succeeded`; per-iter outputs still present.
  - [ ] `TestIter_EarlyExit_OutsideBody_TerminatesLoop`
  - [ ] `TestIter_OutputBlocks_OnlyDeclaredVisible` — non-exported nested step outputs absent from `steps.foo[i]`.
  - [ ] `TestIter_OutputBlocks_NoneDeclared_AdapterStep` — adapter outputs visible by default for non-workflow type.
  - [ ] `TestIter_CrashResume_RebindEach_IncludingPrev` — capturing loader asserts post-resume.
  - [ ] `TestIter_NestedIteration_CursorStack` — workflow step contains a step that itself iterates.
  - [ ] `TestIter_ResumeRejectsModifiedBody` — body edited so saved current step missing; resume fails.
- [ ] `internal/engine/testdata/iteration/`: matching fixtures for the engine tests.
- [ ] `internal/cli/reattach_test.go`: 3 unit tests for `checkIterationCursorValidity`.

**Acceptance**:

- [ ] All new tests pass.
- [ ] `grep -rn 'for_each "[^"]*"\s*{' workflow/testdata/ internal/engine/testdata/` returns zero hits (no top-level `for_each` blocks remain in fixtures).

### Step 9 — Examples

**Files**: rewrite [examples/for_each_review_loop.hcl](../examples/for_each_review_loop.hcl); update [examples/README.md](../examples/README.md); create `examples/workflow_step_compose.hcl` and `examples/lib/check.hcl`.

- [ ] Rewrite `examples/for_each_review_loop.hcl` to:
  ```hcl
  step "process" {
    type     = "workflow"
    for_each = ["alpha", "beta", "gamma"]
    workflow {
      step "execute" { ... outcome "success" { transition_to = "review" } }
      step "review"  { ... outcome "success" { transition_to = "cleanup" }; outcome "failure" { transition_to = "_continue" } }
      step "cleanup" { ... outcome "success" { transition_to = "_continue" } }
      output "label" { value = steps.execute.label }
    }
    outcome "all_succeeded" { transition_to = "done" }
    outcome "any_failed"    { transition_to = "failed" }
  }
  ```
  Keep the same outer outcome targets (`done`, `failed`) and terminal states so the W11 cleanup gate's CLI smoke test still passes.
- [ ] Create `examples/lib/check.hcl` — a small reusable workflow body (a few steps, one `output` block, terminating in `_continue`).
- [ ] Create `examples/workflow_step_compose.hcl` — a parent workflow that loads `examples/lib/check.hcl` via `workflow_file`, with `count = 3`.
- [ ] Add the new example to `examples/README.md`.
- [ ] `make validate` must pass for both examples.

**Acceptance**:

- [ ] `./bin/criteria apply examples/for_each_review_loop.hcl --events-file /tmp/events.ndjson` exits 0; events show 3 iterations, each running execute → review → cleanup, terminal outcome `all_succeeded`.
- [ ] `./bin/criteria apply examples/workflow_step_compose.hcl` exits 0.

### Step 10 — Documentation

**Files**: [docs/workflow.md](../docs/workflow.md).

- [ ] Delete the W08 top-level `for_each` prose (around lines 378–481).
- [ ] Add a new "Step iteration" section covering:
  - [ ] `count` and `for_each` as step-level fields, valid on any step type.
  - [ ] The `workflow` step type with inline body and `workflow_file`.
  - [ ] Full `each.*` binding table (copy from "HCL contract" above).
  - [ ] `on_failure` modes.
  - [ ] Output exposure and `output` blocks; indexed access patterns (numeric vs. keyed).
  - [ ] `each._prev` reduce/scan example.
  - [ ] **Migration note**: "If you have a top-level `for_each \"name\" { ... }` block from W08, rewrite as `step \"name\" { type = \"workflow\"; for_each = ...; workflow { ... } }`. The `do` step becomes the body's entry; outer outcomes are unchanged. `each.*` semantics are preserved; new bindings (`_first`, `_last`, `_total`, `_prev`, `_idx`, `key`) are additive."
  - [ ] Crash-resume guarantees (each.* re-binding including `_prev`).
  - [ ] Variable scope rules for nested bodies (inherit `var.*`, `steps.*`, enclosing `each.*`; cannot redeclare `variable` blocks).
  - [ ] Recursion depth limit (4) and cycle detection.

**Acceptance**:

- [ ] Docs render in reviewer's preview.
- [ ] Every example HCL snippet in the new section is valid (paste into a temporary `.hcl` file and `make validate`).

### Step 11 — Workstream cross-doc updates

**Files**: [workstreams/README.md](README.md), [PLAN.md](../PLAN.md).

- [ ] [workstreams/README.md](README.md): add a Phase 1 workstream listing entry for W10 (this workstream) and W11 (the cleanup gate).
- [ ] [PLAN.md](../PLAN.md) Phase 1 section: replace the "TBD" stub (lines ~53–55) with a workstream listing matching the Phase 0 format (lines 31–48), enumerating W01–W11. W10 points at this file; W11 points at `11-phase1-cleanup-gate.md`. (The W11 file already exists post-rename.)
- [ ] Survey root `README.md` for any references that pin to W08 syntax. The `for-each loops` mention in "What's in the box" is generic and remains accurate; do not edit unless a specific W08-syntax snippet is found.

**Acceptance**:

- [ ] `git ls-files workstreams/` shows `10-step-iteration-and-workflow-step.md` and `11-phase1-cleanup-gate.md`; no `10-phase1-cleanup-gate.md`.
- [ ] `grep -rn '10-phase1-cleanup-gate' workstreams/ docs/ README.md PLAN.md` returns no stale references.
- [ ] [11-phase1-cleanup-gate.md](11-phase1-cleanup-gate.md)'s prereq list includes W10.

## Out of scope

- **Recursion depth above 4.** A static depth limit is enforced. If a real use case demands deeper nesting, a follow-up workstream re-evaluates the limit.
- **Deprecation period for the W08 syntax.** The decision is to rip out, not deprecate. Internal consumers migrate as part of Step 9.
- **Parallel iteration / fan-out concurrency.** Iterations execute sequentially. Parallel for_each is a future workstream.
- **Dynamic `count` from in-iteration outputs.** `count` and `for_each` evaluate their source expression once at iteration start; a step's body cannot dynamically grow the iteration set.
- **Variable redeclaration in nested bodies.** Nested workflow bodies inherit parent vars and cannot redeclare `variable` blocks. A future workstream may relax this if needed.
- **Re-introducing the top-level `for_each` block.** Removed by design; do not re-add.

## Files this workstream may modify

- `workflow/schema.go`
- `workflow/compile.go`
- `workflow/compile_steps.go`
- `workflow/eval.go`
- `workflow/iter_cursor.go`
- `internal/engine/engine.go`
- `internal/engine/node_step.go`
- `internal/engine/node.go`
- `internal/engine/runstate.go`
- `internal/engine/extensions.go`
- `internal/cli/reattach.go`
- `internal/cli/reattach_test.go`
- `proto/criteria/v1/events.proto`
- `events/types.go`
- `internal/run/sink.go`
- `internal/run/console_sink.go`
- `docs/workflow.md`
- `examples/README.md`
- `examples/for_each_review_loop.hcl`
- `workstreams/README.md` (Step 11)
- `PLAN.md` (Step 11)

Creates:

- `internal/engine/node_workflow.go`
- `workflow/iteration_compile_test.go`
- `workflow/testdata/iteration/` (multiple fixture files)
- `internal/engine/iteration_engine_test.go`
- `internal/engine/testdata/iteration/` (multiple fixture files)
- `examples/workflow_step_compose.hcl`
- `examples/lib/check.hcl`

Deletes:

- `workflow/compile_foreach_subgraph.go`
- `internal/engine/node_for_each.go`
- `workflow/for_each_subgraph_compile_test.go`
- `internal/engine/node_for_each_multistep_test.go`
- `workflow/testdata/for_each/` (entire directory)
- `internal/engine/testdata/for_each/` (entire directory)

## Tasks

- [ ] Step 1 — extend schema; delete W08 schema surface.
- [ ] Step 2 — recursive nested-workflow compilation; iteration validation; delete `compile_foreach_subgraph.go`.
- [ ] Step 3 — generalize `IterCursor`; cursor stack on `RunState`.
- [ ] Step 4 — `each.*` binding helpers with new fields; indexed step-output helper.
- [ ] Step 5 — runtime per-step iteration; delete `node_for_each.go`; new `node_workflow.go`.
- [ ] Step 6 — reattach validation rewrite.
- [ ] Step 7 — proto + sink rename (keep field numbers).
- [ ] Step 8 — tests and fixtures: rewrite the W08 test surface.
- [ ] Step 9 — examples: rewrite `for_each_review_loop.hcl`; create `workflow_step_compose.hcl`.
- [ ] Step 10 — `docs/workflow.md` rewrite.
- [ ] Step 11 — `workstreams/README.md` and `PLAN.md` cross-doc updates.

## Exit criteria

- All checkboxes in Steps 1–11 ticked.
- `go build ./...` clean.
- `make proto-check-drift`, `make proto-lint`, `make lint-go`, `make lint-imports`, `make test` (with `-race`), `make test-conformance`, `make validate`, `make ci` all green.
- `./bin/criteria apply examples/for_each_review_loop.hcl --events-file /tmp/events.ndjson` exits 0; events show 3 iterations × 3 body steps each; terminal outcome `all_succeeded`.
- `./bin/criteria apply examples/workflow_step_compose.hcl` exits 0.
- Crash-resume drill: start a long-running workflow with `count = 5`, kill mid-iteration, reattach, confirm correct completion with indexed outputs and `_prev` re-bound.
- Reduce drill: run a `running_total` workflow over `[1,2,3,4]`, assert final iteration's exposed total equals 10.
- `grep -rn 'for_each "[^"]*"\s*{' .` returns no hits outside `workstreams/archived/`, `workstreams/08-for-each-multistep.md`, and reviewer notes.

## Tests

See Step 8 for the full test list. Two non-negotiable invariants from W08's review history apply here:

1. **Tests must use a value-capturing loader, not noop**, anywhere `each.*` binding correctness is being asserted (W08 review R1/R2). Noop-based tests would pass even if the implementation never bound `each.value` — direct regression against the core guarantee.
2. **Crash-resume tests must verify that `each.*` (including `_prev`) was actually re-bound after resume**, not just that the run reached terminal state. Use the capturing loader.

## Risks

| Risk | Mitigation |
|---|---|
| W08 fixture authors elsewhere in the repo (not just in `for_each_review_loop.hcl`) miss the migration | Step 8 deletes the W08 test directories outright; CI's `make validate` will fail on any remaining HCL fixture using the old syntax. The `grep` exit-criterion in Step 8 catches stragglers. |
| `_prev` cursor size grows large (big output objects bloat checkpoints) | Cap output object size at runtime (target: ≤ 64 KB serialized) and surface a clear error if exceeded. Document in `docs/workflow.md`. |
| Recursion via `workflow_file` cycles or pathologically deep nesting | Compile-time depth limit (default 4) and load-stack cycle detection in `SubWorkflowResolver`. |
| Proto field rename breaks event consumers | Keep field numbers stable (rename messages only). Document in the proto file with a comment. Verify any persisted NDJSON in `internal/run/testdata/` round-trips. |
| `_prev` semantics under failure are unclear (especially under `continue` with a failed prior iteration) | Document explicitly: under `continue`, `_prev` is the prior iteration's evaluated `output` block values regardless of that iteration's outcome. Reduce authors guard with `each._prev != null && !steps.<inner>._failed` (or by exporting a status output). Under `abort`, `_prev` is never re-read. |
| Variable-scope confusion in nested bodies | Document strictly: nested bodies inherit `var.*`, `steps.*`, and any enclosing `each.*`; they cannot redeclare `variable` blocks. Add a compile-time diagnostic for redeclaration. |
| Agent registry lookup in nested bodies | `compileAgents` runs at the top level only; nested steps look up agents in the top-level registry. Add a test that confirms a nested step using `agent = "foo"` resolves correctly. |
| The body's terminal-state requirement is unclear | Iterating bodies must transition to `_continue` to advance, or to a parent-graph target to early-exit. Compile-time check enforces a `_continue` path exists. Non-iterating workflow-step bodies advance to outer outcomes via terminal states inside the body. |
| Mixed-type tuples for `for_each` | HCL/cty tuples support mixed types; the iteration code already handles `[]cty.Value`. Add a test to confirm. |
