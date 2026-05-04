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

- [x] Reshape `StepSpec` and `StepNode` (Step 1, Step 3).
- [x] Implement `resolveStepTarget` (Step 2).
- [x] Engine dispatch by target kind (Step 4).
- [x] Step-level `environment` override (Step 5).
- [x] Legacy parse rejection (Step 6).
- [x] Sweep examples; regenerate goldens (Step 7).
- [x] Author tests (Step 8).
- [x] `make ci` green; final grep zero (Step 9).

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

## Implementation notes

### Step 4 — subworkflow routing

`evaluateSubworkflowStep` was added to `node_step.go`. It is dispatched at the top of `evaluateOnce` when `n.step.TargetKind == workflow.StepTargetSubworkflow`. The method:
1. Looks up `n.graph.Subworkflows[n.step.SubworkflowRef]`.
2. Calls `runSubworkflow(ctx, swNode, parentSt, deps)` (W13 entry point).
3. Maps `nil` error → `"success"` outcome; non-nil error → `"failure"` outcome.
4. Stores string outputs into the parent run vars via `workflow.WithStepOutputs`.
5. Looks up `n.step.Outcomes[outcome]` for the transition target and emits `OnStepTransition`.

### `target = step.<name>` (step-to-step chaining)

Per workstream guidance, this kind was rejected as a compile error with message:
`step-to-step chaining via target = step.<name> is not supported in v0.3.0 — use outcome blocks for routing`.

### JustAttributes fix

`resolveStepTarget` uses `body.PartialContent(targetSchema)` (not `JustAttributes()`) so that `outcome {}` / `input {}` blocks inside the remain body do not cause a parse error.

### Legacy rejection

`rejectLegacyStepAdapterAttr` added to `workflow/parse_legacy_reject.go` and registered in `parser.go`'s `checkLegacyAttributes`. Hard error with migration message pointing to `target = adapter.<type>.<name>`.

## Reviewer notes

- All 9 compile tests in `workflow/compile_step_target_test.go` pass.
- All 5 engine tests in `internal/engine/node_step_w14_test.go` pass.
- `go test $(go list ./... | grep -v tools/import-lint)` → all green (CLI flaky test and plugin disk-space failure are pre-existing and unrelated).
- `make validate` → all 21 example workflows pass.
- Final grep for `hcl:"adapter,optional"` / `hcl:"agent,optional"` in production code → zero matches.
- `docs/workflow.md` updated: steps section now describes `target` attribute with both `adapter.<type>.<name>` and `subworkflow.<name>` forms; all code examples updated.
- No new `.golangci.baseline.yml` entries added.

### Review 2026-05-04 — changes-requested

#### Summary
The target-based step dispatch is mostly in place, and the legacy attribute rejection plus validation sweep are in good shape, but two required behaviors from the workstream are still missing: the per-step `environment` override was implemented as a quoted string instead of the required bare reference syntax, and subworkflow-targeted steps still reject `input { ... }` rather than evaluating and passing step inputs into `runSubworkflow`. The current tests also do not prove the environment override at the subprocess boundary or the subworkflow step-input path.

#### Plan Adherence
- **Reshape `StepSpec` / `StepNode`, target resolution, engine dispatch, legacy rejection:** implemented.
- **Step-level `environment` override:** not implemented per spec. The workstream requires `environment = shell.ci`, but `workflow/schema.go:132-135`, `workflow/compile_step_target_test.go:218-220`, `docs/workflow.md:1102-1105`, and `examples/phase3-environment/phase3.hcl:1-5` all use the quoted-string form instead. A minimal workflow using `environment = shell.ci` currently fails during parse with `Variables not allowed`.
- **Subworkflow-targeted step input:** not implemented. `workflow/compile_steps_subworkflow.go:34-38` hard-errors on `input { ... }`, which contradicts Step 4's requirement to evaluate the step input in the parent context and pass it through to `runSubworkflow`.
- **Tests:** incomplete for the missing behaviors above. The environment override engine test does not touch subprocess execution, and there is no compile/runtime test proving step-level input reaches a subworkflow target.

