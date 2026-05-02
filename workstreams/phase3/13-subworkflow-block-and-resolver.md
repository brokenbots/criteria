# Workstream 13 — First-class `subworkflow "<name>"` block + CLI `SubWorkflowResolver` wiring

**Phase:** 3 · **Track:** C · **Owner:** Workstream executor · **Depends on:** [02-split-cli-apply.md](02-split-cli-apply.md), [03-split-compile-steps.md](03-split-compile-steps.md), [07-local-block-and-fold-pass.md](07-local-block-and-fold-pass.md), [08-schema-unification.md](08-schema-unification.md), [09-output-block.md](09-output-block.md), [10-environment-block.md](10-environment-block.md), [11-agent-to-adapter-rename.md](11-agent-to-adapter-rename.md), [12-adapter-lifecycle-automation.md](12-adapter-lifecycle-automation.md). · **Unblocks:** [14-universal-step-target.md](14-universal-step-target.md) (universal `target` includes `subworkflow.<name>`).

## Context

[proposed_hcl.hcl](../../proposed_hcl.hcl) introduces `subworkflow "<name>"` as a first-class block declaring a reusable target loaded from a directory:

```hcl
subworkflow "review_loop" {
    source = "./subworkflows/review_loop"   // local path or remote (future)
    environment = shell.ci                  // optional
    input = {                                // bound to the callee's variable blocks
        target = var.target
        max_attempts = 3
    }
}
```

Key semantics from [architecture_notes.md](../../architecture_notes.md) and [proposed_hcl.hcl §4](../../proposed_hcl.hcl):

1. **Deep-compile.** When the parent workflow compiles, every `subworkflow` block's `source` is resolved, parsed, and compiled into a child `FSMGraph`. The deep graph is fully validated before any step executes. Cycle detection covers the source DAG.
2. **Explicit input.** The `input` map binds parent-scope expressions (`var.*`, `local.*`, `each.*`, `steps.*`) to the callee's declared `variable` blocks. Required variables without bindings produce a compile error.
3. **Output projection.** The callee's [09-output-block.md](09-output-block.md) `output` blocks are accessible from the caller as `subworkflow.<name>.<output_name>`.
4. **Scope isolation.** [12-adapter-lifecycle-automation.md](12-adapter-lifecycle-automation.md): the callee declares its own adapters; sessions are isolated and torn down at the callee's terminal state.
5. **CLI wiring.** [TECH_EVALUATION-20260501-01.md §1](../../tech_evaluations/TECH_EVALUATION-20260501-01.md) #3 calls out the half-feature: schema accepts `workflow_file = "..."` but [internal/cli/apply.go:412](../../internal/cli/apply.go#L412) (post-[02](02-split-cli-apply.md): [internal/cli/apply_setup.go](../../internal/cli/apply_setup.go)) calls `CompileWithOpts` without a `SubWorkflowResolver`. Compile-time references fail with "workflow_file requires SubWorkflowResolver in CompileOpts" ([workflow/compile_steps.go:358](../../workflow/compile_steps.go#L358) — moved to `compile_steps_workflow.go` by [03](03-split-compile-steps.md)). This workstream wires the resolver. (The legacy `workflow_file` step attribute is itself replaced by the `subworkflow` block — the resolver is what makes either work.)

## Prerequisites

- Every dependency above merged. In particular:
  - [08](08-schema-unification.md): sub-workflow IS a `Spec`.
  - [09](09-output-block.md): top-level `output` blocks exist.
  - [10](10-environment-block.md): `environment` declaration surface.
  - [11](11-agent-to-adapter-rename.md): `adapter` block.
  - [12](12-adapter-lifecycle-automation.md): scope-bound lifecycle.
- `make ci` green on `main`.

## In scope

### Step 1 — Schema

Add `SubworkflowSpec` and `SubworkflowNode`:

