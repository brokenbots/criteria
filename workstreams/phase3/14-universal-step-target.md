# Workstream 14 — Universal step `target` attribute

**Phase:** 3 · **Track:** C · **Owner:** Workstream executor · **Depends on:** [11-agent-to-adapter-rename.md](11-agent-to-adapter-rename.md), [13-subworkflow-block-and-resolver.md](13-subworkflow-block-and-resolver.md). · **Unblocks:** [15-outcome-block-and-return.md](15-outcome-block-and-return.md), [16-switch-and-if-flow-control.md](16-switch-and-if-flow-control.md), [19-parallel-step-modifier.md](19-parallel-step-modifier.md), [20-implicit-input-chaining.md](20-implicit-input-chaining.md).

## Context

[proposed_hcl.hcl §4](../../proposed_hcl.hcl) replaces the step-kind bifurcation (`adapter = "..."` vs. `agent = "..."` vs. `type = "workflow"` + `workflow {...}`) with a **single `target = ...`** attribute that uniformly references one of:

- `adapter.<type>.<name>` — invoke the named adapter declaration.
- `subworkflow.<name>` — invoke the named subworkflow declaration.
- `step.<name>` — chain to a sibling step within the same scope (rare, primarily for fan-in patterns).

Examples:

```hcl
step "do_review" {
    target = adapter.copilot.reviewer
    input = { task_id = each.value }
}

step "fork_to_inner" {
    target = subworkflow.review_loop
    input = { item = each.value }
}
```

This is a structural simplification: the engine routes by the resolved target reference, not by which schema field is set. After this workstream there is **no step-kind dispatch** at the schema level.

## Prerequisites

- [11](11-agent-to-adapter-rename.md): adapter block + dotted reference shape.
- [13](13-subworkflow-block-and-resolver.md): subworkflow block + resolver wiring.
- [03](03-split-compile-steps.md): the per-kind compile files exist (this workstream collapses them; the split makes the collapse easy to review).
- `make ci` green.

## In scope

### Step 1 — Schema reshape

In [workflow/schema.go](../../workflow/schema.go) `StepSpec`:

```go
// BEFORE (post-[11], post-[13])
type StepSpec struct {
    Name      string `hcl:"name,label"`
    Adapter   string `hcl:"adapter,optional"`     // dotted: <type>.<name>
    OnCrash   string `hcl:"on_crash,optional"`
    Type      string `hcl:"type,optional"`        // "" or "workflow"  (already removed by [13])
    OnFailure string `hcl:"on_failure,optional"`
    MaxVisits int    `hcl:"max_visits,optional"`
    ...
    Outcomes  []OutcomeSpec `hcl:"outcome,block"`
    Remain    hcl.Body      `hcl:",remain"`
    ...
}

// AFTER
type StepSpec struct {
    Name        string   `hcl:"name,label"`
    Target      hcl.Expression  // captured from Remain; required
    Environment string   `hcl:"environment,optional"`  // overrides adapter/scope environment
    OnCrash     string   `hcl:"on_crash,optional"`
    OnFailure   string   `hcl:"on_failure,optional"`
    MaxVisits   int      `hcl:"max_visits,optional"`
    Input       hcl.Expression  // captured from Remain; optional
    Outcomes    []OutcomeSpec   `hcl:"outcome,block"`
    Remain      hcl.Body        `hcl:",remain"`
    ...
}
```

Notes:

- `Target` and `Input` are captured via `Remain.JustAttributes()` because `gohcl` does not decode `hcl.Expression` into struct fields directly. Same pattern the existing `ForEach` / `Count` / `BranchSpec.Arms[].Remain` use.
- `Adapter` field is **deleted**. The dotted reference moves to the `target` attribute value (`target = adapter.copilot.reviewer`).
- The `step.workflow { ... }` inline form is already gone ([13](13-subworkflow-block-and-resolver.md)).

### Step 2 — Compile-time `target` resolution

