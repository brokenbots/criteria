# WS04 — Switch syntax: `condition { match = ... }` → `match { condition = ... }`

**Phase:** Language Cleanup · **Track:** Language · **Owner:** Workstream executor · **Depends on:** [WS02](WS02-data-and-outcome-semantics.md) (clean schema.go, compile_switches.go). · **Unblocks:** consistent switch ergonomics for adapter-v2 workflows. · **Base branch:** `main`

## Context

The switch block labels its inner cases `condition`, with `match` as an attribute inside:

```hcl
switch "check_env" {
  condition {
    match  = var.env == "prod"
    target = state.deploy
  }
  default {
    target = state.done
  }
}
```

This is the inverse of how switch/case reads in every mainstream language. The case arm is the _match_ (what you're looking for), and the predicate inside it is the _condition_ (the expression to evaluate). The current shape forces users to re-read the nesting each time.

The correct shape:

```hcl
switch "check_env" {
  match {
    condition = var.env == "prod"
    target    = state.deploy
  }
  default {
    target = state.done
  }
}
```

Migration strategy is **hard break with helpful errors**, same as WS01 and WS02. The old `condition { match = ... }` form is rejected at parse time with a clear migration message.

## Prerequisites

- WS02 merged (clean `compile_switches.go` with `resolveNextAttr`).

## In scope

### Step 1 — Schema rename

**File:** [workflow/schema.go](../../workflow/schema.go)

- Rename the `Conditions []ConditionSpec` field HCL tag from `"condition,block"` to `"match,block"`.
- Rename `ConditionSpec` to `MatchSpec` for clarity (internal rename, no surface effect beyond the block name).
- Inside `MatchSpec`, rename the `match` attribute to `condition`. The field name in Go and its HCL tag both change: `Condition hcl.Expression \`hcl:"condition"\``.
- `SwitchSpec` becomes:
  ```go
  type SwitchSpec struct {
      Name    string             `hcl:"name,label"`
      Matches []MatchSpec        `hcl:"match,block"`
      Default *SwitchDefaultSpec `hcl:"default,block"`
  }
  ```

### Step 2 — Compilation update

**File:** [workflow/compile_switches.go](../../workflow/compile_switches.go)

- Update `compileSwitches` to iterate `spec.Matches` (was `spec.Conditions`).
- Update `compileSwitchConditionBlock` (rename to `compileSwitchMatchBlock`) to read the `condition` attribute from the block body (was `match`).
- All other compilation logic is unchanged: `target` attribute, `output` optional attribute, `resolveNextAttr` wiring — these are name-stable.

### Step 3 — Legacy rejection

**File:** [workflow/parse_legacy_reject.go](../../workflow/parse_legacy_reject.go)

Add a rejection pass for the old `condition { match = ... }` form. The old block name `condition` must be detected early (before the new schema attempts to parse it as a `match` block) and rejected with a migration message:

```
switch "name": condition blocks have been renamed to match, and the match attribute inside them has been renamed to condition:

  match {
    condition = <expr>
    target    = <ref>
  }
```

### Step 4 — HCL migrations

Rename all uses across the repository:
- Block label: `condition {` → `match {` inside any `switch` block.
- Attribute: `match = <expr>` → `condition = <expr>` inside the renamed block.

Files to update (search for `switch` blocks containing `condition {`):
- `examples/` — all example `.hcl` files using switch statements.
- `.criteria/workflows/` — all workflow files using switch statements.
- Test fixtures in `workflow/testdata/` and inline test HCL strings.

Mechanical sed: `condition {` → `match {` and `match =` → `condition =` scoped to switch blocks. Spot-verify by compile.

### Step 5 — VSCode grammar update

Coordinated update to [criteria-vscode-extension-v1](../../../criteria-vscode-extension-v1):

- Replace the `condition` block matcher inside `switch` with a `match` block matcher.
- Update attribute highlighting: `condition = ` (expression) and `target = ` (reference) inside `match` block.
- Demote old `condition { match = ... }` form to a legacy-error highlight class so authors see the mismatch immediately.

_Grammar changes are out-of-tree; documented here for coordination._

### Step 6 — Tests

- Rejection test: `switch "x" { condition { match = true, target = state.done } }` emits a parse error containing the migration message.
- Positive test: `switch "x" { match { condition = true, target = state.done } }` compiles cleanly.
- Positive test: `switch "x" { match { condition = var.x == "foo", target = step.foo } default { target = state.done } }` compiles and the compiled switch conditions resolve correctly.
- All existing switch-related tests pass after fixture migration (step 4).

## Out of scope

- Changes to switch evaluation semantics — only the surface syntax changes.
- Adding new switch features (e.g. `match "label"` named arms, multi-condition `and`/`or`) — future work.

## Reuse pointers

- Existing [`compileSwitchConditionBlock`](../../workflow/compile_switches.go) — rename and update attribute read; all other logic is reused verbatim.
- Existing [`resolveNextAttr`](../../workflow/compile_switches.go#L211-L269) — unchanged.
- Existing [parse_legacy_reject.go](../../workflow/parse_legacy_reject.go) rejection pattern — follow the same `hclsyntax.WalkBody` approach used for `shared_variable` and `shared_writes`.

## Behavior change

**User-facing:** all switch blocks must use `match { condition = ... }` instead of `condition { match = ... }`. Old form is rejected at parse time.

**Runtime semantics:** unchanged. The compiled `SwitchCondition` struct and all downstream evaluation logic are unaffected by the block rename.

## Tests required

- All existing tests pass after fixture migration.
- New tests in Step 6 pass.
- `go vet ./...` clean.
- `grep -r 'condition {' examples/ .criteria/ workflow/` returns zero hits inside switch blocks.
- `make spec-check` passes.
- `make validate` passes on all examples.

## Implementation Notes

### Checklist

- [ ] Step 1 — Schema rename (`ConditionSpec` → `MatchSpec`, block tag `condition` → `match`, attribute `match` → `condition`)
- [ ] Step 2 — Compilation update (`compileSwitchConditionBlock` → `compileSwitchMatchBlock`)
- [ ] Step 3 — Legacy rejection for old `condition { match = ... }` form
- [ ] Step 4 — HCL migrations across `examples/`, `.criteria/workflows/`, test fixtures
- [ ] Step 5 — VSCode grammar update (out-of-tree, coordinate separately)
- [ ] Step 6 — Tests

### Reviewer Notes

_To be filled in during review._
