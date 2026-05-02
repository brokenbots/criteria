# Workstream 07 — `local` block + compile-time constant-fold pass

**Phase:** 3 · **Track:** B (compile-time semantics) · **Owner:** Workstream executor · **Depends on:** [03-split-compile-steps.md](03-split-compile-steps.md) (compile flow already split along step-kind lines so the fold pass plugs in cleanly). · **Unblocks:** [11-agent-to-adapter-rename.md](11-agent-to-adapter-rename.md) (`adapter.config` validation depends on this fold pass), [12-adapter-lifecycle-automation.md](12-adapter-lifecycle-automation.md), [13-subworkflow-block-and-resolver.md](13-subworkflow-block-and-resolver.md).

## Context

Three distinct compile-time gaps documented in [architecture_notes.md](../../architecture_notes.md):

1. **No `local` block.** `var.*` defaults are evaluated with a `nil` context (literals only), and there is no intermediate compile-time computed value type. Workflows that want a derived value (`local.full_path = "${var.base}/${var.name}"`) have nowhere to put it.
2. **`file()` validation is too narrow.** [`validateFileFunctionCalls`](../../workflow/compile_validation.go#L97) at line 109 explicitly skips any expression that has variable references: `if len(attr.Expr.Variables()) > 0 { continue }`. So `file(var.path)` is never validated even when `var.path` has a known constant default. The validation also runs against `step.input` only — not against `output.value` blocks, `branch.when` (will be `switch.condition.match` after [16](16-switch-and-if-flow-control.md)), `for_each`/`count`/`parallel` modifiers, or `adapter.config`.
3. **Variable-name validation is silent.** A reference to `var.does_not_exist` produces a runtime error rather than a compile diagnostic. [eval.go:160 `SeedVarsFromGraph`](../../workflow/eval.go#L160) seeds vars from the graph; the compiler does not check that `step.input` / `output.value` / etc. only reference declared names.

This workstream fixes all three at once because they share a single primitive: a **constant-fold evaluator** over the closure `var ∪ local ∪ literal`. Any expression whose free variables are entirely in that closure can be reduced to a `cty.Value` at compile, validated, and stored. Expressions that reference `each.*`, `steps.*`, or `shared_variable.*` (after [18](18-shared-variable-block.md)) stay deferred to runtime.

The Phase 3 runtime-vs-compile boundary requires this primitive everywhere a literal-or-var-only expression appears.

**Note on `agent.config` (per [architecture_notes.md §file()](../../architecture_notes.md)):** Phase 1's W07 file-expression-function landed `agentConfigEvalContext` ([workflow/compile_agents.go:22](../../workflow/compile_agents.go#L22)) which **does** register `file`/`fileexists`/`trimfrontmatter` for compile-time evaluation of `agent.config`. The "silent `""` drop" described in architecture_notes was the **pre-W07** behavior. The remaining gap is that **`validateFileFunctionCalls` is not invoked over `agent.config` attributes** — so a `file()` call that targets a non-existent path inside `agent.config` evaluates eagerly at compile (good) and fails with a hard error (good) — but a `file(var.path)` call where `var.path = "/nope"` skips validation because of the `Variables() > 0` guard at line 109 (bad). This workstream closes that gap.

## Prerequisites

- [03-split-compile-steps.md](03-split-compile-steps.md) merged: per-kind compilers in `workflow/compile_steps_*.go`.
- Familiarity with [workflow/eval.go](../../workflow/eval.go) (`BuildEvalContextWithOpts`, `SeedVarsFromGraph`, `ApplyVarOverrides`).
- Familiarity with [workflow/compile_validation.go](../../workflow/compile_validation.go) (`validateFileFunctionCalls`, `decodeAttrsToStringMap`).
- `make ci` green on `main`.

## In scope

### Step 1 — Add `local "<name>"` schema

In [workflow/schema.go](../../workflow/schema.go) add `LocalSpec` and `LocalNode`:

```go
// LocalSpec declares a compile-time-resolved local value.
type LocalSpec struct {
    Name        string   `hcl:"name,label"`
    Description string   `hcl:"description,optional"`
    Remain      hcl.Body `hcl:",remain"` // captures the "value" expression
}

// LocalNode is a compiled local declaration.
type LocalNode struct {
    Name        string
    Type        cty.Type   // inferred from the folded value
    Value       cty.Value  // fully resolved at compile
    Description string
}
```

In `Spec` struct (line 13), add `Locals []LocalSpec \`hcl:"local,block"\`` between `Variables` and `Agents`.

In `FSMGraph` struct (line 224), add `Locals map[string]*LocalNode` between `Variables` and `Agents`.

### Step 2 — Build the constant-fold evaluator

New file `workflow/compile_fold.go`. Public entry point:

```go
// FoldExpr evaluates expr in the closure (var ∪ local ∪ literal ∪ funcs).
// Returns the cty.Value if the expression folds, or (cty.NilVal, false) if
// it references runtime-only namespaces (each, steps, shared_variable).
//
// Diagnostics are returned for *fold-time* errors (unknown var, type
// mismatch, file-not-found via file()/fileexists()). Runtime-only refs
// are not errors — they signal "leave this expression for the engine".
func FoldExpr(
    expr hcl.Expression,
    vars map[string]cty.Value,    // resolved var.* values
    locals map[string]cty.Value,  // resolved local.* values, in declaration order
    workflowDir string,
) (cty.Value, bool, hcl.Diagnostics)
```

Implementation contract:

1. Inspect `expr.Variables()`. For each traversal, record its root segment (`var`, `local`, `each`, `steps`, `shared_variable`, etc.).
2. If any root is in `{each, steps, shared_variable}`, return `(cty.NilVal, false, nil)` — runtime-deferred, not an error.
3. Otherwise build an `hcl.EvalContext` with:
   - `Variables`: `{"var": cty.ObjectVal(vars), "local": cty.ObjectVal(locals)}`.
   - `Functions`: `workflowFunctions(DefaultFunctionOptions(workflowDir))` — same registration the existing `agentConfigEvalContext` uses.
4. Call `expr.Value(ctx)`. Return the value, `true`, and any diagnostics. Diagnostics with `DiagError` severity make the expression a compile failure.

The closure check on `Variables()` is **not optional**. The current `validateFileFunctionCalls` skips on `Variables() > 0` precisely because there is no fold pass; this new path replaces that skip.

### Step 3 — Compile `local` blocks

New file `workflow/compile_locals.go`. The compile flow:

```go
// compileLocals folds every local.* declaration in declaration order.
// A later local may reference an earlier local; cycles are a compile error.
func compileLocals(g *FSMGraph, spec *Spec, opts CompileOpts) hcl.Diagnostics
```

Algorithm:

1. Topologically order `spec.Locals` by their inter-local references. Use a stable sort: declaration order for ties. If a cycle is detected, emit a diagnostic with all participating local names and return.
2. Walk the ordered list. For each `LocalSpec`:
   - Build a `vars` map from `g.Variables` (already compiled by `compileVariables`).
   - Build a `locals` map from `g.Locals` populated so far.
   - Extract the `value` attribute from `Remain.JustAttributes()`. Exactly one attribute named `value` is required; any other attribute is a compile error.
   - Call `FoldExpr`. If it returns `(_, false, _)`, that means the expression references runtime-only namespaces — **error** (a `local` must fully resolve at compile).
   - Store `LocalNode{Name, Type: val.Type(), Value: val, Description: spec.Description}` in `g.Locals`.

Wire `compileLocals` into the top-level compile in `Compile`/`CompileWithOpts`. It runs after `compileVariables` and before `compileAgents` / `compileSteps`.

### Step 4 — Replace `validateFileFunctionCalls` with the fold pass

Delete the `Variables() > 0` skip at [workflow/compile_validation.go:109](../../workflow/compile_validation.go#L109).

Rewrite `validateFileFunctionCalls` to use `FoldExpr`:

```go
func validateFoldableAttrs(
    attrs hcl.Attributes,
    vars, locals map[string]cty.Value,
    workflowDir string,
) hcl.Diagnostics {
    var diags hcl.Diagnostics
    for _, attr := range attrs {
        _, _, d := FoldExpr(attr.Expr, vars, locals, workflowDir)
        diags = append(diags, d...)
        // If FoldExpr returned (_, false, _), the expression is runtime-deferred;
        // file()/fileexists() validation does not apply. d is empty.
    }
    return diags
}
```

Rename the old `validateFileFunctionCalls` to `validateFoldableAttrs` to reflect the broader scope.

### Step 5 — Broaden the call sites

Currently `validateFileFunctionCalls` is invoked from `compile_steps.go` for `step.input` only. After this workstream, `validateFoldableAttrs` is invoked for **every attribute slot** in the spec where compile-time folding is allowed:

| Attribute slot | Call from | Notes |
|---|---|---|
| `step.input { }` | `workflow/compile_steps_adapter.go` (and iteration variant) | Existing call site; path unchanged in behavior |
| `agent.config { }` | `workflow/compile_agents.go` `compileAgents` | New call site; today the eval context evaluates but `validateFileFunctionCalls` was never invoked |
| `step.workflow { ... output { value = ... } }` | `workflow/compile_steps_workflow.go` | Existing inline output blocks — until [09](09-output-block.md) lands |
| `branch.arm.when` | `workflow/compile_branch.go` (or wherever the branch compiler lives) | Until [16](16-switch-and-if-flow-control.md) replaces with `switch` |
| `step.for_each` / `step.count` | `workflow/compile_steps_iteration.go` | Modifier expressions |

Each call site builds its `vars` map from `g.Variables` and `locals` from `g.Locals`. The `workflowDir` is `opts.WorkflowDir`.

### Step 6 — Validate referenced variable names

In `FoldExpr`, when `vars[name]` is missing for a `var.<name>` traversal, the underlying `expr.Value(ctx)` already errors with "Unknown variable" — that diagnostic now reaches the user as a compile error rather than runtime fail-silent. **Confirm by adding a test**:

```go
// workflow/compile_fold_test.go
func TestFoldExpr_UnknownVarErrors(t *testing.T) {
    // Build a one-line HCL with file(var.does_not_exist).
    // Compile. Assert diags contains a "Unknown variable" with the right Subject.
}
```

This is the headline behavior change of the workstream: a misspelled `var.path` becomes a compile error, not a runtime fail.

For `local.<name>` — same path: missing key in the `locals` map errors with "Unknown variable".

For `each.*`, `steps.*`, `shared_variable.*` (post [18](18-shared-variable-block.md)) — `FoldExpr` returns `(_, false, _)` and the validate-attrs caller does not error. Runtime resolution path applies.

### Step 7 — Update `BuildEvalContextWithOpts` to expose `local.*`

In [workflow/eval.go](../../workflow/eval.go), `BuildEvalContextWithOpts` already takes `vars cty.Value`. Extend it (or add a sibling) to also accept `locals cty.Value` so runtime evaluation can read folded `local.*` values consistently.

```go
func BuildEvalContextWithOpts(vars, locals cty.Value, opts EvalOpts) *hcl.EvalContext
```

`SeedLocalsFromGraph` (new helper alongside `SeedVarsFromGraph`):

```go
func SeedLocalsFromGraph(g *FSMGraph) cty.Value {
    if len(g.Locals) == 0 {
        return cty.EmptyObjectVal
    }
    m := make(map[string]cty.Value, len(g.Locals))
    for name, ln := range g.Locals {
        m[name] = ln.Value
    }
    return cty.ObjectVal(m)
}
```

Engine state setup (`internal/engine/eval.go` or wherever `BuildEvalContextWithOpts` is invoked) calls both seeders.

### Step 8 — Migration: `agent` block keeps its name (deferred to [11](11-agent-to-adapter-rename.md))

This workstream **does not rename** `agent` → `adapter`. That rename lives in [11](11-agent-to-adapter-rename.md). All call sites and diagnostics in this workstream still say "agent". When [11](11-agent-to-adapter-rename.md) lands, it will rename the call sites mechanically.

### Step 9 — Tests

Required test files:

- `workflow/compile_fold_test.go`:
  - `TestFoldExpr_PureLiteral` — `"hello"` → `cty.StringVal("hello"), true`.
  - `TestFoldExpr_VarReference_Resolved` — `var.x` with `vars={x: 42}` → `cty.NumberIntVal(42), true`.
  - `TestFoldExpr_VarReference_Missing` → diagnostic.
  - `TestFoldExpr_LocalReference_Resolved`.
  - `TestFoldExpr_RuntimeOnly_StepsRef` — `steps.foo.out` → `(_, false, nil)`.
  - `TestFoldExpr_RuntimeOnly_EachRef` — `each.value` → `(_, false, nil)`.
  - `TestFoldExpr_FileFunc_Literal_Resolves` → reads a fixture file content.
  - `TestFoldExpr_FileFunc_VarPath_Resolves` — `file(var.path)` where `vars={path: "/fixture.txt"}` → reads file content.
  - `TestFoldExpr_FileFunc_VarPath_Missing` — `file(var.path)` where `vars={path: "/nope"}` → file-not-found diagnostic with the right `Subject`.
  - `TestFoldExpr_FileFunc_RuntimeRef_Skipped` — `file(steps.foo.path)` → `(_, false, nil)`, no diagnostic.

- `workflow/compile_locals_test.go`:
  - `TestCompileLocals_Simple`.
  - `TestCompileLocals_DependsOnVar`.
  - `TestCompileLocals_DependsOnEarlierLocal`.
  - `TestCompileLocals_Cycle` — error includes all participating names.
  - `TestCompileLocals_MultipleAttrs` — extra attribute is an error.
  - `TestCompileLocals_NoValueAttr` — error.
  - `TestCompileLocals_RuntimeRef` — `value = steps.foo.out` is a compile error (locals must fold).

- Compile-flow tests (extend existing test files):
  - In `workflow/compile_validation_test.go`: `TestValidateFoldableAttrs_AgentConfigFile` — `agent.config { prompt = file(var.path) }` with bad `var.path` errors at compile.
  - In `workflow/compile_steps_iteration_test.go`: `TestForEachExprFoldsAtCompile_FilesValidated`.

- End-to-end: an example HCL under [examples/phase3-fold/](../../examples/phase3-fold/) (new directory) demonstrates a workflow with `local`, `var`, and `file(local.path)` and runs to completion under `make validate`.

### Step 10 — Validation

```sh
go build ./...
go test -race -count=2 ./workflow/...
make validate                   # all examples
make lint-go
make lint-baseline-check
make ci
```

All exit 0. The new example under `examples/phase3-fold/` is in the `validate` matrix.

## Behavior change

**Behavior change: yes.** Two observable differences for HCL authors:

1. A `var.*` reference to an undeclared variable now produces a **compile error** with HCL diagnostic source range. Previously it was silently `cty.NullVal(typ)` and surfaced as a runtime error (or as `""` in `step.input`).
2. A `file(...)` call that resolves to a missing path is now caught at compile **even when its argument is `var.*` or `local.*`**, as long as the var/local has a fold-time value. Previously it was deferred to runtime.

Migration burden for existing workflows: workflows with misspelled `var.*` references will fail to compile. This is the intended catch — it cannot be silently aliased. The migration note for v0.2.0 → v0.3.0 (per [21](21-phase3-cleanup-gate.md)) calls this out.

A new top-level block `local "<name>" { value = ... }` is introduced. Existing workflows do not use it; no migration impact for that surface.

No proto change. No SDK change. No event change. No CLI flag change.

## Reuse

- [`agentConfigEvalContext`](../../workflow/compile_agents.go#L22) — pattern for building an `hcl.EvalContext` with the workflow function set; do not duplicate.
- [`workflowFunctions`](../../workflow/eval.go) — function registration; do not redefine.
- [`DefaultFunctionOptions`](../../workflow/eval.go) — function options; do not duplicate.
- [`SeedVarsFromGraph`](../../workflow/eval.go#L160) — pattern for seeding cty values; the new `SeedLocalsFromGraph` mirrors it exactly.
- [`errorDiagsWithFallbackSubject`](../../workflow/compile_validation.go#L70) — diagnostic-subject preservation; reuse for fold errors.
- The per-kind compile layout from [03](03-split-compile-steps.md) is the structural prerequisite — call sites for `validateFoldableAttrs` are already separated by step kind.

## Out of scope

- The `agent` → `adapter` rename. Owned by [11](11-agent-to-adapter-rename.md).
- `WorkflowBodySpec` removal. Owned by [08](08-schema-unification.md).
- Top-level `output` block. Owned by [09](09-output-block.md). (Inline body `output` blocks at [workflow/schema.go:117](../../workflow/schema.go#L117) keep their existing shape until [09](09-output-block.md).)
- `shared_variable` namespace. Owned by [18](18-shared-variable-block.md). (`FoldExpr` should treat `shared_variable.*` as runtime-deferred even before [18](18-shared-variable-block.md) lands; add the namespace to the runtime-only set in Step 2.)
- Renaming `var.*` → anything. The `var` namespace is the established surface; do not change.
- Adding new HCL functions. Function set is fixed by [workflow/eval.go](../../workflow/eval.go).
- Performance work on the fold pass (caching, memoization). The compile path is single-shot; folding once per attribute is acceptable.

## Files this workstream may modify

- [`workflow/schema.go`](../../workflow/schema.go) — add `LocalSpec`, `LocalNode`, `Spec.Locals`, `FSMGraph.Locals`.
- New: `workflow/compile_fold.go`.
- New: `workflow/compile_locals.go`.
- [`workflow/compile_validation.go`](../../workflow/compile_validation.go) — rename + rewrite `validateFileFunctionCalls` → `validateFoldableAttrs`.
- [`workflow/compile_agents.go`](../../workflow/compile_agents.go) — add `validateFoldableAttrs` call.
- `workflow/compile_steps_*.go` (the files [03](03-split-compile-steps.md) created) — broaden call sites per Step 5.
- [`workflow/eval.go`](../../workflow/eval.go) — extend `BuildEvalContextWithOpts`; add `SeedLocalsFromGraph`.
- [`internal/engine/eval.go`](../../internal/engine/eval.go) (or wherever the engine builds the eval context) — pass locals to the eval-context builder.
- New tests: `workflow/compile_fold_test.go`, `workflow/compile_locals_test.go`, additions to existing test files.
- New: `examples/phase3-fold/*.hcl` and any fixture files it references.

This workstream may **not** edit:

- `PLAN.md`, `README.md`, `AGENTS.md`, `CHANGELOG.md`, `workstreams/README.md`, or any other workstream file.
- `.proto` files. No wire change.
- The `agent` block name or `AgentSpec` struct — owned by [11](11-agent-to-adapter-rename.md).
- `WorkflowBodySpec` shape — owned by [08](08-schema-unification.md).
- [`.golangci.baseline.yml`](../../.golangci.baseline.yml) — no new entries; complexity must stay below the cap from [01](01-lint-baseline-burndown.md).

## Tasks

- [ ] Add `LocalSpec` / `LocalNode` to schema (Step 1).
- [ ] Implement `FoldExpr` in `compile_fold.go` (Step 2).
- [ ] Implement `compileLocals` in `compile_locals.go` (Step 3).
- [ ] Rewrite `validateFileFunctionCalls` → `validateFoldableAttrs` (Step 4).
- [ ] Add call sites for every foldable attribute slot (Step 5).
- [ ] Confirm undeclared `var.*` references are now compile errors (Step 6).
- [ ] Extend `BuildEvalContextWithOpts` and add `SeedLocalsFromGraph` (Step 7).
- [ ] Author all new test files (Step 9).
- [ ] Author the example workflow under `examples/phase3-fold/` (Step 9).
- [ ] `make ci` green; baseline cap unchanged (Step 10).

## Exit criteria

- `local "<name>"` blocks parse, compile, and produce a `LocalNode` in `g.Locals`.
- `local` cycles are caught with a diagnostic listing every participating name.
- `var.<undeclared>` produces a compile diagnostic with HCL source range.
- `file(var.x)` with a foldable `var.x` is validated at compile.
- `file(steps.x.y)` is deferred to runtime (no compile error, no validation).
- `validateFoldableAttrs` is invoked over: `step.input`, `agent.config`, inline `output.value`, `branch.arm.when`, `step.for_each`, `step.count`.
- `BuildEvalContextWithOpts` accepts and exposes `local.*` to runtime expressions.
- All new tests in Step 9 exist and pass.
- `examples/phase3-fold/*.hcl` validates and runs end-to-end.
- `make ci` exits 0; lint baseline cap unchanged.

## Tests

The Step 9 test list is the deliverable surface. Coverage targets:

- `workflow/compile_fold.go`: ≥ 90% line coverage.
- `workflow/compile_locals.go`: ≥ 90% line coverage.
- `workflow/compile_validation.go`: existing coverage maintained or raised.

## Risks

| Risk | Mitigation |
|---|---|
| Existing workflows depend on the silent-fail behavior for misspelled `var.*` references | The behavior change is intentional and documented in the migration note. Survey [examples/](../../examples/) and [workflow/testdata/](../../workflow/testdata/) for any test that relied on the silent path; fix the test data to reference declared variables. |
| The fold pass mis-classifies an expression as foldable when it should be deferred | The classification is `Variables()` traversal roots only — a structural check, not a value check. False positives are possible only if HCL ever introduces a new namespace; out of scope. |
| Cycle detection in `compileLocals` produces a confusing diagnostic | Test `TestCompileLocals_Cycle` is the contract. The diagnostic must list every name in the cycle, not just one. Use a tarjan-style SCC check, not a DFS visited flag. |
| `BuildEvalContextWithOpts` signature change breaks callers in [internal/engine/](../../internal/engine/) | Search for every caller before changing the signature; update each in this workstream. If a caller cannot be updated locally (e.g. a sibling workstream's branch already changed it), coordinate via the cleanup gate. |
| The rewrite removes a `// W04: ...` lint exception comment that another workstream relied on | The complexity entries on `validateFileFunctionCalls` should drop, not rise. If a new finding surfaces post-rewrite, extract a helper rather than adding a baseline entry. |
| `examples/phase3-fold/*.hcl` exposes a runtime evaluation path the engine doesn't yet support | Confirm via `make validate` first; if the engine gap is real, this workstream is the wrong one to land it — the engine support belongs in the workstream that introduced the runtime gap. |