In `workflow/compile_steps.go` (the dispatcher slimmed by [03](03-split-compile-steps.md)), the dispatch logic changes from "switch on step kind fields" to "resolve target reference":

```go
func compileStep(g *FSMGraph, sp *StepSpec, schemas map[string]AdapterInfo, opts CompileOpts) hcl.Diagnostics {
    target, kind, refName, diags := resolveStepTarget(sp.Target, g)
    if diags.HasErrors() {
        return diags
    }
    switch kind {
    case stepTargetAdapter:
        return compileAdapterStep(g, sp, target, refName, schemas, opts)
    case stepTargetSubworkflow:
        return compileSubworkflowStep(g, sp, target, refName, opts)
    case stepTargetStep:
        return compileChainStep(g, sp, target, refName, opts)
    }
    return diags
}
```

`resolveStepTarget` returns:

- `kind` ∈ `{stepTargetAdapter, stepTargetSubworkflow, stepTargetStep}`.
- `refName` — the resolved adapter / subworkflow / step name.
- Diagnostic if the target reference does not resolve to a declared entity.

`stepTargetStep` (chaining to a sibling step) is the least common case and primarily used for explicit fan-in. Validate that the target step exists in `g.Steps` and document semantics: chaining is a transition, not an invocation; the target step's outcome routing applies. **Decision (per [proposed_hcl.hcl §4](../../proposed_hcl.hcl)):** ship `target = step.<name>` as a first-class chain. Existing `transition_to` style chaining belongs in `outcome` blocks (per [15](15-outcome-block-and-return.md)) — `target = step.<name>` is for the rare case where the entire step IS the chain (e.g. an iteration step whose body just hands off).

If `target = step.<name>` introduces ambiguity with outcome-block chaining, simplify by making `target = step.<name>` an error in v0.3.0 ("step-to-step routing belongs in outcome blocks; use target = adapter.X or target = subworkflow.X"). Defer to the workstream executor's judgement during implementation; default to **rejecting** `target = step.<name>` if it complicates routing — the universal `target` attribute still serves its main purpose with adapter/subworkflow.

### Step 3 — Compiled `StepNode` reshape

```go
type StepNode struct {
    Name        string
    TargetKind  StepTargetKind   // adapter | subworkflow | (step, if not rejected)
    AdapterRef  string           // "<type>.<name>" if TargetKind == adapter
    SubworkflowRef string        // "<name>" if TargetKind == subworkflow
    Environment string           // override ("<env_type>.<env_name>"); empty = use scope default
    OnCrash     string
    OnFailure   string
    MaxVisits   int
    InputExprs  map[string]hcl.Expression
    Timeout     time.Duration
    Outcomes    map[string]string
    AllowTools  []string
    ForEach     hcl.Expression
    Count       hcl.Expression
    Parallel    hcl.Expression  // [19] adds this; this workstream's StepNode reserves the field but does not populate
}
```

Delete fields: `Adapter` (the dotted ref moves to AdapterRef populated from the resolved target), `Type`, `Body`, `BodyEntry`, `Outputs` (those moved to `SubworkflowNode` per [13](13-subworkflow-block-and-resolver.md)).

### Step 4 — Engine routing by target kind

In [internal/engine/node_step.go](../../internal/engine/node_step.go), the step's `Evaluate` method routes by `TargetKind`:

```go
switch n.TargetKind {
case StepTargetAdapter:
    // existing adapter-execution path
case StepTargetSubworkflow:
    // call into runSubworkflow ([13])
case StepTargetStep:
    // direct transition to the named step (if Step 2 kept this kind)
}
```

For the subworkflow case, this workstream wires the call into [13-subworkflow-block-and-resolver.md](13-subworkflow-block-and-resolver.md)'s `runSubworkflow`. The step's `input` expression evaluates against the parent's eval context and is passed through as the subworkflow's input bindings.

