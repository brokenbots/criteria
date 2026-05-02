# Workstream 12 — Adapter lifecycle automation (drop explicit `lifecycle = "open"|"close"`)

**Phase:** 3 · **Track:** C · **Owner:** Workstream executor · **Depends on:** [11-agent-to-adapter-rename.md](11-agent-to-adapter-rename.md). · **Unblocks:** [13-subworkflow-block-and-resolver.md](13-subworkflow-block-and-resolver.md) (subworkflow scope isolation depends on automatic per-scope adapter session lifecycle).

## Context

[architecture_notes.md §6](../../architecture_notes.md) and [proposed_hcl.hcl](../../proposed_hcl.hcl) move adapter lifecycle from explicit step attributes (`step "x" { lifecycle = "open" }`, `step "y" { lifecycle = "close" }`) to **automatic, scope-bound provisioning and teardown**:

> **Initialization:** When a workflow (or subworkflow) begins execution, the engine automatically provisions and initializes all `adapter` blocks declared in that scope.
>
> **Execution:** Any `step` within that workflow referencing an adapter shares this initialized session. Long-lived context is maintained automatically.
>
> **Teardown:** When the workflow reaches a terminal state, the engine automatically closes the adapter sessions bound to that scope.
>
> **Subworkflow Isolation:** If a subworkflow declares its own `adapter` block, a fresh adapter session is spun up and torn down explicitly with the subworkflow.

The `lifecycle` step attribute is **removed**. No alias, no deprecation cycle. Workflows that used `lifecycle = "open"` / `lifecycle = "close"` steps must delete those steps; the engine takes over the provisioning automatically.

## Prerequisites

- [11-agent-to-adapter-rename.md](11-agent-to-adapter-rename.md) merged: `Adapters` map exists on `FSMGraph`; the schema is renamed.
- `make ci` green on `main`.

## In scope

### Step 1 — Remove `lifecycle` from `StepSpec`

In [workflow/schema.go](../../workflow/schema.go):

```go
// BEFORE
type StepSpec struct {
    ...
    Lifecycle string `hcl:"lifecycle,optional"`
    ...
}

// AFTER — Lifecycle field DELETED
```

In `StepNode` similarly delete the `Lifecycle string` field.

A step with `lifecycle = "..."` in HCL produces a parse error via the legacy-rejection mechanism from [11](11-agent-to-adapter-rename.md). Extend `rejectLegacyBlocks` (or its attribute-level sibling, `rejectLegacyAttrs`) to add `lifecycle` to the rejected step attributes. Error message:

```
attribute "lifecycle" was removed in v0.3.0 — adapter lifecycle is automatic.
Delete this step. The engine provisions and tears down adapter sessions at
workflow scope boundaries. See CHANGELOG.md migration note.
```

### Step 2 — Engine: scope-start adapter provisioning

In [internal/engine/](../../internal/engine/), find the workflow-start path (likely in [internal/engine/engine.go](../../internal/engine/engine.go) or [internal/engine/run.go](../../internal/engine/run.go)). Before the first step executes:

```go
// initScopeAdapters walks g.Adapters and asks the SessionManager to provision
// every declared adapter. Returns a map of "<type>.<name>" → SessionHandle.
// Errors abort the run before any step executes; partial provisioning is
// torn down via the symmetric tearDownScopeAdapters call.
func initScopeAdapters(ctx context.Context, g *workflow.FSMGraph, deps Deps) (map[string]SessionHandle, error)
```

Existing `SessionManager` (or whatever the abstraction is called in [internal/plugin/](../../internal/plugin/) and [internal/engine/runtime/](../../internal/engine/runtime/)) already supports session creation. Reuse — do not reimplement.

