# WS05 — Compiler hardening and eval extensions: step-ref errors, path variables, `hasattr`/`can`/`try`

**Phase:** Language Cleanup · **Track:** Language · **Owner:** Workstream executor · **Depends on:** [WS02](WS02-data-and-outcome-semantics.md) (eval context shape, function registry pattern). · **Unblocks:** safer workflow authoring; richer error-handling patterns in adapter-v2 workflows. · **Base branch:** `main`

## Context

Three independent improvements to the compiler and eval layer that share the same code area and can land in one focused PR:

1. **Step-ref warnings should be errors.** The post-compilation pass `warnCrossStepFieldRefs` emits `DiagWarning` for unknown step names and unknown fields on steps with a known output schema. Compilation succeeds; the error surfaces only at runtime. At post-compilation time `g.Steps` is fully populated — an unknown step name genuinely does not exist in the workflow, and a misspelled field name on a schema-bearing step is a typo. There is no legitimate forward-reference justification for either case. These should be `DiagError`.

2. **Path variables and functions.** Workflow authors need to reference paths relative to the workflow file and the project root — essential for `file()`, `fileset()`, and `templatefile()` calls and for constructing paths to sibling files. Terraform provides `path.module`, `path.root`, `path.cwd`, plus `abspath()`, `dirname()`, `basename()`. Criteria should expose the same surface with `path.workflow` in place of `path.module` (a workflow file, not a module).

3. **`hasattr`, `can`, `try`.** Steps may return outputs typed `any`, adapters may be called with optional keys, and forward references to step outputs can fail at runtime when a step's schema is not known at compile time. Terraform's `hasattr`, `can`, and `try` give authors a principled way to handle these cases without forcing a crash. All three are available in the existing HCL/cty stdlib and only need to be registered.

Combining them is efficient: items 2 and 3 both land in `eval.go` / `eval_functions.go`, and item 1 is a two-line change in `compile_steps_graph.go` that a reviewer already in the compiler will handle trivially.

## Prerequisites

- WS02 merged (eval context shape, `FunctionOptions` struct, `workflowFunctions` registry).

## In scope

### Step 1 — Step-ref diagnostics: warnings → errors

**File:** [workflow/compile_steps_graph.go](../../workflow/compile_steps_graph.go)

In `warnCrossStepFieldRefs` (rename to `checkCrossStepFieldRefs`):

- Change the `DiagWarning` for an unknown step name to `DiagError`.
- Change the `DiagWarning` for an unknown field on a step with a known output schema to `DiagError`.
- Leave the "no adapter schema available" path unchanged — that is the legitimate uncertainty case.

The function rename (`warn` → `check`) reflects the new severity. Update the single call site in `compileFSMGraph` or equivalent.

### Step 2 — Path variables

**File:** [workflow/eval.go](../../workflow/eval.go)

Add a `path` object to `BuildEvalContextWithOpts`. The three values are populated from `FunctionOptions`, which already carries `WorkflowDir`:

```go
"path": cty.ObjectVal(map[string]cty.Value{
    "workflow": cty.StringVal(opts.WorkflowDir),   // directory containing the workflow file
    "root":     cty.StringVal(opts.RootDir),        // project root (criteria invocation cwd)
    "cwd":      cty.StringVal(opts.Cwd),            // current working directory at runtime
}),
```

Add `RootDir` and `Cwd` fields to `FunctionOptions`:

```go
type FunctionOptions struct {
    WorkflowDir string
    RootDir     string // new: project root directory
    Cwd         string // new: process working directory
}
```

Pass these through from the CLI/run layer where `FunctionOptions` is constructed. `RootDir` is the directory from which `criteria` was invoked; `Cwd` is `os.Getwd()` at the time of evaluation (same as `RootDir` in the common case; separate for completeness).

**Compile-time:** add `"path"` to the set of runtime-only namespaces in `compile_fold.go` (alongside `"steps"`, `"data"`, etc.) so path references are not constant-folded.

### Step 3 — Path functions

**File:** [workflow/eval_functions.go](../../workflow/eval_functions.go)

Add to `workflowFunctions()`:

- `abspath(path)` — wraps `filepath.Abs`; returns the absolute path of the argument, resolving relative paths against `opts.WorkflowDir`.
- `dirname(path)` — wraps `filepath.Dir`; returns the parent directory component.
- `basename(path)` — wraps `filepath.Base`; returns the file name component.

These follow the same opts-capturing closure pattern used by the existing `file()`, `fileexists()`, and `fileset()` functions.

