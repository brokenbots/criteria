# Bugfix Workstream BF-03 — Validate `steps.<name>.<field>` cross-step output field refs at compile time

**Owner:** Workstream executor · **Depends on:** none · **Coordinates with:** BF-01, BF-02 (all independent).

## Context

Three expression sites in a workflow can reference the output of a previously-run step via the
`steps.<stepName>.<field>` namespace:

1. **Switch condition `match` expressions** — e.g. `match = steps.build.exit_code == "0"`
2. **Step `input { }` block expressions** — e.g. `command = "echo ${steps.build.stdout}"`
3. **Outcome `output = { ... }` projections** — e.g. `output = { result = steps.build.stdout }`
   (the cross-step `steps.*` form, distinct from the same-step `step.output.*` form addressed in BF-02)

`validateSwitchExprRefs` ([workflow/compile_switches.go:275](../workflow/compile_switches.go#L275))
already validates that `steps.<name>` refers to a declared step, but it stops at the second
traversal segment. The third segment — the output field name — is silently ignored. The other two
sites do not check step names at all.

If a workflow authors `steps.build.stddout` (typo), or `steps.build.nonexistent`, nothing catches
it until the run evaluates the expression at runtime and HCL raises an "unsupported attribute"
panic/error. The compiler has all the necessary information post-compilation:

- `g.Steps` is fully populated with every `StepNode`, including its `AdapterRef`.
- `schemas[step.AdapterRef].OutputSchema` declares the fields the adapter promises to emit.

The fix is a post-compilation validation pass added at the end of `CompileWithOpts`
([workflow/compile.go](../workflow/compile.go)) that walks every relevant expression in the
compiled graph and checks `steps.<name>.<field>` traversals against the resolved `OutputSchema`.

### Why a post-compilation pass (not inline)

Steps are compiled in declaration order. When step B's input expression references `steps.A.x`,
step A may not yet be compiled into `g.Steps` at the point B is being compiled. Running the
check inline would require two-pass compilation or forward-declaration tracking. The post-pass
approach is simpler: all steps are registered before the check begins, matching the existing
precedent of `resolveTransitions` and `warnBackEdges`.

### Severity: warning, not error

Unlike unknown *step names* (which are errors), unknown *field names* carry more uncertainty:
- An adapter with no `OutputSchema` has no declared contract — field refs must be permissive.
- Some adapters emit dynamic output fields not listed in their schema.
- The pattern is new; a warning is the appropriate introduction before promoting to error.

This mirrors the `warnBackEdges` precedent (a `DiagWarning`, not `DiagError`).

## Prerequisites

- `make test` green on `main`.
- Familiarity with:
  - [workflow/compile.go](../workflow/compile.go) — `CompileWithOpts`, compilation order,
    location of `warnBackEdges` call (the reference point for where the new pass is added).
  - [workflow/compile_switches.go:275](../workflow/compile_switches.go#L275) — `validateSwitchExprRefs`
    (the reference traversal-walking pattern; the new pass extends it).
  - [workflow/schema.go:272](../workflow/schema.go#L272) — `AdapterInfo`, `ConfigField`,
    `OutputSchema`; [workflow/schema.go:455](../workflow/schema.go#L455) — `StepNode`,
    `InputExprs`, `AdapterRef`.
  - [workflow/schema.go:548](../workflow/schema.go#L548) — `SwitchNode`, `SwitchCondition.Match`.
  - [workflow/schema.go:423](../workflow/schema.go#L423) — `CompiledOutcome.OutputExpr`.
  - `hcl.TraverseRoot`, `hcl.TraverseAttr` from `github.com/hashicorp/hcl/v2`.

## In scope

### Step 1 — Add `warnCrossStepFieldRefs` pass in `workflow/compile_steps_graph.go`

Add a new function alongside `warnBackEdges` in
[workflow/compile_steps_graph.go](../workflow/compile_steps_graph.go):

```go
// warnCrossStepFieldRefs walks every compiled expression that may contain
// steps.<name>.<field> traversals and emits DiagWarning when <field> is absent
// from the referenced step's declared OutputSchema. Only fires when a schema is
// available; steps with no OutputSchema are skipped (permissive).
//
// Expression sites checked:
//   - StepNode.InputExprs (step input block attribute expressions)
//   - CompiledOutcome.OutputExpr (outcome output projections, cross-step form)
//   - SwitchCondition.Match (switch condition match expressions)
//
// This is a post-compilation pass: all steps must be registered in g.Steps
// before it runs so forward-references resolve correctly.
func warnCrossStepFieldRefs(g *FSMGraph, schemas map[string]AdapterInfo) hcl.Diagnostics {
    var diags hcl.Diagnostics

    // Collect all expressions to check.
    type namedExpr struct {
        context string
        expr    hcl.Expression
    }
    var exprs []namedExpr

    for _, step := range g.Steps {
        for k, expr := range step.InputExprs {
            exprs = append(exprs, namedExpr{
                context: fmt.Sprintf("step %q input %q", step.Name, k),
                expr:    expr,
            })
        }
        for outName, co := range step.Outcomes {
            if co.OutputExpr != nil {
                exprs = append(exprs, namedExpr{
                    context: fmt.Sprintf("step %q outcome %q output", step.Name, outName),
                    expr:    co.OutputExpr,
                })
            }
        }
    }
    for swName, sw := range g.Switches {
        for i, cond := range sw.Conditions {
            exprs = append(exprs, namedExpr{
                context: fmt.Sprintf("switch %q condition[%d]", swName, i),
                expr:    cond.Match,
            })
        }
        if sw.DefaultOutput != nil {
            exprs = append(exprs, namedExpr{
                context: fmt.Sprintf("switch %q default output", swName),
                expr:    sw.DefaultOutput,
            })
        }
    }

    for _, ne := range exprs {
        diags = append(diags, checkStepsFieldTraversals(ne.context, ne.expr, g, schemas)...)
    }
    return diags
}

// checkStepsFieldTraversals inspects expr for steps.<name>.<field> traversals
// and emits warnings for fields absent from the step's OutputSchema.
func checkStepsFieldTraversals(context string, expr hcl.Expression, g *FSMGraph, schemas map[string]AdapterInfo) hcl.Diagnostics {
    var diags hcl.Diagnostics
    for _, traversal := range expr.Variables() {
        // Require at least: steps . <name> . <field>
        if len(traversal) < 3 {
            continue
        }
        root, rootOK := traversal[0].(hcl.TraverseRoot)
        nameAttr, nameOK := traversal[1].(hcl.TraverseAttr)
        fieldAttr, fieldOK := traversal[2].(hcl.TraverseAttr)
        if !rootOK || !nameOK || !fieldOK {
            continue
        }
        if root.Name != "steps" {
            continue
        }

        step, isStep := g.Steps[nameAttr.Name]
        if !isStep {
            // Unknown step name — already caught as an error by validateSwitchExprRefs
            // for switch conditions; step input expressions may not have been checked.
            // Emit a warning here so both sites are covered; it is not promoted to an
            // error because the inline compilers already own that check for switches.
            continue
        }

        // Look up the step's OutputSchema via its AdapterRef.
        info, hasSchema := adapterInfo(schemas, adapterTypeFromRef(step.AdapterRef))
        if !hasSchema || len(info.OutputSchema) == 0 {
            continue // no declared contract; permissive
        }

        if _, known := info.OutputSchema[fieldAttr.Name]; !known {
            r := fieldAttr.SrcRange
            diags = append(diags, &hcl.Diagnostic{
                Severity: hcl.DiagWarning,
                Summary: fmt.Sprintf(
                    "%s: field %q is not declared in the output schema of step %q (adapter %q)",
                    context, fieldAttr.Name, nameAttr.Name, step.AdapterRef,
                ),
                Subject: &r,
            })
        }
    }
    return diags
}
```

### Step 2 — Call the pass from `CompileWithOpts`

Edit [workflow/compile.go](../workflow/compile.go) in `CompileWithOpts`, immediately after the
`warnBackEdges` call:

```go
diags = append(diags, warnBackEdges(g)...)
diags = append(diags, warnCrossStepFieldRefs(g, schemas)...)
```

The pass is a warning-only scan; it never sets `diags.HasErrors()`, so it does not affect the
`if diags.HasErrors() { return nil, diags }` guard below it.

### Step 3 — Upgrade `validateSwitchExprRefs` to also check field names

The existing `case "steps":` block in `validateSwitchExprRefs`
([workflow/compile_switches.go:295](../workflow/compile_switches.go#L295)) validates only the
step name. Extend it to also check the field name when a schema is available, consistent with the
new post-pass:

```go
case "steps":
    // ... existing step-name and self-reference checks ...

    // Check field name against step's OutputSchema when a schema is available.
    // Require at least steps.<name>.<field> (three segments).
    if len(traversal) >= 3 {
        fieldAttr, fieldOK := traversal[2].(hcl.TraverseAttr)
        if fieldOK && (isStep || isSwitch) {
            if isStep {
                stepNode := g.Steps[attr.Name]
                info, hasSchema := adapterInfo(schemas, adapterTypeFromRef(stepNode.AdapterRef))
                if hasSchema && len(info.OutputSchema) > 0 {
                    if _, known := info.OutputSchema[fieldAttr.Name]; !known {
                        r := fieldAttr.SrcRange
                        diags = append(diags, &hcl.Diagnostic{
                            Severity: hcl.DiagWarning,
                            Summary:  fmt.Sprintf("switch %q condition[%d]: field %q is not declared in the output schema of step %q", switchName, condIdx, fieldAttr.Name, attr.Name),
                            Subject:  &r,
                        })
                    }
                }
            }
        }
    }
```

`validateSwitchExprRefs` is called inline during compilation, before `g.Steps` is complete for
the overall workflow. However, switch nodes are compiled after all step nodes
([workflow/compile.go](../workflow/compile.go) shows `compileSwitches` is called after
`compileSteps`), so at the point `compileSwitches` runs, `g.Steps` is fully populated. The inline
check is therefore safe and produces tighter error messages than the post-pass (it knows the
switch name and condition index).

To make this work, `validateSwitchExprRefs` must receive `schemas` as an additional parameter.
Update its signature and all call sites (one call in [workflow/compile_switches.go](../workflow/compile_switches.go)).

### Step 4 — Tests

Add to a new file [workflow/compile_cross_step_refs_test.go](../workflow/compile_cross_step_refs_test.go)
(preferred over appending to existing files, given the volume):

1. **`TestWarnCrossStepField_SwitchKnownField`** — switch condition `match = steps.build.stdout == "ok"`;
   schema declares `stdout`. Must produce no diagnostic.

2. **`TestWarnCrossStepField_SwitchUnknownField`** — switch condition `match = steps.build.stddout == "ok"`;
   schema does NOT include `stddout`. Must produce a `DiagWarning` containing `"stddout"`.

3. **`TestWarnCrossStepField_StepInputKnownField`** — step input `command = steps.build.stdout`;
   schema declares `stdout`. No diagnostic.

4. **`TestWarnCrossStepField_StepInputUnknownField`** — step input `command = steps.build.stddout`;
   schema does NOT include `stddout`. `DiagWarning` containing `"stddout"`.

5. **`TestWarnCrossStepField_NoSchema`** — any `steps.<name>.<field>` reference with nil schemas.
   No diagnostic (permissive).

6. **`TestWarnCrossStepField_OutcomeOutputCrossStep`** — outcome `output = { x = steps.build.stdout }`;
   schema declares `stdout`. No diagnostic.

7. **`TestWarnCrossStepField_OutcomeOutputCrossStepUnknown`** — outcome `output = { x = steps.build.ghost }`;
   schema does NOT include `ghost`. `DiagWarning` containing `"ghost"`.

All tests wire the schema via the `schemas` argument to `Compile` (or `CompileWithOpts`):
`map[string]AdapterInfo{"noop.default": {OutputSchema: map[string]ConfigField{"stdout": {}}}}`.

Existing tests that use `steps.*` refs without a schema (e.g. `TestCompileOutcome_OutputExprRuntimeRef`,
`TestSwitch_FirstMatchWins`) must continue to pass — they pass nil schemas and should not be
affected.

## Behavior change

**Yes — new compile warnings when `OutputSchema` is provided.**

- `steps.<name>.<field>` traversals where `<field>` is absent from the referenced step's
  `OutputSchema` now produce a `DiagWarning` at compile time.
- `DiagWarning` does not prevent compilation from succeeding (`Compile` still returns a valid
  `*FSMGraph`).
- When no schema is provided for the referenced adapter, behavior is unchanged — permissive.
- No change to runtime behavior. No change to the wire contract or event types.
- `validateSwitchExprRefs` gains an additional warning for field names in switch conditions;
  its signature gains a `schemas` parameter (internal function, no public API impact).

## Reuse

- `validateSwitchExprRefs` traversal pattern — extend, do not duplicate.
- `adapterInfo` and `adapterTypeFromRef` helpers from
  [workflow/compile_adapters.go:131](../workflow/compile_adapters.go#L131) and
  [workflow/compile_steps_adapter.go:88](../workflow/compile_steps_adapter.go#L88) — use as-is.
- `warnBackEdges` in [workflow/compile_steps_graph.go](../workflow/compile_steps_graph.go) —
  the structural pattern for the post-compilation warning pass.
- `hcl.TraverseRoot`, `hcl.TraverseAttr` — same types used throughout the `workflow/` package.

## Out of scope

- Promoting these warnings to errors. That is a separate decision, not in scope for this bugfix.
- Validating `step.output.<field>` (same-step namespace in outcome projections) — covered by BF-02.
- Validating `var.*` or `local.*` reference field names — those are already compile-time errors
  via `validateFoldableAttrs`.
- Iterating-step `each.*` namespace validation.
- Subworkflow `subworkflow.*` namespace validation (subworkflow output fields are not tracked in
  the FSMGraph at compile time).
- Any change to the wire contract, event types, `Sink` interface, or engine runtime.

## Files this workstream may modify

- `workflow/compile_steps_graph.go` — add `warnCrossStepFieldRefs` and `checkStepsFieldTraversals`.
- `workflow/compile.go` — add `warnCrossStepFieldRefs(g, schemas)` call after `warnBackEdges`.
- `workflow/compile_switches.go` — extend `validateSwitchExprRefs` with field check; add
  `schemas` parameter; update its single call site.
- `workflow/compile_cross_step_refs_test.go` — new test file with 7 tests.

This workstream may **not** edit `README.md`, `PLAN.md`, `AGENTS.md`, `CHANGELOG.md`,
`CONTRIBUTING.md`, `workstreams/README.md`, or any other workstream file.

## Tasks

- [x] Add `warnCrossStepFieldRefs` and `checkStepsFieldTraversals` to `workflow/compile_steps_graph.go`.
- [x] Add `warnCrossStepFieldRefs(g, schemas)` call in `CompileWithOpts` after `warnBackEdges`.
- [x] Add `schemas` parameter to `validateSwitchExprRefs`; add field-name check in `case "steps"`.
- [x] Update the single `validateSwitchExprRefs` call site in `compile_switches.go`.
- [x] Add `workflow/compile_cross_step_refs_test.go` with all 7 tests.
- [x] `go test ./workflow/ -run TestWarnCrossStepField` passes.
- [x] Confirm `TestCompileOutcome_OutputExprRuntimeRef` and `TestSwitch_FirstMatchWins` still pass.
- [x] `make test` clean.

## Exit criteria

- `steps.build.stddout` (typo) in a switch condition, step input, or outcome output projection,
  when the `build` step's adapter has a schema that does not include `stddout`, produces a
  `DiagWarning` at compile time.
- `steps.build.stdout` when the schema declares `stdout` produces no diagnostic.
- All `steps.*` refs when `schemas` is nil produce no diagnostic.
- Compile still succeeds (returns a valid `*FSMGraph`) for all warning-only cases.
- `make test` clean.

## Reviewer Notes

**Implementation summary:**

1. **`workflow/compile_steps_graph.go`** — Added `warnCrossStepFieldRefs(g, schemas)` (post-pass
   collector) and `checkStepsFieldTraversals(context, expr, g, schemas)` (per-expression checker).
   Both follow the `warnBackEdges` pattern exactly. Traversal shape `steps.<name>.<field>` is
   matched; unknown step names are skipped (already an error elsewhere); steps with no
   `OutputSchema` are permissive.

2. **`workflow/compile.go`** — One-line addition: `diags = append(diags, warnCrossStepFieldRefs(g, schemas)...)`
   immediately after the `warnBackEdges` call. Also threaded `schemas` into `compileSwitches`.

3. **`workflow/compile_switches.go`** — `compileSwitches`, `compileSwitchConditionBlock`, and
   `validateSwitchExprRefs` each gained a `schemas map[string]AdapterInfo` parameter. In
   `validateSwitchExprRefs`, the `case "steps"` arm now checks the third traversal segment against
   `OutputSchema` when a schema is available, consistent with the post-pass.

4. **`workflow/compile_cross_step_refs_test.go`** — New file with all 7 specified tests.
   Helper `outputSchemaFor` named to avoid conflict with the existing `noopSchema` var in
   `compile_input_test.go`.

**Validation:**
- `go test ./workflow/ -run TestWarnCrossStepField` — all 7 PASS
- `TestCompileOutcome_OutputExprRuntimeRef` — PASS (nil schemas, no warnings)
- `make test` — clean across all packages (workflow race-tested)

### Review 2026-05-07 — changes-requested

#### Summary
Implementation is close, but the switch-condition path currently emits duplicate warnings for the same bad `steps.<name>.<field>` reference, so the behavior does not meet a clean acceptance bar yet. Test coverage also misses that regression because the new tests only assert warning presence, not warning cardinality or successful graph return for warning-only compiles. No separate security concerns were identified in this pass.

#### Plan Adherence
- **Step 1 / Step 2 / Step 3:** Implemented, but the combined behavior is incorrect for switch conditions: `validateSwitchExprRefs` warns inline and `warnCrossStepFieldRefs` warns again during the post-pass for the same traversal.
- **Step 4:** The requested test file was added with the seven named tests, but the assertions are not strong enough to prove the exit criteria. In particular, they do not detect duplicate warnings and they do not assert that warning-only compiles still return a valid `*FSMGraph`.
- **Exit criteria:** `make test` is clean, permissive nil-schema behavior still holds, and known fields stay warning-free. The warning-on-typo criterion is only partially satisfied because the switch case currently produces two warnings instead of one coherent compile-time warning.

#### Required Remediations
- **Blocker** — `workflow/compile.go:107-108`, `workflow/compile_steps_graph.go:364-380`, `workflow/compile_switches.go:316-333`: switch-condition field validation is performed twice, once inline and once again in the post-pass, so `steps.build.stddout` in a switch emits two warnings. **Acceptance criteria:** a bad cross-step field in a switch `match` expression must produce exactly one warning; retain warning coverage for step-input and outcome-output sites without duplicating the switch diagnostic.
- **Blocker** — `workflow/compile_cross_step_refs_test.go:133-146`, `workflow/compile_cross_step_refs_test.go:166-178`, `workflow/compile_cross_step_refs_test.go:213-225`: the unknown-field tests only check for the existence of a matching warning substring, so the current duplicate-warning bug passes unnoticed; the tests also ignore the returned graph, leaving the "compile still succeeds" exit criterion unproven. **Acceptance criteria:** strengthen the tests to assert warning counts (especially exactly one warning for the switch unknown-field case, and no warnings for the known/nil-schema cases) and assert that warning-only compiles return a non-nil graph.

#### Test Intent Assessment
The new tests do exercise the intended expression sites, which is the right shape. The weak point is regression sensitivity: a faulty implementation that emits duplicate diagnostics still passes, and the warning-only success contract is not asserted because the returned graph is discarded. Tightening those assertions is required before this workstream can be approved.

### Remediation 2026-05-07

**Blocker 1 fixed** — `warnCrossStepFieldRefs` no longer includes `SwitchCondition.Match`
expressions in its post-pass. Switch match expressions are handled inline by
`validateSwitchExprRefs` (which runs after `g.Steps` is fully populated because
`compileSwitches` is called after `compileSteps`). Each bad field reference in a switch
condition now produces exactly one warning.  The post-pass retains coverage for step inputs,
outcome output projections, and switch default output expressions.

**Lint fix 2026-05-07** — `validateSwitchExprRefs` exceeded the gocognit limit of 20 (was 39)
after the field-check addition. Extracted two helpers to restore compliance:
- `validateSwitchStepTraversal` — handles self-reference check, unknown-step check, and delegates to field check.
- `validateSwitchStepFieldRef` — checks the third traversal segment against `OutputSchema`.
`make lint-go` and `make test` clean.
- Assert a non-nil `*FSMGraph` is returned for warning-only compiles.
- Assert exact warning counts via `countWarnings` helper: unknown-field cases require count == 1;
  known-field and nil-schema cases require count == 0.

`make test` clean.

### Review 2026-05-07-02 — approved

#### Summary
The prior blockers are resolved. Switch-condition cross-step field validation no longer emits duplicate warnings, the warning-only compile path now stays explicitly covered by tests, and the implementation matches the workstream scope and exit criteria. No security concerns were identified in this pass.

#### Plan Adherence
- **Step 1 / Step 2 / Step 3:** Implemented correctly. `warnCrossStepFieldRefs` now covers step inputs, outcome output projections, and switch default output without duplicating the inline switch-condition warning path.
- **Step 4:** The new tests now assert warning cardinality and confirm warning-only compiles return a non-nil `*FSMGraph`, which closes the prior regression gap.
- **Exit criteria:** Satisfied. Unknown cross-step fields warn at compile time when schema is present, known fields stay clean, nil-schema compiles remain permissive, warning-only compiles succeed, and repository validation is green.

#### Test Intent Assessment
The tests now validate behavioral intent instead of mere warning presence. In particular, the switch unknown-field case is regression-sensitive to duplicate diagnostics, and the warning-only cases explicitly prove compile success by asserting a returned graph.

#### Validation Performed
- `go test ./workflow/ -run 'TestWarnCrossStepField|TestCompileOutcome_OutputExprRuntimeRef|TestSwitch_FirstMatchWins'` — passed.
- `make lint-go` — passed.
- `make test` — passed.
- Ad-hoc compile probe for `match = steps.build.stddout == "ok"` with schema `{stdout}` — observed `WARN_COUNT=1` and `GRAPH_NON_NIL=true`.

### Post-review remediation 2026-05-08 (PR #95 thread fixes)

Three unresolved reviewer threads addressed:

1. **PRRT_kwDOSOBb1s6AhWrm — Coverage gap: `SwitchCondition.OutputExpr` never checked** (`compile_steps_graph.go:378`)
   - Added inner loop over `sw.Conditions` in `warnCrossStepFieldRefs` to enqueue each non-nil `cond.OutputExpr` alongside `sw.DefaultOutput`.
   - Updated doc comment to list `SwitchCondition.OutputExpr` as a checked site.
   - Added `TestWarnCrossStepField_SwitchCondOutputKnownField` and `TestWarnCrossStepField_SwitchCondOutputUnknownField` regression tests in `compile_cross_step_refs_test.go`.

2. **PRRT_kwDOSOBb1s6AhWro — Non-deterministic diagnostic ordering** (`compile_steps_graph.go:353`)
   - Changed step loop from `for _, step := range g.Steps` to `for _, name := range g.stepOrder` for deterministic step order.
   - Changed switch loop from `for swName, sw := range g.Switches` to a sorted-key walk (added `sort.Strings` over collected switch names).

3. **PRRT_kwDOSOBb1s6AhWrq — Comment overstates coverage** (`compile_steps_graph.go:409`)
   - Replaced the misleading "already caught as an error by validateSwitchExprRefs" comment.
   - Implemented option 1 from the reviewer: emit a `DiagWarning` for unknown step names at non-switch sites (step inputs, outcome outputs, switch condition/default outputs), so typos like `steps.bulid.stdout` surface at compile time rather than silently failing at runtime.
   - Added `TestWarnCrossStepField_UnknownStepName` regression test.

Validation: `make test` — all pass.

### Review 2026-05-07-03 — approved

#### Summary
The latest executor changes meet the workstream scope and exit criteria. Cross-step field validation now warns exactly once for bad switch-condition references, continues to cover step-input and outcome-output expressions in the post-pass, remains permissive when schemas are absent, and preserves successful compilation for warning-only cases. No security or architecture issues were found in this review pass.

#### Plan Adherence
- **Step 1 / Step 2:** Implemented as required. `warnCrossStepFieldRefs` is wired from `CompileWithOpts` after `warnBackEdges`, and its post-pass coverage now correctly focuses on step inputs, outcome output projections, and switch default output without re-walking switch `match` expressions.
- **Step 3:** Implemented correctly. `validateSwitchExprRefs` now threads `schemas` through the switch compilation path and validates the third `steps.<name>.<field>` segment against the referenced step's `OutputSchema` when available.
- **Step 4:** Implemented and now sufficiently asserted. The seven requested tests are present, and the warning-only cases assert both exact warning cardinality and a non-nil `*FSMGraph`, which directly proves the intended behavior.
- **Exit criteria:** Satisfied. Unknown cross-step fields warn at compile time when schema-backed, known fields remain clean, nil-schema compiles remain permissive, and warning-only compiles succeed.

#### Test Intent Assessment
The tests now validate behavioral intent rather than mere execution success. The switch unknown-field case is sensitive to the duplicate-warning regression that previously existed, and the warning-only cases assert returned graph presence so a broken "warn then fail compilation" implementation would not pass. For this internal compiler change, the focused workflow compilation tests are the appropriate level of coverage.

#### Validation Performed
- `git --no-pager diff --name-status origin/main...HEAD` — reviewed changed scope; no unexpected source or baseline files were modified outside the workstream.
- `git --no-pager diff --check origin/main...HEAD` — passed.
- `go test ./workflow -run 'TestWarnCrossStepField|TestCompileOutcome_OutputExprRuntimeRef|TestSwitch_FirstMatchWins'` — passed.
- `make lint-go && make test` — passed.