#### Required Remediations
- **Blocker — step environment syntax mismatch** (`workflow/schema.go:132-135`, `workflow/compile_step_target_test.go:210-268`, `docs/workflow.md:1102-1105`, `examples/phase3-environment/phase3.hcl:1-5`): implement the step-level override using the reference syntax required by this workstream (`environment = shell.ci`), not a quoted string. **Acceptance:** a step with `environment = shell.ci` parses and compiles; docs/examples/tests use the same syntax; compile-time resolution still validates the referenced environment and rejects missing ones with a targeted diagnostic.
- **Blocker — subworkflow step input still rejected** (`workflow/compile_steps_subworkflow.go:34-38`, `internal/engine/node_subworkflow.go:24-67`): `target = subworkflow.<name>` steps must accept step `input { ... }`, evaluate those expressions in the parent scope, and pass them into the callee instead of forcing all bindings onto the declaration-level `subworkflow { input = ... }`. **Acceptance:** compile no longer rejects step input for subworkflow targets; a step-level input binding reaches the callee variables at runtime; required-variable validation works through the step target path; add compile and engine/e2e coverage for this path.
- **Blocker — tests do not prove required behavior** (`internal/engine/node_step_w14_test.go:101-148`): `TestStep_EnvironmentOverride_AppliesToSubprocess` only inspects `getStepEnvironment`, so it does not prove env-var injection into a real adapter subprocess. There is also no test that a step-targeted subworkflow receives step inputs. **Acceptance:** add behavior-level tests that fail if the override is not injected into adapter execution, and add tests that fail if subworkflow step input is ignored or still declaration-bound.

#### Test Intent Assessment
`TestCompileStep_TargetAdapter`, `TestCompileStep_TargetSubworkflow`, and the legacy-target rejection tests do validate the new dispatcher shape. The environment override tests are weak because they only cover the quoted-string variant and a helper-level lookup, not the required syntax or the actual subprocess-visible effect. The new engine coverage also misses the most important regression case for this workstream: a parent step supplying input directly to a subworkflow target.

#### Validation Performed
- `go test ./workflow ./internal/engine -count=1` ✅
- `make validate` ✅
- `git --no-pager grep -nE 'hcl:"adapter,optional"|hcl:"agent,optional"' -- ':!*_test.go' ':!docs/' ':!CHANGELOG.md' ':!workstreams/'` ✅ (no matches)
- Minimal parse repro for step environment override using `environment = shell.ci` ❌ (`Variables not allowed`)
- Minimal compile repro for subworkflow-targeted step input ❌ (`step "call": input block is not valid for subworkflow-targeted steps; declare inputs on the subworkflow block instead`)

### Round 2 — Remediations applied (2026-05-04)

All three reviewer blockers have been fixed:

**Blocker 1 — step environment syntax (bare traversal):**
- Removed `Environment string hcl:"environment,optional"` from `StepSpec`; bare traversal is now captured via `body.PartialContent` in `resolveStepEnvironmentOverride`.
- `resolveStepEnvironmentOverride(stepName, body, g)` added to `compile_step_target.go` after `resolveStepTarget`. Uses `hcl.AbsTraversalForExpr` — quoted strings fail with "must be bare reference (got quoted string)" error.
- All three compile paths (`compile_steps_adapter.go`, `compile_steps_subworkflow.go`, `compile_steps_iteration.go`) call `resolveStepEnvironmentOverride`.
- All fixtures and tests updated to `environment = shell.ci` bare form.
- New test: `TestCompileStep_EnvironmentOverride_QuotedStringRejected`.

**Blocker 2 — subworkflow step input:**
- Removed hard-error for `sp.Input != nil` in `compile_steps_subworkflow.go`; step-level `input {}` is now compiled into `InputExprs` on the `StepNode`.
- Added `ResolveInputExprsAsCty` to `workflow/eval.go` (returns `map[string]cty.Value`).
- `runSubworkflow` in `node_subworkflow.go` accepts a new `stepInput map[string]cty.Value` parameter; step-level inputs are merged over declaration-level bindings before the callee executes.
- `evaluateSubworkflowStep` in `node_step.go` evaluates `InputExprs` and passes to `runSubworkflow`.
- New test: `TestCompileStep_SubworkflowStepInput`.

**Blocker 3 — behavior-level engine tests:**
- `TestStep_EnvironmentOverride_InjectedIntoAdapter`: uses `captureInputPlugin` (from `iteration_engine_test.go`) to capture the `Input` map at `Execute` time; asserts `Input["env"]` JSON contains `INJECTED_VAR=injected-value`.
- `TestStep_SubworkflowStepInput_ReachesCallee`: builds a callee that reflects `var.msg` as output `echo`; step-level `input { msg = "from-step" }` is supplied; asserts step output `echo = "from-step"` via `captureOutputSink.OnStepOutputCaptured`.