### Step 5 — Step-level `environment = ...` override

Per [10-environment-block.md](10-environment-block.md) the env declaration surface exists. Per [11](11-agent-to-adapter-rename.md) the adapter block declares its environment. This workstream adds the **per-step override**:

If a step has `environment = shell.ci`:

- Validate the reference at compile (must exist in `g.Environments`).
- At runtime, the step's adapter session is invoked with that environment's variables (overriding the adapter-block's environment, overriding the workflow default).

The override has effect only for the current step's execution. Subsequent steps revert to their own resolved environment. **Decision:** environment overrides do not change the underlying adapter session — they only affect the env-var injection for the subprocess invocation of that step. (This matches [10](10-environment-block.md)'s "v0.3.0 only injects env vars" decision.)

### Step 6 — Migration

Hard parse error for any step that uses the legacy `adapter = "..."` shape (note: **non-dotted** reference; the dotted form `adapter = copilot.reviewer` was the [11](11-agent-to-adapter-rename.md) intermediate state and is also removed here):

```
attribute "adapter" was removed in v0.3.0 — use target = adapter.<type>.<name> instead.
See CHANGELOG.md migration note.
```

Update [11](11-agent-to-adapter-rename.md)'s rejection helper to add this attribute. Coordinate via reviewer notes — this workstream does not edit [11](11-agent-to-adapter-rename.md)'s files; instead, this workstream's `parse_legacy_reject.go` extension lives alongside [11](11-agent-to-adapter-rename.md)'s. Single file, two-workstream cumulative content.

Migration text for [21](21-phase3-cleanup-gate.md):

```
### `step.adapter = ...` and `step.agent = ...` → `step.target = ...`

v0.2.0 form:
    step "review" { adapter = "copilot" }
    step "review" { agent = "reviewer" }

v0.3.0 (transitional, [11]):
    step "review" { adapter = copilot.reviewer }

v0.3.0 final ([14]):
    step "review" { target = adapter.copilot.reviewer }
```

### Step 7 — Examples and goldens

Sweep every example HCL under [examples/](../../examples/). Convert every step to the new `target` attribute. Regenerate goldens.

Update [docs/workflow.md](../../docs/workflow.md):

- Steps section explains `target` and the three reference kinds.
- Optional environment override.

### Step 8 — Tests

- Compile:
  - `TestCompileStep_TargetAdapter`.
  - `TestCompileStep_TargetSubworkflow`.
  - `TestCompileStep_TargetUnresolvedAdapter` — error.
  - `TestCompileStep_TargetUnresolvedSubworkflow` — error.
  - `TestCompileStep_LegacyAdapterAttr_HardError`.
  - `TestCompileStep_EnvironmentOverride_Resolves`.
  - `TestCompileStep_EnvironmentOverride_Missing` — error.

- Engine:
  - `TestStep_Evaluate_AdapterTarget`.
  - `TestStep_Evaluate_SubworkflowTarget`.
  - `TestStep_EnvironmentOverride_AppliesToSubprocess`.

- End-to-end: every example runs.

### Step 9 — Validation

```sh
go build ./...
go test -race -count=2 ./...
make validate
make test-conformance
make ci
git grep -nE 'hcl:"adapter,optional"|hcl:"agent,optional"' -- ':!*_test.go' ':!docs/' ':!CHANGELOG.md' ':!workstreams/'
```

Final grep MUST return zero matches in production code.

## Behavior change

**Behavior change: yes — breaking.**

Observable differences:

1. `step.target = <reference>` is **required**. A step without `target` is a compile error.
2. `step.adapter = ...` and `step.agent = ...` are hard parse errors.
3. New `step.environment = ...` attribute (optional override).

Migration text for [21](21-phase3-cleanup-gate.md) per Step 6.

## Reuse