### Step 4 — `hasattr`, `can`, `try`

**File:** [workflow/eval_functions.go](../../workflow/eval_functions.go)

Register in `workflowFunctions()`:

- `can(expr)` — from `github.com/zclconf/go-cty/cty/function/stdlib`; evaluates an expression, returns `true` if it succeeds without error, `false` otherwise.
- `try(expr...)` — from `github.com/zclconf/go-cty/cty/function/stdlib`; evaluates expressions in order, returns the first value that does not produce an error.
- `hasattr(obj, name)` — custom implementation: given a cty object and a string attribute name, returns `true` if the attribute exists on the object. Handles the `any`-typed case by inspecting the actual value type at evaluation time.

`can` and `try` are direct stdlib registrations (same pattern as the ~80 stdlib functions added in WS01). `hasattr` is a small custom function over cty's `Type().HasAttribute(name)`.

### Step 5 — Tests

**Compiler tests** (`workflow/compile_steps_graph_test.go` or equivalent):
- `steps.ghost.result` (step `ghost` not in graph) produces `DiagError`, not `DiagWarning`.
- `steps.build.stddout` (step `build` in graph, schema known, field `stddout` absent) produces `DiagError`, not `DiagWarning`.
- `steps.build.stdout` (valid field) produces no diagnostic (regression guard).
- `steps.maybe.result` (no adapter schema available) produces no diagnostic (permissive case preserved).

**Eval context tests** (`workflow/eval_test.go` or equivalent):
- `path.workflow`, `path.root`, `path.cwd` resolve to the correct values in an eval context.
- `abspath("relative/path")` returns an absolute path resolved against `WorkflowDir`.
- `dirname("/foo/bar/baz.hcl")` returns `"/foo/bar"`.
- `basename("/foo/bar/baz.hcl")` returns `"baz.hcl"`.

**Function tests**:
- `hasattr(obj, "key")` returns `true` when the key exists, `false` when it does not.
- `can(expr)` returns `true` for valid expressions, `false` for expressions that would error.
- `try(expr_bad, expr_good)` returns the value of `expr_good` when `expr_bad` would error.

## Out of scope

- Full `try`/`can` semantics at compile time (folding, static analysis) — these remain runtime-only.
- New language constructs for structured error handling (e.g. `on_error` blocks) — future work.
- Changing the permissive path for steps with unknown schemas — that case stays a no-op.

## Reuse pointers

- Existing `workflowFunctions()` in [eval_functions.go](../../workflow/eval_functions.go) — add to existing function map; no new registration entry points needed.
- Existing `FunctionOptions` struct in [eval_functions.go](../../workflow/eval_functions.go) — extend in place.
- Existing `file()` / `fileexists()` closures in [eval_functions.go](../../workflow/eval_functions.go) — pattern for `abspath()`, `dirname()`, `basename()`.
- `github.com/zclconf/go-cty/cty/function/stdlib` — already imported; `can` and `try` are in this package.
- `runtimeOnlyNamespaces` in [compile_fold.go](../../workflow/compile_fold.go) — add `"path"` alongside existing entries.

## Behavior change

**User-facing:** workflows with misspelled step references or field names now fail at compile time instead of at runtime. `path.workflow`, `path.root`, `path.cwd`, `abspath()`, `dirname()`, `basename()`, `hasattr()`, `can()`, and `try()` are now available in all expressions.

**Runtime semantics:** unchanged for all workflows without step-ref errors. No HCL file migrations required.

## Tests required

- All existing tests pass.
- New tests in Step 5 pass.
- `go vet ./...` clean.
- `make validate` passes on all examples.
- Manual: a workflow with `steps.ghost.result` fails to compile with a `DiagError`.
- Manual: `path.workflow` in a `file()` call resolves correctly during `criteria run`.

## Implementation Notes

### Checklist

- [ ] Step 1 — `warnCrossStepFieldRefs` → `checkCrossStepFieldRefs`; `DiagWarning` → `DiagError` for unknown step and unknown field cases
- [ ] Step 2 — `path` object in eval context; `RootDir`/`Cwd` added to `FunctionOptions`; `"path"` added to runtime-only namespaces
- [ ] Step 3 — `abspath()`, `dirname()`, `basename()` registered in `workflowFunctions()`
- [ ] Step 4 — `hasattr()`, `can()`, `try()` registered in `workflowFunctions()`
- [ ] Step 5 — Tests

### Reviewer Notes

_To be filled in during review._