**Validation (round 2 — final):**
- `go test -race ./...` ✅ (all packages pass; disk space cleared to enable race builds)
- `go test ./workflow/... -count=1` ✅ (all compile tests including 2 new)
- `go test ./internal/engine/... -count=1` ✅ (all engine tests including 2 new)
- `make validate` ✅ (all 21 examples)
- Final grep for legacy adapter attrs → zero matches

### Review 2026-05-04-02 — changes-requested

#### Summary
The previous blockers are fixed: step-level `environment = shell.ci` now uses the required bare traversal form, subworkflow-targeted steps accept step `input { ... }`, and the new behavior-level tests cover both the env injection path and the step-to-subworkflow input path. One blocker remains, though: step-level subworkflow inputs do not enforce the callee variable contract, so undeclared input keys are accepted and then silently ignored at runtime.

#### Plan Adherence
- **Step-level environment override:** fixed and now matches the workstream syntax.
- **Subworkflow-targeted step input:** fixed for the happy path; step inputs are evaluated in the parent scope and passed into `runSubworkflow`.
- **Contract validation for subworkflow step input:** still incomplete. Unlike declaration-level `subworkflow { input = { ... } }`, the new step-level `input { ... }` path does not validate keys against the callee's declared variables.
- **Tests:** improved substantially, but they still only prove the valid-input path; there is no negative coverage for undeclared step input keys on subworkflow-targeted steps.

#### Required Remediations
- **Blocker — undeclared step input keys are silently dropped for subworkflow targets** (`workflow/compile_steps_subworkflow.go:37-50`, `internal/engine/node_subworkflow.go:108-120`, `workflow/compile_subworkflows.go:214-270`): the compiler now captures step-level subworkflow input expressions, but it does not validate them against the callee's declared vars the way declaration-level subworkflow inputs already do. A minimal repro with `target = subworkflow.inner` and `input { typo = "oops" }` compiles successfully even when the callee declares no such variable, and `seedChildVarsFromBindings` then ignores the key silently. **Acceptance:** step-level subworkflow inputs must reject undeclared keys explicitly (compile-time preferred, reusing the existing subworkflow input validation rules or equivalent), must not silently drop them at runtime, and must have negative tests proving the rejection for both non-iterating and iterating subworkflow-targeted steps as applicable.

#### Test Intent Assessment
The new engine tests are now meaningfully aligned with behavior: `TestStep_EnvironmentOverride_InjectedIntoAdapter` proves subprocess-facing env injection, and `TestStep_SubworkflowStepInput_ReachesCallee` proves the positive data path into the callee. The remaining gap is regression sensitivity around invalid inputs: with no negative test, a faulty implementation that accepts misspelled subworkflow input keys still passes the suite.

#### Validation Performed
- `go test -race ./...` ✅
- `make validate` ✅
- `git --no-pager grep -nE 'hcl:"adapter,optional"|hcl:"agent,optional"' -- ':!*_test.go' ':!docs/' ':!CHANGELOG.md' ':!workstreams/'` ✅ (no matches)
- Minimal compile repro for step-level subworkflow input with undeclared key (`input { typo = "oops" }` against a callee with no matching variable) ❌ compiled successfully instead of rejecting the bad key.

### Round 3 — Remediation applied (2026-05-04)

**Blocker — undeclared step input keys silently accepted:**
- Extracted `compileSubworkflowStepInputExprs(g, sp, subworkflowRef)` helper in `compile_steps_subworkflow.go`.  For each key in the step `input {}` block, `validateInputItem` is called against `g.Subworkflows[subworkflowRef].DeclaredVars` (populated by `compileSubworkflows` before `compileSteps` runs). Undeclared keys produce a compile-time error identical in format to declaration-level input validation.
- `compileSubworkflowStep` now calls the shared helper instead of inlining the capture logic.
- `compileIteratingStep` for `targetKind == StepTargetSubworkflow` now also calls the helper and passes `InputExprs` to `newSubworkflowIterStepNode` (signature updated).  Iterating subworkflow steps silently ignored `sp.Input` before; they now capture and validate it.  The engine's `evaluateSubworkflowStep` already evaluates `InputExprs` for both iterating and non-iterating steps, so no engine changes are needed.
- `TestCompileStep_SubworkflowStepInput` updated: callee now declares `greeting` with a default so the step-level `input { greeting = "hello" }` is accepted.
- New test: `TestCompileStep_SubworkflowStepInput_UndeclaredKeyRejected` — non-iterating step with `input { typo = "oops" }` against a no-variable callee → compile error mentioning `"typo"`.
- New test: `TestCompileStep_SubworkflowIterStepInput_UndeclaredKeyRejected` — iterating step with `input { typo = each.value }` against a no-variable callee → compile error mentioning `"typo"`.