- Existing routing logic in [`internal/engine/node_step.go`](../../internal/engine/node_step.go) — extend for `TargetKind`-based dispatch.
- [`runSubworkflow`](../../internal/engine/node_subworkflow.go) from [13](13-subworkflow-block-and-resolver.md).
- The HCL traversal-resolution helper that already exists for parsing dotted references (`adapter.foo.bar` resolves to a `[]hcl.Traverser`).

## Out of scope

- The `outcome` block and `return` outcome. Owned by [15-outcome-block-and-return.md](15-outcome-block-and-return.md).
- `parallel` modifier. Owned by [19-parallel-step-modifier.md](19-parallel-step-modifier.md).
- Implicit input chaining. Owned by [20-implicit-input-chaining.md](20-implicit-input-chaining.md).
- New target kinds beyond adapter/subworkflow/(step). HCL function calls as targets, etc., are out of scope.

## Files this workstream may modify

- [`workflow/schema.go`](../../workflow/schema.go) — reshape `StepSpec` and `StepNode`.
- `workflow/compile_steps.go` (dispatcher) — replace step-kind switch with target resolution.
- `workflow/compile_steps_*.go` — per-kind compilers updated to take a resolved target.
- New: `workflow/compile_step_target.go` — `resolveStepTarget` helper.
- [`internal/engine/node_step.go`](../../internal/engine/node_step.go) — dispatch by `TargetKind`.
- `workflow/parse_legacy_reject.go` — extend with `step.adapter`/`step.agent`/`step.type` rejection.
- All example HCL files under [`examples/`](../../examples/).
- Goldens under [`internal/cli/testdata/`](../../internal/cli/testdata/).
- [`docs/workflow.md`](../../docs/workflow.md).
- New tests.

This workstream may **not** edit:

- `PLAN.md`, `README.md`, `AGENTS.md`, `CHANGELOG.md`, `workstreams/README.md`, or any other workstream file.
- `.proto` files.
- The `adapter` block schema ([11](11-agent-to-adapter-rename.md)).
- The `subworkflow` block schema ([13](13-subworkflow-block-and-resolver.md)).

## Tasks

- [ ] Reshape `StepSpec` and `StepNode` (Step 1, Step 3).
- [ ] Implement `resolveStepTarget` (Step 2).
- [ ] Engine dispatch by target kind (Step 4).
- [ ] Step-level `environment` override (Step 5).
- [ ] Legacy parse rejection (Step 6).
- [ ] Sweep examples; regenerate goldens (Step 7).
- [ ] Author tests (Step 8).
- [ ] `make ci` green; final grep zero (Step 9).

## Exit criteria

- `step.target = <reference>` is required and resolves to one of the three target kinds.
- Legacy `step.adapter = ...` / `step.agent = ...` produce hard parse errors with migration messages.
- Step-level `environment = ...` override works.
- All examples updated; `make validate` green.
- All required tests pass.
- `make ci` exits 0.
- Final grep for legacy attribute tags returns zero in production code.

## Tests

The Step 8 list is the deliverable. Coverage: ≥ 90% on the new `compile_step_target.go`.

## Risks

| Risk | Mitigation |
|---|---|
| `target = step.<name>` adds dispatch ambiguity | Step 2 allows the executor to reject this kind in v0.3.0 if it complicates routing. Document the choice. |
| The `Target` `hcl.Expression` decode pattern interacts oddly with `Remain` | Existing `ForEach`/`Count` use the same pattern. Reuse the extraction logic. |
| Step environment override semantics confuse readers ("does it create a new session?") | Document explicitly: override is env-var injection only, not a new session. Test `TestStep_EnvironmentOverride_NewSessionNotCreated`. |
| Legacy-rejection message is too terse | Use the multiline format from [11](11-agent-to-adapter-rename.md)'s rejection messages, with a CHANGELOG pointer. |
| `target` references break HCL `gohcl` decode for unknown reasons | Capture via `Remain.JustAttributes()` as the existing pattern does; do not try to decode `hcl.Expression` into a struct field directly. |
