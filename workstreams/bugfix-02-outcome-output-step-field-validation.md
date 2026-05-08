# Bugfix Workstream BF-02 — Validate `step.output.<field>` refs in outcome projections against `OutputSchema`

**Owner:** Workstream executor · **Depends on:** none · **Coordinates with:** BF-01 (independent).

## Context

When a step outcome declares an output projection:

```hcl
step "run" {
  target = adapter.shell.default
  outcome "success" {
    next   = "done"
    output = { code = step.output.exit_code }
  }
}
```

the `step.*` namespace is runtime-only, so `validateOutcomeOutputExpr`
([workflow/compile_steps_graph.go:80](../workflow/compile_steps_graph.go#L80)) silently defers
the entire expression. However, the step's `adapterOutputSchema` (`AdapterInfo.OutputSchema`)
**is** available at compile time and declares the exact fields the adapter promises to return.

If `exit_code` is not in `OutputSchema`, the run fails at runtime when the HCL expression
attempts `val.GetAttr("exit_code")` on an object that has no such attribute — often far removed
from the authoring mistake. The compiler has all the information it needs to catch this at
`criteria plan` time instead.

The fix mirrors the existing `validateSwitchExprRefs` pattern
([workflow/compile_switches.go:275](../workflow/compile_switches.go#L275)): walk
`expr.Variables()`, identify `step.output.<field>` traversals, and check each field name
against the schema.

Adjacent gap (out of scope for this workstream): `steps.<step_name>.<field>` cross-step field
validation in switch conditions and step inputs. That requires a post-compilation pass and is
independent.

## Prerequisites

- `make test` green on `main`.
- Familiarity with:
  - [workflow/compile_steps_graph.go](../workflow/compile_steps_graph.go) — `compileOutcomeRemain`,
    `validateOutcomeOutputExpr`.
  - [workflow/compile_switches.go:275](../workflow/compile_switches.go#L275) — `validateSwitchExprRefs`
    (the reference traversal-walking pattern).
  - [workflow/schema.go:272](../workflow/schema.go#L272) — `AdapterInfo`, `ConfigField`,
    `InputSchema`, `OutputSchema`.
  - `hcl.TraverseRoot`, `hcl.TraverseAttr` from `github.com/hashicorp/hcl/v2`.

## In scope

### Step 1 — Add `validateOutputExprStepOutputRefs`

Add a new unexported function to
[workflow/compile_steps_graph.go](../workflow/compile_steps_graph.go), immediately after
`validateOutcomeOutputExpr`:

```go
// validateOutputExprStepOutputRefs checks that every step.output.<field>
// traversal in expr references a field that exists in adapterOutputSchema.
// When schema is empty (nil or zero-length), no check is performed — the
// adapter has no declared output contract and all field references are valid.
// Traversals that do not match the step.output.<field> shape are ignored.
func validateOutputExprStepOutputRefs(stepName, outcomeName string, expr hcl.Expression, schema map[string]ConfigField) hcl.Diagnostics {
    if len(schema) == 0 {
        return nil
    }
    var diags hcl.Diagnostics
    for _, traversal := range expr.Variables() {
        // Require at least step.output.<field> — three segments minimum.
        if len(traversal) < 3 {
            continue
        }
        root, rootOK := traversal[0].(hcl.TraverseRoot)
        mid, midOK := traversal[1].(hcl.TraverseAttr)
        field, fieldOK := traversal[2].(hcl.TraverseAttr)
        if !rootOK || !midOK || !fieldOK {
            continue
        }
        if root.Name != "step" || mid.Name != "output" {
            continue
        }
        if _, known := schema[field.Name]; !known {
            r := field.SrcRange
            diags = append(diags, &hcl.Diagnostic{
                Severity: hcl.DiagError,
                Summary:  fmt.Sprintf("step %q outcome %q: output field %q is not declared in the adapter's output schema", stepName, outcomeName, field.Name),
                Subject:  &r,
            })
        }
    }
    return diags
}
```

### Step 2 — Wire into `compileOutcomeRemain`

Edit the `output` attribute handling block inside `compileOutcomeRemain`
([workflow/compile_steps_graph.go:148](../workflow/compile_steps_graph.go#L148)) to call the
new function after `validateOutcomeOutputExpr`, guarded by `!isAggregateIter` (aggregate
outcomes fire after all iterations complete and have no `step.output.*` binding):

```go
if attr, ok := content.Attributes["output"]; ok {
    compiled.OutputExpr = attr.Expr
    diags = append(diags, validateOutcomeOutputExpr(stepName, outcomeName, attr, g, opts)...)
    if !isAggregateIter {
        diags = append(diags, validateOutputExprStepOutputRefs(stepName, outcomeName, attr.Expr, adapterOutputSchema)...)
    }
    knownOutputKeys = staticObjectExprKeys(attr.Expr)
}
```

### Step 3 — Tests

Add to [workflow/compile_outcomes_test.go](../workflow/compile_outcomes_test.go).

The test helper `testSchemas` already exists in
[workflow/compile_input_test.go](../workflow/compile_input_test.go) — use it as a reference for
how `AdapterInfo` with an `OutputSchema` is passed to `Compile`. Wire it the same way: pass a
`map[string]AdapterInfo{"noop.default": {OutputSchema: map[string]ConfigField{...}}}` as the
schemas argument to `Compile`.

Three tests:

1. **`TestCompileOutcome_StepOutputRef_KnownField`** — adapter declares `OutputSchema` with
   field `"result"`; outcome has `output = { x = step.output.result }`. Must compile without
   error.

2. **`TestCompileOutcome_StepOutputRef_UnknownField`** — same adapter schema; outcome has
   `output = { x = step.output.ghost }`. Must produce a compile error whose message contains
   `"ghost"`.

3. **`TestCompileOutcome_StepOutputRef_NoSchema`** — pass `nil` schemas to `Compile`; outcome
   has `output = { x = step.output.ghost }`. Must compile without error (permissive when no
   schema).

Existing test `TestCompileOutcome_OutputExprRuntimeRef` uses `steps.a.exit_code` (the
cross-step namespace, not `step.output.*`). It must continue to pass unchanged — the new
validation only fires on the `step.output.*` shape.

## Behavior change

**Yes — new compile errors when `OutputSchema` is provided.**

- Outcome `output = { ... }` expressions that reference `step.output.<field>` where `<field>`
  is absent from the adapter's `OutputSchema` now produce a `DiagError` at compile time instead
  of failing at runtime.
- When no `OutputSchema` is provided (nil or empty map), behavior is unchanged — permissive.
- `steps.<other>.<field>` references (cross-step namespace) are unaffected.
- `var.*`, `local.*`, `each.*`, `shared.*` references are unaffected.
- No change to the wire contract or event types.

## Reuse

- `expr.Variables()` traversal pattern from `validateSwitchExprRefs`
  ([workflow/compile_switches.go:275](../workflow/compile_switches.go#L275)) — follow it
  exactly.
- `hcl.TraverseRoot`, `hcl.TraverseAttr` — same types used in
  [workflow/compile_locals.go:100](../workflow/compile_locals.go#L100) and
  [workflow/compile_step_target.go:142](../workflow/compile_step_target.go#L142).
- `adapterOutputSchema` is already threaded through `compileOutcomeBlock` →
  `compileOutcomeRemain`; no new parameters needed.

## Out of scope

- `steps.<step_name>.<field>` cross-step field validation (requires a post-compilation pass;
  separate workstream).
- Validation of `step.output.*` in switch condition `match` expressions (different code path;
  separate workstream if needed).
- Validation of `step.output.*` in step input `input { }` expressions (those use the
  `each.*`/`steps.*` namespace at runtime, not `step.output.*`).
- Any change to the wire contract, event types, or `Sink` interface.
- Any change to `AdapterInfo`, `OutputSchema`, or how schemas are passed to `Compile`.

## Files this workstream may modify

- `workflow/compile_steps_graph.go` — add `validateOutputExprStepOutputRefs`; call it from
  `compileOutcomeRemain`.
- `workflow/compile_outcomes_test.go` — add 3 tests.

This workstream may **not** edit `README.md`, `PLAN.md`, `AGENTS.md`, `CHANGELOG.md`,
`CONTRIBUTING.md`, `workstreams/README.md`, or any other workstream file.

## Tasks

- [ ] Add `validateOutputExprStepOutputRefs` to `workflow/compile_steps_graph.go`.
- [ ] Call it from `compileOutcomeRemain` (guarded by `!isAggregateIter`).
- [ ] Add `TestCompileOutcome_StepOutputRef_KnownField` to `workflow/compile_outcomes_test.go`.
- [ ] Add `TestCompileOutcome_StepOutputRef_UnknownField`.
- [ ] Add `TestCompileOutcome_StepOutputRef_NoSchema`.
- [ ] `go test ./workflow/ -run TestCompileOutcome` passes.
- [ ] `make test` clean.

## Exit criteria

- `output = { x = step.output.declared_field }` with a schema that includes `declared_field`
  compiles without errors.
- `output = { x = step.output.undeclared_field }` with a schema that does NOT include
  `undeclared_field` produces a compile error containing the field name.
- `output = { x = step.output.anything }` with no schema (nil) compiles without errors.
- Existing `TestCompileOutcome_OutputExprRuntimeRef` (uses `steps.a.exit_code`) continues to
  pass.
- `make test` clean.