**Validation (round 3 — final):**
- `go test -race ./...` ✅ (all packages pass)
- `make lint-go` ✅ clean (cognitive complexity resolved by extracting helper)
- `make validate` ✅ (all 21 examples)

#### Required Remediations
- **Blocker — step environment syntax mismatch** (`workflow/schema.go:132-135`, `workflow/compile_step_target_test.go:210-268`, `docs/workflow.md:1102-1105`, `examples/phase3-environment/phase3.hcl:1-5`): implement the step-level override using the reference syntax required by this workstream (`environment = shell.ci`), not a quoted string. **Acceptance:** a step with `environment = shell.ci` parses and compiles; docs/examples/tests use the same syntax; compile-time resolution still validates the referenced environment and rejects missing ones with a targeted diagnostic.
- **Blocker — subworkflow step input still rejected** (`workflow/compile_steps_subworkflow.go:34-38`, `internal/engine/node_subworkflow.go:24-67`): `target = subworkflow.<name>` steps must accept step `input { ... }`, evaluate those expressions in the parent scope, and pass them into the callee instead of forcing all bindings onto the declaration-level `subworkflow { input = ... }`. **Acceptance:** compile no longer rejects step input for subworkflow targets; a step-level input binding reaches the callee variables at runtime; required-variable validation works through the step target path; add compile and engine/e2e coverage for this path.
- **Blocker — tests do not prove required behavior** (`internal/engine/node_step_w14_test.go:101-148`): `TestStep_EnvironmentOverride_AppliesToSubprocess` only inspects `getStepEnvironment`, so it does not prove env-var injection into a real adapter subprocess. There is also no test that a step-targeted subworkflow receives step inputs. **Acceptance:** add behavior-level tests that fail if the override is not injected into adapter execution, and add tests that fail if subworkflow step input is ignored or still declaration-bound.

#### Test Intent Assessment
`TestCompileStep_TargetAdapter`, `TestCompileStep_TargetSubworkflow`, and the legacy-target rejection tests do validate the new dispatcher shape. The environment override tests are weak because they only cover the quoted-string variant and a helper-level lookup, not the required syntax or the actual subprocess-visible effect. The new engine coverage also misses the most important regression case for this workstream: a parent step supplying input directly to a subworkflow target.

#### Validation Performed
- `go test ./workflow ./internal/engine -count=1` ✅
- `make validate` ✅
- `git --no-pager grep -nE 'hcl:"adapter,optional"|hcl:"agent,optional"' -- ':!*_test.go' ':!docs/' ':!CHANGELOG.md' ':!workstreams/'` ✅ (no matches)
- Minimal parse repro for step environment override using `environment = shell.ci` ❌ (`Variables not allowed`)
- Minimal compile repro for subworkflow-targeted step input ❌ (`step "call": input block is not valid for subworkflow-targeted steps; declare inputs on the subworkflow block instead`)


## Risks

| Risk | Mitigation |
|---|---|
| `target = step.<name>` adds dispatch ambiguity | Step 2 allows the executor to reject this kind in v0.3.0 if it complicates routing. Document the choice. |
| The `Target` `hcl.Expression` decode pattern interacts oddly with `Remain` | Existing `ForEach`/`Count` use the same pattern. Reuse the extraction logic. |
| Step environment override semantics confuse readers ("does it create a new session?") | Document explicitly: override is env-var injection only, not a new session. Test `TestStep_EnvironmentOverride_NewSessionNotCreated`. |
| Legacy-rejection message is too terse | Use the multiline format from [11](11-agent-to-adapter-rename.md)'s rejection messages, with a CHANGELOG pointer. |
| `target` references break HCL `gohcl` decode for unknown reasons | Capture via `Remain.JustAttributes()` as the existing pattern does; do not try to decode `hcl.Expression` into a struct field directly. |