```go
type SubworkflowSpec struct {
    Name        string   `hcl:"name,label"`
    Source      string   `hcl:"source"`              // directory path; local or "scheme://host/path"
    Environment string   `hcl:"environment,optional"`// "<env_type>.<env_name>" reference
    Remain      hcl.Body `hcl:",remain"`             // captures the "input" map attribute
}

type SubworkflowNode struct {
    Name         string
    SourcePath   string                          // resolved absolute path
    Body         *FSMGraph                       // deep-compiled callee
    BodyEntry    string
    Environment  string                          // resolved "<env_type>.<env_name>"
    Inputs       map[string]hcl.Expression       // parent-scope expressions, evaluated at call site
    DeclaredVars map[string]cty.Type             // callee's required variable types (cached for input-bind validation)
}
```

In `Spec`, add `Subworkflows []SubworkflowSpec \`hcl:"subworkflow,block"\``.

In `FSMGraph`, add `Subworkflows map[string]*SubworkflowNode` and `SubworkflowOrder []string`.

Delete `StepSpec.WorkflowFile` (line 83) and `StepSpec.Workflow` (line 94 — already retyped to `*Spec` by [08](08-schema-unification.md)). The `step.workflow { ... }` inline form survives **only** as the inline-only path that [08](08-schema-unification.md) preserved for cases where a body doesn't deserve a separate file. The `subworkflow` block is the multi-file/cross-source case.

Wait — [08](08-schema-unification.md) explicitly added `step.workflow { input = ... }` as a stopgap. With this workstream, the stopgap is removed: any step that wants a sub-workflow declares the `subworkflow` block at top level and references it via `target` ([14-universal-step-target.md](14-universal-step-target.md)). The inline `step.workflow { }` form is **also removed**.

Update [08](08-schema-unification.md)'s reviewer notes (cannot edit other workstream files; instead, this workstream's reviewer notes record the rationale: the stopgap retired with this workstream).

So, in this workstream:

- Delete `StepSpec.Workflow *Spec` field.
- Delete `StepSpec.Input` field (the [08](08-schema-unification.md) stopgap).
- Delete `StepSpec.WorkflowFile string` field.
- Add hard parse-error rejection for any of those legacy attributes.

### Step 2 — `SubWorkflowResolver` interface (already exists; verify and extend)