Provisioning happens in **declaration order** (use `g.Adapters`'s ordered iteration; if the map doesn't preserve order, also store an `AdapterOrder []string` on `FSMGraph` per [11](11-agent-to-adapter-rename.md)'s pattern for `OutputOrder`).

Failure handling:

- If any adapter fails to initialize, tear down every adapter that succeeded so far (in reverse order), emit an event for the failure, and return the error.
- The run does not transition to any terminal state — it never started. Status: `failure`, reason: `adapter_init_failed`.

### Step 3 — Engine: scope-terminal adapter teardown

In the symmetric path (terminal state reached, run cancelled, run errored):

```go
// tearDownScopeAdapters releases every session in handles in reverse order.
// Errors during teardown are logged via a dedicated lifecycle sink hook
// (per Phase 2 W12) but do not change the run's terminal state.
func tearDownScopeAdapters(ctx context.Context, handles map[string]SessionHandle, deps Deps)
```

Always called. Specifically:

- Terminal state reached → teardown runs after output evaluation ([09](09-output-block.md)) and before `run.finished` event emission.
- Run cancelled or errored → teardown runs in a `defer` from the run's main loop.
- Process exit (SIGTERM/SIGINT) → teardown runs as part of the existing signal-handling cleanup. Confirm by reading [internal/cli/apply.go](../../internal/cli/apply.go) (after [02](02-split-cli-apply.md), [internal/cli/apply_local.go](../../internal/cli/apply_local.go) and [internal/cli/apply_server.go](../../internal/cli/apply_server.go)).

### Step 4 — Subworkflow scope isolation

Per [architecture_notes.md §6](../../architecture_notes.md):

> If a subworkflow declares its own `adapter` block, a fresh adapter session is spun up and torn down explicitly with the subworkflow.

In [internal/engine/node_workflow.go](../../internal/engine/node_workflow.go) `runWorkflowBody` (already touched by [08](08-schema-unification.md) to drop `Vars` aliasing):

- At body entry: call `initScopeAdapters(ctx, body, deps)` for the body's own `g.Adapters`. Note that with [08](08-schema-unification.md) the body IS a `Spec` so it has its own `g.Adapters`.
- At body terminal: call `tearDownScopeAdapters(ctx, bodyHandles, deps)`.

The handles map is **scope-local** — it does not merge with the parent scope's handles. A step inside the body can reference only adapters declared in the body's scope or **explicitly inherited** via parent input binding. **Decision (this workstream):** explicit-only — there is no implicit parent-adapter visibility. A body that wants to use a parent adapter must declare its own.

This may seem heavy for the common case where a body wants to use the same Copilot session as the parent. The trade-off is correctness: implicit parent-adapter visibility re-introduces the runtime aliasing [08](08-schema-unification.md) explicitly removed. The Phase 4 environment-plug architecture is the right place to add cross-scope session reuse if it's needed; for v0.3.0, every scope owns its sessions.

### Step 5 — Lifecycle events

Phase 2's W12 added `OnAdapterLifecycle` sink hook ([archived/v2/12-lifecycle-log-clarity.md](../archived/v2/12-lifecycle-log-clarity.md)). Plumb the new automatic provisioning into that hook:

- Emit `adapter.session.opened` (or whatever the W12-defined event is named) at provision time.
- Emit `adapter.session.closed` at teardown.
- The `step.adapter_open` / `step.adapter_close` events tied to the legacy `lifecycle = ...` step are **gone** because those steps are gone. Cancellation events for failed init are new: `adapter.session.init_failed` with the underlying error.

Confirm by reading the W12 events from [events/](../../events/) and aligning the new emissions with the existing taxonomy.

### Step 6 — Examples and goldens

Sweep [examples/](../../examples/) for any HCL that uses `lifecycle = "open"` or `lifecycle = "close"`. Delete those steps; the engine takes over.

Re-run `make validate`. If any example fails because it relied on the explicit lifecycle steps for sequencing (e.g. a step depended on running after the open), the workflow's intent must be re-expressed via step ordering. Document each such migration in reviewer notes.

Regenerate compile/plan goldens.

### Step 7 — Migration note text

For [21-phase3-cleanup-gate.md](21-phase3-cleanup-gate.md):

```
### `lifecycle = "open"|"close"` step attribute removed

v0.2.0 form:
    step "open_session" {
        adapter = "copilot"
        lifecycle = "open"
    }
    step "do_work" { adapter = "copilot.reviewer" ... }
    step "close_session" {
        adapter = "copilot"
        lifecycle = "close"
    }

v0.3.0 form:
    adapter "copilot" "reviewer" { ... }
    step "do_work" { adapter = copilot.reviewer ... }

The engine provisions and tears down the adapter session automatically at
workflow scope start and terminal state. Subworkflows have their own
isolated session lifecycles.
```

### Step 8 — Tests

- `workflow/compile_steps_*_test.go` (the per-kind tests from [03](03-split-compile-steps.md)):
  - `TestStep_LegacyLifecycleAttr_HardError` — `step { lifecycle = "open" }` produces a parse error with the documented message.

- `internal/engine/lifecycle_test.go`:
  - `TestEngine_AdapterAutoProvisionAtScopeStart` — adapter init runs before first step.
  - `TestEngine_AdapterAutoTeardownAtTerminal` — teardown runs after terminal state, before run.finished.
  - `TestEngine_AdapterTeardownOnError` — run that errors out still tears down.
  - `TestEngine_AdapterTeardownOnCancel` — run cancelled mid-step still tears down.
  - `TestEngine_AdapterInitFailureRollsBack` — second adapter init fails; first is torn down; run aborts.
  - `TestEngine_AdapterInitOrder` — adapters initialize in declaration order.

- `internal/engine/node_workflow_test.go`:
  - `TestRunWorkflowBody_BodyAdapterIsolated` — body's adapter is provisioned at body entry, torn down at body terminal, NOT shared with parent.
  - `TestRunWorkflowBody_BodyDoesNotInheritParentAdapter` — body referencing a parent-scope adapter compile-errors.

- Conformance (in [sdk/conformance/](../../sdk/conformance/)):
  - `LifecycleAutomatic` test: a subject runs a workflow with declared adapters; expects open/close events at scope boundaries.

### Step 9 — Validation

```sh
go build ./...
go test -race -count=2 ./workflow/... ./internal/engine/... ./internal/plugin/... ./internal/cli/...
make validate
make test-conformance
make lint-go
make lint-baseline-check
make ci
git grep -nE 'Lifecycle\s+string|hcl:"lifecycle' -- ':!*_test.go' ':!docs/' ':!CHANGELOG.md' ':!workstreams/'
```

Final `git grep` MUST return zero matches in production code.

## Behavior change

**Behavior change: yes — breaking for HCL authors.**

Observable differences:

1. `step "x" { lifecycle = "open"|"close" }` is a hard parse error.
2. Adapter sessions provision automatically at workflow start.
3. Adapter sessions tear down automatically at terminal state, error, or cancel.
4. New events: `adapter.session.opened` / `adapter.session.closed` / `adapter.session.init_failed`.
5. Subworkflow bodies isolate their own adapter sessions.

Migration note recorded for [21](21-phase3-cleanup-gate.md).

No proto change beyond what [11](11-agent-to-adapter-rename.md) already did. New event types follow the existing event-emission infrastructure.

## Reuse

- Existing `SessionManager` / session abstraction in [internal/engine/runtime/](../../internal/engine/runtime/) and [internal/plugin/](../../internal/plugin/) — do not reimplement.
- Phase 2 W12 `OnAdapterLifecycle` sink hook — emit through it.
- Existing terminal-state handling and signal-cleanup paths in [internal/cli/](../../internal/cli/).
- `runWorkflowBody` shape from [08](08-schema-unification.md).

## Out of scope

- Per-step adapter session reuse for adapters NOT declared at scope start (i.e. lazy adapter initialization). Phase 4 may add it; not v0.3.0.
- Cross-scope adapter session sharing. Explicitly out per Step 4 decision.
- Adapter session pooling. Each adapter is one session per workflow scope.
- Process-lifetime session reuse across workflow runs. Each `criteria apply` is a fresh process.

## Files this workstream may modify

- [`workflow/schema.go`](../../workflow/schema.go) — delete `StepSpec.Lifecycle`, `StepNode.Lifecycle`.
- `workflow/parse_legacy_reject.go` (from [11](11-agent-to-adapter-rename.md)) — extend with `lifecycle` attribute rejection.
- `workflow/compile_steps_*.go` — remove the lifecycle-step compile branches; treat all steps as work-doing.
- New: `internal/engine/lifecycle.go` — `initScopeAdapters` / `tearDownScopeAdapters`.
- [`internal/engine/engine.go`](../../internal/engine/engine.go) (or run loop) — scope-start init, scope-end teardown.
- [`internal/engine/node_workflow.go`](../../internal/engine/node_workflow.go) — body-scope init/teardown.
- [`internal/cli/apply_local.go`](../../internal/cli/apply_local.go) and [`internal/cli/apply_server.go`](../../internal/cli/apply_server.go) — signal-cleanup teardown invocation.
- [`events/`](../../events/) — new `adapter.session.opened|closed|init_failed` event types.
- All test files needing updates.
- New: `internal/engine/lifecycle_test.go`.
- All affected example HCL files in [`examples/`](../../examples/).
- Goldens under [`internal/cli/testdata/`](../../internal/cli/testdata/).
- [`docs/workflow.md`](../../docs/workflow.md) — adapter lifecycle section rewrite.
- [`sdk/conformance/`](../../sdk/conformance/) — new `LifecycleAutomatic` conformance test.

This workstream may **not** edit:

- `PLAN.md`, `README.md`, `AGENTS.md`, `CHANGELOG.md`, `workstreams/README.md`, or any other workstream file.
- `.proto` files (no proto change required).
- The session abstraction's interface — implement against it, do not change it.

## Tasks

- [ ] Delete `Lifecycle` field from schema (Step 1).
- [ ] Extend legacy-rejection to surface a hard error for `lifecycle = ...` (Step 1).
- [ ] Implement `initScopeAdapters` and `tearDownScopeAdapters` (Step 2, Step 3).
- [ ] Wire scope-start init at run start and at body entry (Step 2, Step 4).
- [ ] Wire scope-end teardown at terminal/error/cancel (Step 3, Step 4).
- [ ] Plumb lifecycle events (Step 5).
- [ ] Update examples; regenerate goldens (Step 6).
- [ ] Record migration text in reviewer notes (Step 7).
- [ ] Author all required tests including conformance (Step 8).
- [ ] `make ci`, `make test-conformance` green; final grep zero (Step 9).

## Exit criteria

- `git grep 'Lifecycle string'` returns zero in production code.
- `git grep 'hcl:"lifecycle'` returns zero in production code.
- `step { lifecycle = "..." }` HCL produces a hard parse error with the migration message.
- Adapters auto-init at scope start; failures roll back partial provisioning.
- Adapters auto-tear-down at terminal/error/cancel.
- Subworkflow bodies isolate their adapter lifecycles.
- New `adapter.session.{opened|closed|init_failed}` events emitted.
- Conformance test `LifecycleAutomatic` passes.
- Examples updated; `make validate` green.
- Migration text in reviewer notes.
- `make ci` exits 0.

## Tests

The Step 8 list is the deliverable. Coverage targets:

- `internal/engine/lifecycle.go` ≥ 90%.
- The body-scope isolation path ≥ 85%.

## Risks

| Risk | Mitigation |
|---|---|
| Existing workflows use `lifecycle = "open"` to delay session provisioning until a specific step runs | The auto-init runs at scope start. A workflow that wanted lazy init can no longer express it. Decision: lazy init is out of scope; the workflow author moves the conditional into the adapter or accepts eager init. Document explicitly. |
| Teardown failures hide real adapter bugs | Teardown errors emit `adapter.session.close_failed` events but do not change the run's terminal status. Operators who care about teardown can subscribe to the event. |
| Subworkflow body isolation is too strict for the common case (parent and body share a long-lived adapter) | The Phase 4 environment-plug architecture is the right place to add cross-scope session reuse. v0.3.0 explicit isolation is the simpler, correct default. |
| The session abstraction in [internal/plugin/](../../internal/plugin/) doesn't currently support fail-rollback | Add a small helper `Provisioned` slice + reverse-order `Close` loop in `initScopeAdapters`. No interface change required. |
| Signal-cleanup at process exit doesn't reach the teardown path on SIGKILL | `SIGKILL` is unhandlable — accept that the OS reaps. For SIGTERM/SIGINT (handlable), confirm the existing handler invokes the new teardown path. Add a test using `cmd.Process.Signal(syscall.SIGTERM)`. |
| Examples that used lifecycle steps had implicit ordering invariants the rewrite breaks | Map each removed lifecycle step to its work-doing dependent steps; the engine's auto-provisioning happens before the first step, which is at least as early as the original lifecycle = open. The dependency direction is preserved. |
