# Workstream 16 — `switch` and `if` flow-control blocks (replace `branch`)

**Phase:** 3 · **Track:** C · **Owner:** Workstream executor · **Depends on:** [07-local-block-and-fold-pass.md](07-local-block-and-fold-pass.md), [15-outcome-block-and-return.md](15-outcome-block-and-return.md). · **Unblocks:** [21-phase3-cleanup-gate.md](21-phase3-cleanup-gate.md) (the legacy-removal grep gate requires zero `BranchSpec` references).

## Context

[proposed_hcl.hcl](../../proposed_hcl.hcl) replaces `branch` with `switch` (and optionally `if`):

```hcl
switch "review_dispatch" {
    condition {
        match = step.review.output.severity == "critical"
        output = { route = "escalate" }
        next = step.escalate
    }
    condition {
        match = step.review.output.severity == "minor"
        next = step.auto_approve
    }
    default {
        next = step.manual_review
    }
}
```

Versus the legacy [`BranchSpec`](../../workflow/schema.go#L191):

```hcl
branch "review_dispatch" {
    arm {
        when = step.review.output.severity == "critical"
        transition_to = "escalate"
    }
    default { transition_to = "manual_review" }
}
```

Three structural differences:

1. **Block names.** `branch` → `switch`. Both rejected at parse if seen as `branch` (legacy).
2. **Inner block.** `arm { when, transition_to }` → `condition { match, output, next }`. The `output` attribute is new and lets the switch project a custom output map (mirroring the pattern from [15-outcome-block-and-return.md](15-outcome-block-and-return.md)).
3. **`default` shape.** v0.2.0: `default { transition_to = ... }`. v0.3.0: `default { next = ... }`. (`output` is allowed on `default` too.)

Plus: the open question on `if` (per the plan file's open questions section). **Decision in this workstream:**

> Ship `switch` only. `if "<name>" { match = ..., next = ..., else_next = ... }` would be syntactic sugar for a two-condition switch; for v0.3.0 the marginal complexity is not worth the new surface. A future phase can add `if` if real workflows demand it. Document this decision in [docs/workflow.md](../../docs/workflow.md) so HCL authors know to use `switch`.

## Prerequisites

- [07](07-local-block-and-fold-pass.md): `FoldExpr` for compile-fold of condition expressions.
- [15](15-outcome-block-and-return.md): outcome `next`/`output` shape — `switch` mirrors it.
- `make ci` green.

## In scope

### Step 1 — Schema

Add `SwitchSpec`, `ConditionSpec`, `SwitchDefaultSpec`:

```go
type SwitchSpec struct {
    Name       string             `hcl:"name,label"`
    Conditions []ConditionSpec    `hcl:"condition,block"`
    Default    *SwitchDefaultSpec `hcl:"default,block"`
}

type ConditionSpec struct {
    Remain hcl.Body `hcl:",remain"`  // captures: match (required), next (required), output (optional)
}

type SwitchDefaultSpec struct {
    Remain hcl.Body `hcl:",remain"`  // captures: next (required), output (optional)
}
```

In `Spec`, replace `Branches []BranchSpec` with `Switches []SwitchSpec` (HCL tag `\`hcl:"switch,block"\``).

In `FSMGraph`:

```go
// BEFORE
Branches map[string]*BranchNode

// AFTER
Switches map[string]*SwitchNode

type SwitchNode struct {
    Name          string
    Conditions    []SwitchCondition
    DefaultNext   string
    DefaultOutput hcl.Expression  // nil if not declared
}

type SwitchCondition struct {
    Match       hcl.Expression  // boolean condition; runtime-evaluated
    MatchSrc    string          // source text for diagnostics (mirrors BranchArm.ConditionSrc)
    Next        string          // resolved target node name OR "return"
    OutputExpr  hcl.Expression  // nil if not declared
}
```

Delete `BranchSpec`, `ArmSpec`, `DefaultArmSpec`, `BranchNode`, `BranchArm`. The struct removals come at compile-error time for any caller; sweep them in this workstream.

### Step 2 — Compile pass

New file `workflow/compile_switches.go`:

```go
func compileSwitches(g *FSMGraph, spec *Spec, opts CompileOpts) hcl.Diagnostics
```

Algorithm:

1. For each `SwitchSpec`, validate the name is a valid identifier and unique across all node kinds in the graph.
2. For each `ConditionSpec.Remain.JustAttributes()`:
   - `match` is required; capture as `hcl.Expression`. Validate via `validateFoldableAttrs` ([07](07-local-block-and-fold-pass.md)) — it can reference any namespace (var, local, each, steps, subworkflow); not required to fold.
   - `next` is required; resolve to a step/state/switch name OR the reserved `"return"` sentinel.
   - `output` is optional; capture as `hcl.Expression`.
   - Any other attribute is a compile error.
3. The `Default` block similarly: `next` required, `output` optional.
4. The default block is required if at least one condition does not provably match all inputs (i.e. always required in practice; warn at compile if absent and the conditions don't cover constant `true`).

Replace the existing branch compile flow (`workflow/compile_branch.go` or wherever it lives — find via `git grep BranchSpec`). The pattern matches: condition is to switch as arm.when is to branch.

### Step 3 — Runtime

Replace [internal/engine/node_branch.go](../../internal/engine/node_branch.go) (or equivalent) with `node_switch.go`:

```go
func (n *SwitchNode) Evaluate(ctx context.Context, st *RunState, deps Deps) (string, error) {
    evalCtx := workflow.BuildEvalContextWithOpts(st.Vars, st.Locals, ...)
    for _, cond := range n.Conditions {
        val, diags := cond.Match.Value(evalCtx)
        if diags.HasErrors() {
            return "", asError(diags)
        }
        if val.True() {
            applyOutputProjection(st, cond.OutputExpr, evalCtx)
            return cond.Next, nil
        }
    }
    applyOutputProjection(st, n.DefaultOutput, evalCtx)
    return n.DefaultNext, nil
}
```

Reuse the `next = "return"` handling from [15](15-outcome-block-and-return.md) — switch nodes can also bubble.

### Step 4 — Migration: hard rejection of legacy `branch`

Add to `parse_legacy_reject.go`:

```
block "branch" was renamed to "switch" in v0.3.0. The arm shape changed
from arm { when, transition_to } to condition { match, next, output }.
The default block uses next instead of transition_to. See CHANGELOG.md
migration note.
```

Migration text for [21](21-phase3-cleanup-gate.md):

```
### `branch` → `switch`

v0.2.0:
    branch "dispatch" {
        arm {
            when = var.severity == "critical"
            transition_to = "escalate"
        }
        default { transition_to = "manual" }
    }

v0.3.0:
    switch "dispatch" {
        condition {
            match = var.severity == "critical"
            next = step.escalate
        }
        default { next = step.manual }
    }

`output` attribute on conditions and default is new and optional; it projects
a custom output map for the routed step.
```

### Step 5 — Tests

- Compile:
  - `TestCompileSwitch_BasicCondition`.
  - `TestCompileSwitch_MultipleConditions`.
  - `TestCompileSwitch_DefaultRequired_Warning` — when conditions are not provably exhaustive.
  - `TestCompileSwitch_NextResolvesToStep`.
  - `TestCompileSwitch_NextIsReturn`.
  - `TestCompileSwitch_OutputExprFolds`.
  - `TestCompileSwitch_LegacyBranchBlock_HardError`.
  - `TestCompileSwitch_LegacyTransitionToOnArm_HardError`.

- Engine:
  - `TestSwitch_FirstMatchWins`.
  - `TestSwitch_NoMatchFallsToDefault`.
  - `TestSwitch_OutputProjection_AppliedBeforeNext`.
  - `TestSwitch_ReturnFromCondition_BubblesToCaller`.

- End-to-end: example with switch + return.

### Step 6 — Validation

```sh
go build ./...
go test -race -count=2 ./...
make validate
make test-conformance
make ci
git grep -nE '\bBranchSpec\b|\bBranchNode\b|\bArmSpec\b|"branch,block"' -- ':!*_test.go' ':!docs/' ':!CHANGELOG.md' ':!workstreams/'
```

Final grep MUST return zero in production code.

## Behavior change

**Behavior change: yes — breaking.**

Observable differences:

1. `branch "x" { arm { when, transition_to }, default { transition_to } }` is a hard parse error.
2. `switch "x" { condition { match, next, output }, default { next, output } }` is the new shape.
3. New `output` projection on conditions and default.
4. `next = "return"` works inside switch conditions (mirrors [15](15-outcome-block-and-return.md)).

## Reuse

- `BranchSpec`-shape compile pattern — port to `SwitchSpec`.
- `ArmSpec.Remain` extraction logic — port to `ConditionSpec.Remain`.
- The legacy-rejection helper.
- The next-resolution helper (resolves `step.<name>` traversals to graph node names).
- `FoldExpr` for the `match` and `output` attribute validation.

## Out of scope

- `if` block. Decision in Context: not in v0.3.0.
- Inline switch expressions inside step `outcome` blocks. Step-level conditional routing belongs in `outcome` ([15](15-outcome-block-and-return.md)) using adapter outcome names; cross-step conditional routing belongs in a top-level `switch`.
- New comparison/string-manipulation HCL functions specific to switch. Conditions use the existing function set.
- Switch-level `parallel` modifier. Out of scope.

## Files this workstream may modify

- [`workflow/schema.go`](../../workflow/schema.go) — replace branch types with switch types.
- New: `workflow/compile_switches.go`. Delete `workflow/compile_branch.go` (or whatever the legacy file was; find and delete).
- New: `internal/engine/node_switch.go`. Delete `internal/engine/node_branch.go`.
- `workflow/parse_legacy_reject.go` — extend with `branch` block rejection.
- All example HCL files using `branch`.
- Goldens.
- [`docs/workflow.md`](../../docs/workflow.md) — switch section + the no-`if`-in-v0.3.0 note.
- New tests.
- The top-level compile entry — invoke `compileSwitches` instead of `compileBranches`.
- Engine dispatcher — route `switch` nodes via `SwitchNode.Evaluate`.

This workstream may **not** edit:

- `PLAN.md`, `README.md`, `AGENTS.md`, `CHANGELOG.md`, `workstreams/README.md`, or any other workstream file.
- `.proto` files.
- Outcome-block code paths owned by [15](15-outcome-block-and-return.md).

## Tasks

- [ ] Reshape schema (Step 1).
- [ ] Compile pass (Step 2).
- [ ] Engine evaluator (Step 3).
- [ ] Legacy parse rejection (Step 4).
- [ ] Author tests (Step 5).
- [ ] `make ci` green; final grep zero (Step 6).

## Exit criteria

- `git grep -E 'BranchSpec|BranchNode|ArmSpec'` returns zero in production code.
- `branch "..."` HCL produces a hard parse error with migration message.
- `switch "..."` parses, compiles, and routes correctly.
- `next = "return"` works in switch conditions.
- All examples updated; `make validate` green.
- All required tests pass.
- `make ci` exits 0.

## Tests

The Step 5 list. Coverage: switch compile + engine ≥ 90%.

## Risks

| Risk | Mitigation |
|---|---|
| `output` attribute on switch conditions vs. on outcome blocks confuses HCL authors | The semantics are identical: project a custom output for the routed step. Document the parallel in [docs/workflow.md](../../docs/workflow.md). |
| Conditions are evaluated in declaration order; an order-sensitive workflow that worked under `branch` (also first-match-wins) might not behave identically | First-match-wins is preserved. Confirm with `TestSwitch_FirstMatchWins` covering the same cases as the legacy `TestBranch_FirstMatchWins`. |
| Removing `BranchSpec` breaks downstream consumers that used the SDK to parse a workflow | The SDK doesn't expose `BranchSpec` directly; the parsed graph is the public surface. Confirm via `git grep` in [sdk/](../../sdk/). |
| The `if` decision in v0.3.0 frustrates authors who want a simple boolean dispatch | A two-condition switch is one more line: `condition { match = ..., next = step.A }; default { next = step.B }`. Document the pattern. |
| `next = step.<name>` traversal resolution diverges from `transition_to = "<name>"` string-name resolution | The new `next` accepts a traversal (`step.foo` or `state.terminal`) for type-safety. Bare strings (`next = "foo"`) also accepted as a fallback for state names. Document both forms; test both. |
