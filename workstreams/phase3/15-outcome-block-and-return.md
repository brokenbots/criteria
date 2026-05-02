# Workstream 15 — `outcome` block reshape + reserved `return` outcome

**Phase:** 3 · **Track:** C · **Owner:** Workstream executor · **Depends on:** [13-subworkflow-block-and-resolver.md](13-subworkflow-block-and-resolver.md), [14-universal-step-target.md](14-universal-step-target.md). · **Unblocks:** [16-switch-and-if-flow-control.md](16-switch-and-if-flow-control.md).

## Context

[proposed_hcl.hcl §4](../../proposed_hcl.hcl) reshapes the outcome block:

```hcl
step "review" {
    target = adapter.copilot.reviewer
    input = { ... }

    outcome "success" {
        next = step.next_thing
        output = { result = step.review.output }
    }

    outcome "needs_review" {
        next = "return"          // reserved: bubbles to caller
        output = { reason = step.review.output.reason }
    }

    default_outcome = "needs_review"
}
```

Three changes from v0.2.0 ([workflow/schema.go OutcomeSpec](../../workflow/schema.go#L158)):

1. **`transition_to` → `next`.** Hard rename; the legacy attribute is a parse error.
2. **`output` attribute on outcome.** Allows the outcome to project a custom output map back to the caller (or to the next step in the chain). Optional; defaults to the step's full output.
3. **Reserved `next = "return"`.** When a step's outcome routes to `"return"`, the engine halts the current scope (workflow body or subworkflow) and bubbles the outcome's `output` back to the caller. For top-level workflows, `return` is equivalent to a terminal state with the projected output as the run output set.

Plus a new step-level attribute:

4. **`default_outcome = "<name>"`.** When an adapter returns an outcome name not in the declared set, the engine maps it to the named default. Without `default_outcome`, an unknown outcome is a runtime error. (Phase 2's W14/W15 introduced `AllowedOutcomes` on the wire — Copilot now constrains its tool-call to the declared set; for adapters that still produce free-form outcomes, `default_outcome` is the safety net.)

## Prerequisites

- [13](13-subworkflow-block-and-resolver.md), [14](14-universal-step-target.md) merged.
- Familiarity with [archived/v2/15-copilot-submit-outcome-adapter.md](../archived/v2/15-copilot-submit-outcome-adapter.md) (the wire-side `submit_outcome` finalization).
- `make ci` green.

## In scope

### Step 1 — Schema reshape

```go
// BEFORE
type OutcomeSpec struct {
    Name         string `hcl:"name,label"`
    TransitionTo string `hcl:"transition_to"`
}

// AFTER
type OutcomeSpec struct {
    Name   string   `hcl:"name,label"`
    Next   string   `hcl:"next"`              // step name | state name | "return"
    Remain hcl.Body `hcl:",remain"`           // captures the optional "output" expression
}
```

Add `default_outcome` to `StepSpec`:

```go
StepSpec.DefaultOutcome string `hcl:"default_outcome,optional"`
```

`StepNode.Outcomes` evolves:

```go
// BEFORE
Outcomes map[string]string  // outcome name -> target

// AFTER
type CompiledOutcome struct {
    Name        string
    Next        string                  // resolved target node name OR "return" sentinel
    OutputExpr  hcl.Expression          // nil = pass-through (use step's full output)
}
Outcomes        map[string]*CompiledOutcome
DefaultOutcome  string                  // "" if not declared
```

### Step 2 — Reserved `return` semantics

`next = "return"` is a sentinel string. The compiler:

1. Recognizes `"return"` and stores it as-is in `CompiledOutcome.Next`.
2. Does NOT try to resolve it to a step/state name.
3. Validates that the outcome's `output` expression (if present) folds against parent eval context (it can reference `var.*`, `local.*`, `each.*`, `steps.*`, `subworkflow.*`).

The engine, when handling an outcome:

```go
if outcome.Next == "return" {
    // Evaluate outcome.OutputExpr (if non-nil) against current run state.
    // For a subworkflow scope: project the result as the subworkflow's
    // output bundle and signal the parent step.
    // For a top-level workflow: the result becomes the run's output set
    // (overrides any declared top-level output blocks for this exit).
    return outcomeReturnResult{Outputs: ..., Status: success}
}
```

If `next = "return"` appears in an outcome AND the workflow has top-level `output` declarations ([09](09-output-block.md)), there is a tension: which outputs are surfaced?

**Decision (proposed_hcl.hcl):** The `outcome.output` projection wins. If the outcome routes to `"return"` with an `output = { ... }` map, that map IS the run's outputs (or the subworkflow's outputs back to the caller). The top-level `output` blocks are for the **default** terminal-state path; an explicit `return` outcome is the override.

Document this clearly in [docs/workflow.md](../../docs/workflow.md). Add a test asserting the precedence.

### Step 3 — `default_outcome` semantics

When an adapter step finalizes with an outcome name not in the step's declared outcome set:

1. If `default_outcome` is set on the step, the unknown name is **silently mapped** to the default. Emit a `step.outcome.defaulted` event with both the original and mapped names.
2. If `default_outcome` is not set, the unknown outcome is a step-level error. Status: `failure`. Emit `step.outcome.unknown` event.

Compile-time check: `default_outcome = "<name>"` MUST refer to one of the declared outcomes. Otherwise compile error.

Note interaction with Phase 2's W14 `AllowedOutcomes`: when an adapter respects the wire constraint, the step never sees an unknown outcome — `default_outcome` is the safety net for adapters that don't, plus a friendly fallback for outcomes the workflow author hasn't enumerated yet. Document in reviewer notes.

### Step 4 — Migration: `transition_to` → `next`

Hard parse error for `transition_to` on outcome blocks (and on `branch.arm` blocks until [16](16-switch-and-if-flow-control.md) deletes those). The error message:

```
attribute "transition_to" was renamed to "next" in v0.3.0.
For terminal-state outcomes that bubble to the caller, use next = "return".
See CHANGELOG.md migration note.
```

Add to `parse_legacy_reject.go` (cumulative with [11](11-agent-to-adapter-rename.md), [12](12-adapter-lifecycle-automation.md), [14](14-universal-step-target.md)).

### Step 5 — Engine routing

In [internal/engine/node_step.go](../../internal/engine/node_step.go), the outcome-routing logic changes:

```go
// resolveOutcomeTransition determines the next node based on the adapter's
// declared outcome name.
func (n *StepNode) resolveOutcomeTransition(name string, st *RunState) (next string, outputProjection map[string]cty.Value, isReturn bool, err error)
```

The engine then:

- If `isReturn`, halts the current scope and propagates `outputProjection` upward.
- Otherwise, transitions to `next` and stores `outputProjection` (if non-nil) as the step's effective output. If `outputProjection` is nil, the step's full adapter output is used (current behavior).

For subworkflow scopes: the `runSubworkflow` ([13](13-subworkflow-block-and-resolver.md)) entry observes the return-bubble signal and returns the projected output to the parent step.

For top-level workflows: the engine treats return as terminal-success, with the projected output overriding `g.Outputs` evaluation (the projection IS the run output set).

### Step 6 — Tests

- Compile:
  - `TestCompileOutcome_NextIsStep`.
  - `TestCompileOutcome_NextIsState`.
  - `TestCompileOutcome_NextIsReturn`.
  - `TestCompileOutcome_OutputExprFolds`.
  - `TestCompileOutcome_OutputExprRuntimeRef`.
  - `TestCompileOutcome_LegacyTransitionTo_HardError`.
  - `TestCompileStep_DefaultOutcomeMissing` — `default_outcome = "x"` but no `outcome "x"` declared → error.

- Engine:
  - `TestStep_OutcomeReturn_BubblesToParent`.
  - `TestStep_OutcomeReturn_TopLevelTerminal`.
  - `TestStep_OutcomeReturnOutputOverridesOutputBlocks`.
  - `TestStep_DefaultOutcome_AppliedOnUnknownName`.
  - `TestStep_DefaultOutcomeUnset_UnknownNameErrors`.
  - `TestStep_OutcomeOutputProjection_PassedToNextStep`.

- End-to-end: a workflow with subworkflow + return outcome.

### Step 7 — Validation

```sh
go build ./...
go test -race -count=2 ./...
make validate
make test-conformance
make ci
git grep -nE 'hcl:"transition_to"' -- ':!*_test.go' ':!docs/' ':!CHANGELOG.md' ':!workstreams/'
```

Final grep MUST return zero in production code.

## Behavior change

**Behavior change: yes — breaking.**

Observable differences:

1. `outcome "x" { transition_to = ... }` → hard parse error.
2. `outcome "x" { next = ... }` is the new shape.
3. Reserved `next = "return"` halts the current scope.
4. Optional `outcome.output = { ... }` projects custom output.
5. Step-level `default_outcome = "<name>"` for unknown-outcome safety net.
6. New events: `step.outcome.defaulted`, `step.outcome.unknown`.

## Reuse

- Existing outcome storage on `StepNode.Outcomes` — extend, not rewrite.
- `FoldExpr` from [07](07-local-block-and-fold-pass.md).
- The legacy-rejection helper from [11](11-agent-to-adapter-rename.md).
- The subworkflow scope-exit propagation path from [13](13-subworkflow-block-and-resolver.md).

## Out of scope

- `branch` block conversion to `switch`/`if`. Owned by [16-switch-and-if-flow-control.md](16-switch-and-if-flow-control.md) — that workstream also rejects legacy `branch.arm.transition_to`.
- Free-form outcome name validation. The adapter declares its outcome domain via Phase 2's W14 `AllowedOutcomes`; this workstream consumes that input but does not change the wire contract.
- Streaming partial outcome projections. Outcome routing is single-shot at step finalization.

## Files this workstream may modify

- [`workflow/schema.go`](../../workflow/schema.go) — reshape `OutcomeSpec`, add `StepSpec.DefaultOutcome`, define `CompiledOutcome` and reshape `StepNode.Outcomes`/`DefaultOutcome`.
- `workflow/compile_steps_*.go` — outcome compile.
- [`internal/engine/node_step.go`](../../internal/engine/node_step.go) — outcome routing.
- [`internal/engine/node_subworkflow.go`](../../internal/engine/node_subworkflow.go) — observe return-bubble signal.
- [`internal/engine/engine.go`](../../internal/engine/engine.go) — top-level return-as-terminal handling.
- [`events/`](../../events/) — new `step.outcome.defaulted` / `step.outcome.unknown` event types.
- `workflow/parse_legacy_reject.go` — extend.
- All example HCL files using outcome blocks.
- Goldens.
- [`docs/workflow.md`](../../docs/workflow.md).
- New tests.

This workstream may **not** edit:

- `PLAN.md`, `README.md`, `AGENTS.md`, `CHANGELOG.md`, `workstreams/README.md`, or any other workstream file.
- `.proto` files (the wire `AllowedOutcomes` from Phase 2 W14 is unchanged).
- [`.golangci.baseline.yml`](../../.golangci.baseline.yml).

## Tasks

- [ ] Reshape `OutcomeSpec` and `StepSpec.DefaultOutcome`; reshape compiled types (Step 1).
- [ ] Reserved `return` compile and runtime semantics (Step 2).
- [ ] `default_outcome` compile validation and runtime mapping (Step 3).
- [ ] Legacy parse rejection (Step 4).
- [ ] Engine routing (Step 5).
- [ ] All required tests (Step 6).
- [ ] `make ci` green; final grep zero (Step 7).

## Exit criteria

- `outcome "x" { next = ... }` parses; `transition_to` errors.
- `next = "return"` works at both subworkflow and top-level.
- `outcome.output = ...` projection overrides default output flow.
- `default_outcome` compile-validates and runtime-applies.
- New events emit on defaulted/unknown outcomes.
- All required tests pass.
- All examples updated; `make validate` green.
- `make ci` exits 0.

## Tests

Step 6 list. Coverage: outcome routing path ≥ 90%.

## Risks

| Risk | Mitigation |
|---|---|
| `return` outcome and top-level `output` block precedence is confusing | Document explicitly; test the precedence rule. The override semantics matches what HCL authors expect from a `return` keyword. |
| `default_outcome` masks real adapter bugs | Emit a clear event on default mapping; the operator who is auditing for adapter conformance can subscribe. |
| `outcome.output` expression references a step output that didn't run | Same error as in [09](09-output-block.md): "outcome X output references step Y which did not execute". Reuse the error helper. |
| Migration burden: every example with outcome blocks rewrites | Mechanical — substitute `transition_to` → `next`. Sweep all examples; regenerate goldens. |
| The reserved-name approach for `"return"` collides with a user step named `return` | Steps cannot be named `"return"` — add a name validation that rejects this. Test `TestCompileStep_NameReturn_HardError`. |