The interface is referenced by [workflow/compile_steps.go:358](../../workflow/compile_steps.go#L358) and likely defined in [workflow/compile.go](../../workflow/compile.go) or similar. Read the existing definition.

If it's:

```go
type SubWorkflowResolver interface {
    Resolve(ctx context.Context, ref string) ([]byte, error)  // legacy: returns HCL bytes
}
```

Extend (or wrap) it for directory-based sources:

```go
type SubWorkflowResolver interface {
    // ResolveSource resolves a source string ("./path" or "scheme://...")
    // to a directory containing one or more .hcl files plus referenced fixtures.
    // For local paths, the returned dir is the absolute path; for remote sources,
    // the resolver fetches into a cache dir.
    ResolveSource(ctx context.Context, callerDir string, source string) (dir string, err error)
}
```

Document the callerDir resolution (relative paths resolve against the parent workflow's directory).

### Step 3 — Local-only resolver implementation

In `internal/cli/subwfresolve.go` (or a similar location):

```go
// LocalSubWorkflowResolver resolves source strings against the local
// filesystem only. Remote schemes (git://, https://, etc.) produce a
// "remote sources not supported in v0.3.0" error pointing at Phase 4.
type LocalSubWorkflowResolver struct {
    AllowedRoots []string  // optional: restrict resolution to roots; empty = no restriction
}

func (r *LocalSubWorkflowResolver) ResolveSource(ctx context.Context, callerDir, source string) (string, error)
```

Behavior:

1. If `source` parses as a URL with a scheme other than empty/`file`, error with the Phase 4 forward-pointer.
2. If `source` is absolute, use it directly. Reject if `AllowedRoots` is non-empty and the path is not under any allowed root (security guard).
3. If `source` is relative, resolve against `callerDir`.
4. Verify the resolved path is a directory (not a file); error if not.
5. Verify the directory contains at least one `.hcl` file; error if empty.
6. Return the absolute path.

`AllowedRoots` is optional; the CLI populates it from a `--subworkflow-root` flag (repeatable) or a config file. v0.3.0 default: no roots configured, no restriction. Phase 4 may tighten.

### Step 4 — Wire the resolver into the CLI compile path

In [internal/cli/apply_setup.go](../../internal/cli/apply_setup.go) (post-[02](02-split-cli-apply.md)), `compileForExecution`:

```go
// BEFORE
graph, diags := workflow.CompileWithOpts(spec, schemas, workflow.CompileOpts{
    WorkflowDir: filepath.Dir(workflowPath),
})

// AFTER
resolver := &cli.LocalSubWorkflowResolver{}  // AllowedRoots from --subworkflow-root flag if set
graph, diags := workflow.CompileWithOpts(spec, schemas, workflow.CompileOpts{
    WorkflowDir:           filepath.Dir(workflowPath),
    SubWorkflowResolver:   resolver,
})
```

Add a CLI flag `--subworkflow-root <path>` (repeatable) that populates `AllowedRoots`. Default: empty.

### Step 5 — Compile pass

New file `workflow/compile_subworkflows.go`:

```go
// compileSubworkflows resolves each subworkflow.source via opts.SubWorkflowResolver,
// reads + parses every .hcl file in the resolved directory ([17] does the merge),
// recursively compiles the callee Spec into a child FSMGraph, validates the
// input bindings against the callee's declared variables, and stores the result
// in g.Subworkflows. Cycle detection on the source DAG is enforced via opts.SubworkflowChain.
func compileSubworkflows(g *FSMGraph, spec *Spec, opts CompileOpts) hcl.Diagnostics
```

`CompileOpts` already exists; extend with:

```go
type CompileOpts struct {
    ...
    SubWorkflowResolver SubWorkflowResolver
    SubworkflowChain    []string  // resolved source paths in the current call stack — for cycle detection
}
```

Algorithm:

1. For each `SubworkflowSpec`, validate the name (unique, identifier shape).
2. Resolve `source` via `opts.SubWorkflowResolver.ResolveSource(ctx, opts.WorkflowDir, source)`.
3. Cycle check: if the resolved path is already in `opts.SubworkflowChain`, error with the chain printed.
4. Read every `.hcl` file in the resolved dir. (Until [17-directory-module-compile.md](17-directory-module-compile.md) lands, the multi-file merge is local to this workstream — implement the simple merge here, and [17](17-directory-module-compile.md) generalizes it. Specifically: parse each file as a `Spec`, merge the slices field-by-field, error on cross-file duplicates.)
5. Recursively `CompileWithOpts(calleeSpec, schemas, opts')` where `opts'.SubworkflowChain` is the parent chain plus the resolved path. The recursion is bounded by cycle detection.
6. Extract declared variable types from the compiled callee for input-bind validation.
7. Validate the parent-scope `input = { ... }` map: every required callee variable (no default) must have a key; extra keys produce an error.
8. Store `SubworkflowNode` in `g.Subworkflows`.

### Step 6 — Runtime: invoke a subworkflow

Subworkflow invocation comes from a step targeting `subworkflow.<name>`. The step-target wiring is [14-universal-step-target.md](14-universal-step-target.md)'s job; this workstream provides the **runtime entry point** that [14](14-universal-step-target.md) calls into.

In `internal/engine/node_subworkflow.go` (new file, sibling to `node_workflow.go`):

```go
// runSubworkflow invokes a declared subworkflow synchronously, with its own
// scoped Vars, Adapters (per [12]), and execution lifetime. The parent step's
// input expression is evaluated against parent state and bound to the callee's
// variables; the callee's output values are returned to the parent.
func runSubworkflow(ctx context.Context, sw *workflow.SubworkflowNode, parentSt *RunState, deps Deps) (map[string]cty.Value, error)
```

Implementation:

1. Evaluate `sw.Inputs` expressions against `parentSt`'s eval context.
2. Build `childSt` with `Vars` seeded from the bound input map (no parent aliasing).
3. Invoke `initScopeAdapters` for the callee's `g.Adapters` (per [12](12-adapter-lifecycle-automation.md)).
4. Run the callee to terminal state via the engine's standard run loop (refactor: extract the run-loop body so it can be invoked recursively without duplicating the top-level loop).
5. Evaluate the callee's `g.Outputs` per [09](09-output-block.md).
6. `tearDownScopeAdapters`.
7. Return the output map to the parent.

The recursive run-loop refactor is **non-optional** — without it, this workstream duplicates the run loop for sub-workflows. Invest in the refactor; document in reviewer notes.

### Step 7 — Output-namespace exposure

Add `subworkflow.<name>.output.<output_name>` to the runtime evaluation context. In [workflow/eval.go](../../workflow/eval.go)'s `BuildEvalContextWithOpts`:

```go
// Build a "subworkflow" object whose keys are sub-workflow names and whose
// values are objects with one key, "output", which itself is an object of
// resolved output values.
subworkflowVal := buildSubworkflowOutputs(rs.SubworkflowOutputs)
ctx.Variables["subworkflow"] = subworkflowVal
```

`rs.SubworkflowOutputs map[string]map[string]cty.Value` is populated by `runSubworkflow` after each successful invocation. Subsequent steps in the parent can read `subworkflow.review_loop.output.result_count`.

### Step 8 — Examples

- New: `examples/phase3-subworkflow/` with a parent workflow plus `subworkflows/inner/main.hcl` (and a multi-file `subworkflows/multi/{vars,steps}.hcl` to demonstrate [17](17-directory-module-compile.md)'s merge — though [17](17-directory-module-compile.md) is the proper home for the multi-file generalization).
- Update [docs/workflow.md](../../docs/workflow.md) with a Subworkflows section.
- Restore the previously-deferred [examples/workflow_step_compose.hcl](../../examples/) (mentioned as deferred in Phase 2). Rewrite it under the new `subworkflow` block shape.

### Step 9 — Tests

- `workflow/compile_subworkflows_test.go`:
  - `TestCompileSubworkflows_Basic`.
  - `TestCompileSubworkflows_RelativeSource`.
  - `TestCompileSubworkflows_AbsoluteSource`.
  - `TestCompileSubworkflows_RemoteScheme_Errors` — Phase 4 forward-pointer.
  - `TestCompileSubworkflows_DirNotExist` — error.
  - `TestCompileSubworkflows_DirEmptyOfHCL` — error.
  - `TestCompileSubworkflows_Cycle_Direct` — A → A.
  - `TestCompileSubworkflows_Cycle_Indirect` — A → B → A.
  - `TestCompileSubworkflows_InputMissingRequiredVar` — error.
  - `TestCompileSubworkflows_InputExtraKey` — error.
  - `TestCompileSubworkflows_InputTypeMismatch` — error.
  - `TestCompileSubworkflows_DeclaredEnvironmentResolves`.

- `internal/cli/subwfresolve_test.go`:
  - `TestLocalResolver_LocalRelative`.
  - `TestLocalResolver_LocalAbsolute`.
  - `TestLocalResolver_RemoteScheme_Error`.
  - `TestLocalResolver_AllowedRootsRestriction`.
  - `TestLocalResolver_NotADirectory_Error`.

- `internal/engine/node_subworkflow_test.go`:
  - `TestRunSubworkflow_HappyPath`.
  - `TestRunSubworkflow_OutputsAccessibleFromParent`.
  - `TestRunSubworkflow_AdaptersIsolatedFromParent`.
  - `TestRunSubworkflow_ErrorPropagatesToParent`.
  - `TestRunSubworkflow_CalleeCancellation`.

- End-to-end: `examples/phase3-subworkflow/` runs and the parent observes the callee's outputs.

### Step 10 — Validation

```sh
go build ./...
go test -race -count=2 ./...
make validate
make proto-check-drift
make test-conformance
make ci
```

All exit 0.

## Behavior change

**Behavior change: yes — additive at the language surface; replaces the deleted inline + `workflow_file` paths.**

Observable differences:

1. New top-level `subworkflow "<name>" { source = ..., environment = ..., input = {...} }` block.
2. New `subworkflow.<name>.output.<output_name>` namespace at runtime.
3. New CLI flag `--subworkflow-root <path>` (repeatable).
4. The legacy `step.workflow { ... }` inline body and `step.workflow_file = ...` attribute are **removed** (hard parse error).
5. Cycle detection on subworkflow sources.
6. Cross-source compile errors include the resolved file path.

Migration:

- Inline-body workflows must be extracted to a separate directory and referenced via `subworkflow`. The migration burden is real but the new shape is what [08](08-schema-unification.md) prepared the way for.
- `workflow_file = "x.hcl"` → declare `subworkflow "x" { source = "./x" }` where `./x/` is a directory containing `x.hcl`.

No proto change. No SDK conformance change beyond a new "subworkflows execute" assertion.

## Reuse

- Existing `SubWorkflowResolver` interface scaffolding in [workflow/compile.go](../../workflow/compile.go) — extend, do not rewrite.
- The recursive `CompileWithOpts` invocation pattern — already used internally for body compile via [`compileWorkflowBodyFromFile`](../../workflow/compile_steps.go#L350).
- [08-schema-unification.md](08-schema-unification.md)'s "sub-workflow IS a Spec" guarantee.
- [09](09-output-block.md)'s `OutputNode` shape for cross-scope output projection.
- [12](12-adapter-lifecycle-automation.md)'s `initScopeAdapters` / `tearDownScopeAdapters` per-scope hooks.
- [`runWorkflowBody`](../../internal/engine/node_workflow.go) shape — refactor to share a recursive run-loop helper.

## Out of scope

- Remote source schemes (`git://`, `https://`). Phase 4.
- Caching of resolved subworkflow content. v0.3.0 reads source on every compile.
- Multi-file merge across `.hcl` files in a directory. **Local minimum** lands here so subworkflows of one file work; the **generalization** is [17-directory-module-compile.md](17-directory-module-compile.md). Coordinate with [17](17-directory-module-compile.md) executor: this workstream's merge implementation is local to `compileSubworkflows`; [17](17-directory-module-compile.md) extracts and generalizes.
- The universal step `target = subworkflow.<name>` attribute. Owned by [14-universal-step-target.md](14-universal-step-target.md). Until [14](14-universal-step-target.md) lands, `subworkflow` blocks are declared but not invokable from a step. **Decision:** that's acceptable — [14](14-universal-step-target.md) is in the same Phase 3 batch and lands shortly after.

## Files this workstream may modify

- [`workflow/schema.go`](../../workflow/schema.go) — add `SubworkflowSpec`, `SubworkflowNode`, `Spec.Subworkflows`, `FSMGraph.Subworkflows`, `FSMGraph.SubworkflowOrder`. Delete `StepSpec.Workflow`, `StepSpec.WorkflowFile`, `StepSpec.Input` (the [08](08-schema-unification.md) stopgap).
- New: `workflow/compile_subworkflows.go`.
- [`workflow/compile.go`](../../workflow/compile.go) — extend `CompileOpts` with `SubworkflowChain`; invoke `compileSubworkflows` after `compileEnvironments` and before `compileSteps`.
- New: `internal/cli/subwfresolve.go`.
- [`internal/cli/apply_setup.go`](../../internal/cli/apply_setup.go) — wire the resolver.
- New CLI flag in [`internal/cli/`](../../internal/cli/) — `--subworkflow-root`.
- New: `internal/engine/node_subworkflow.go`.
- [`internal/engine/engine.go`](../../internal/engine/engine.go) (or run.go) — extract reusable run-loop helper.
- [`workflow/eval.go`](../../workflow/eval.go) — add `subworkflow` namespace to eval context.
- `workflow/parse_legacy_reject.go` — extend with rejection for `workflow_file`, inline `workflow {}` block on a step, and the [08](08-schema-unification.md) stopgap `input` attribute on a step.
- New: `examples/phase3-subworkflow/` and rewritten `examples/workflow_step_compose.hcl`.
- Goldens under [`internal/cli/testdata/`](../../internal/cli/testdata/).
- [`docs/workflow.md`](../../docs/workflow.md) — Subworkflows section.

This workstream may **not** edit:

- `PLAN.md`, `README.md`, `AGENTS.md`, `CHANGELOG.md`, `workstreams/README.md`, or any other workstream file.
- `.proto` files.
- The base `SubWorkflowResolver` interface's existing public methods if they survive — extend, do not break.

## Tasks

- [ ] Add schema types (Step 1).
- [ ] Extend `SubWorkflowResolver` for directory sources (Step 2).
- [ ] Implement `LocalSubWorkflowResolver` (Step 3).
- [ ] Wire the resolver into the CLI compile path; add `--subworkflow-root` flag (Step 4).
- [ ] Implement `compileSubworkflows` with cycle detection (Step 5).
- [ ] Implement runtime `runSubworkflow`; extract run-loop helper (Step 6).
- [ ] Add `subworkflow` namespace to eval context (Step 7).
- [ ] Add legacy parse rejection for inline workflow / workflow_file / step.input stopgap.
- [ ] Add examples; update docs (Step 8).
- [ ] Author all required tests (Step 9).
- [ ] `make ci` green; example runs end-to-end (Step 10).

## Exit criteria

- `subworkflow "<name>" { source = ..., environment = ..., input = {...} }` parses, compiles deeply, and is invokable.
- Cycle detection catches direct and indirect cycles.
- `subworkflow.<name>.output.<key>` resolves at runtime in the parent scope.
- CLI passes a non-nil `SubWorkflowResolver` to `CompileWithOpts`.
- `--subworkflow-root` flag works.
- Inline `step.workflow { }` and `step.workflow_file = ...` produce hard parse errors with migration messages.
- All required tests pass.
- `examples/phase3-subworkflow/` runs end-to-end.
- `make ci` exits 0.

## Tests

The Step 9 list is the deliverable. Coverage targets:

- `workflow/compile_subworkflows.go` ≥ 90%.
- `internal/cli/subwfresolve.go` ≥ 90%.
- `internal/engine/node_subworkflow.go` ≥ 85%.

## Risks

| Risk | Mitigation |
|---|---|
| Recursive compile + cycle detection has subtle interactions with deeply-nested subworkflows | The chain is a slice; cycle = membership check. Test depths up to 5 with branching; test direct + indirect cycles. The failure mode is a stack overflow if the recursion is unbounded — the cycle check must run BEFORE recursion. |
| Resolving sources synchronously at compile time blocks on a slow filesystem | The resolver returns errors fast on missing dirs. For local FS, latency is bounded. Remote schemes are out of scope. |
| Cross-source error messages don't show the path the error came from | Every diagnostic from a recursively-compiled callee must have its `Subject.Filename` prefixed by the resolved source path. Add `TestCompileSubworkflows_DiagnosticPath`. |
| Run-loop extraction touches the engine's hot path and risks regressions | The extraction is a refactor: same behavior, function-shape change. Run `-race -count=20` on engine tests; cross-check `make bench` for the engine baseline. |
| The subworkflow namespace in eval context conflicts with a user variable named "subworkflow" | `subworkflow` is now a reserved namespace (like `var`, `local`, `each`, `steps`). A workflow declaring `variable "subworkflow"` errors at compile. Document. |
| Multi-file merge implemented locally diverges from [17](17-directory-module-compile.md)'s generalization | Implement the local merge as a private helper `mergeSpecsFromDir` callable from both this workstream's `compileSubworkflows` and [17](17-directory-module-compile.md)'s top-level entry. Coordinate the contract via reviewer notes. |
| `examples/workflow_step_compose.hcl` regresses or is hard to express in the new shape | If it can't be expressed cleanly under `subworkflow`, replace it with a fresh `examples/phase3-subworkflow/compose.hcl`. The example's role is illustrative; preserve the intent, not the file. |
