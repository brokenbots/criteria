# Workstream 5 — Wire `SubWorkflowResolver` into the CLI compile path

> **Status: CANCELLED (2026-04-30).**
> This workstream has been removed from Phase 2 scope. Phase 2 priorities
> were re-aligned to land tool-call outcome finalization for the Copilot
> adapter (new [W14](14-copilot-tool-call-wire-contract.md) and
> [W15](15-copilot-submit-outcome-adapter.md)) ahead of `workflow_file`
> resolver wiring. The `workflow_file` runtime gap remains a forward-pointer
> in [PLAN.md](../PLAN.md) and is a candidate for Phase 3.
>
> **Do not execute this workstream.** The historical scope is preserved
> below for context only. The cleanup gate (now [W16](16-phase2-cleanup-gate.md))
> drops the example-validation step that depended on this work.

---

**Owner:** Workstream executor · **Depends on:** none · **Unblocks:** [W16](16-phase2-cleanup-gate.md) (cleanup gate verifies the example workflow runs).

## Context

Phase 1 [W10](archived/v1/10-step-iteration-and-workflow-step.md)
shipped the `type = "workflow"` step type with two body modes:
inline (`workflow { ... }`) and external file
(`workflow_file = "..."`). The schema-level support is complete —
[workflow/compile_steps.go:340](../workflow/compile_steps.go#L340)
calls `opts.SubWorkflowResolver(sp.WorkflowFile, opts.WorkflowDir)`
when the file path is set.

The CLI never passes a resolver. The compile call at
[internal/cli/apply.go:350](../internal/cli/apply.go#L350) constructs
`workflow.CompileOpts{WorkflowDir: filepath.Dir(workflowPath)}` with
`SubWorkflowResolver` left nil. Any workflow that uses
`workflow_file = "..."` therefore fails compile with the diagnostic:

> `step "X": workflow_file requires SubWorkflowResolver in CompileOpts`

This is the "W10 partial" gap called out in the v0.2.0 tech evaluation
([tech_evaluations/TECH_EVALUATION-20260429-01.md](../tech_evaluations/TECH_EVALUATION-20260429-01.md)
section 6 item 5). The example workflow
`examples/workflow_step_compose.hcl` was deferred specifically
because the resolver is not wired.

This workstream adds the wiring. There are two `SubWorkflowResolver`
concepts in the codebase — they are not the same:

1. **Compile-time:**
   `workflow.CompileOpts.SubWorkflowResolver func(filePath, workflowDir string) (*Spec, error)`
   ([workflow/compile.go:42](../workflow/compile.go#L42)). Called from
   `compileWorkflowBodyFromFile` to load and parse the referenced HCL
   file.
2. **Runtime:** `engine.SubWorkflowResolver` interface
   ([internal/engine/extensions.go:118](../internal/engine/extensions.go#L118))
   with `Resolve(ctx, callerPath, targetPath string) (*workflow.FSMGraph, error)`.
   Documented as "Implemented in Phase 1.6"; the engine path may not
   actually need a runtime resolver if compile-time resolution is
   sufficient (the compiled FSM already inlines the sub-graph).

This workstream wires the **compile-time** resolver, which is what
the schema needs. The runtime resolver is a separate concern; we
verify it is not actually called for the `workflow_file` path before
deciding whether to wire it. If runtime resolution is required (e.g.
for late-binding or hot-reload), expand scope; otherwise leave it
deferred with a clear note.

## Prerequisites

- `make ci` green on `main`.
- Familiarity with the W10 design doc:
  [workstreams/archived/v1/10-step-iteration-and-workflow-step.md](archived/v1/10-step-iteration-and-workflow-step.md).
- Read the existing test fixture for resolver wiring:
  [workflow/iteration_compile_test.go:495](../workflow/iteration_compile_test.go#L495)
  shows the pattern.

## In scope

### Step 1 — Implement the filesystem resolver

Add a new file
`internal/cli/subworkflow_resolver.go` with a function:

```go
// FilesystemSubWorkflowResolver returns a workflow.CompileOpts
// SubWorkflowResolver that resolves workflow_file references against
// the local filesystem. Paths are treated as relative to workflowDir
// unless they are absolute.
//
// The resolver:
//   - rejects absolute paths that escape workflowDir if
//     CRITERIA_WORKFLOW_ALLOWED_PATHS does not whitelist them
//     (mirrors the file() HCL function's confinement).
//   - rejects symlinks that resolve outside the allowed roots.
//   - parses the HCL file via workflow.ParseFile.
//   - does not cache; the compile_steps.go cycle detector handles
//     re-entry; caching is a future optimization.
func FilesystemSubWorkflowResolver(workflowDir string) func(filePath, callerWorkflowDir string) (*workflow.Spec, error) {
    return func(filePath, callerWorkflowDir string) (*workflow.Spec, error) {
        // Resolve filePath relative to callerWorkflowDir (which is
        // the dir of the file currently being compiled, not
        // necessarily the top-level workflowDir).
        // Validate against CRITERIA_WORKFLOW_ALLOWED_PATHS using the
        // existing helper from internal/cli/file_paths.go (or
        // wherever the file() function's confinement lives).
        // Read and parse the HCL.
        // Return the *workflow.Spec.
    }
}
```

Notes:

- Reuse the existing path-confinement helper used by the `file()` HCL
  function (Phase 1 W07). Locate via grep for
  `CRITERIA_WORKFLOW_ALLOWED_PATHS`. Do not duplicate the logic.
- The signature of `workflow.CompileOpts.SubWorkflowResolver` is
  `func(filePath, workflowDir string) (*Spec, error)` — note the
  *second* arg is `workflowDir` of the caller (per
  `workflow/compile_steps.go:347` it's `opts.WorkflowDir` of the
  outer compile). The resolver must support nested loads where each
  child's `workflowDir` is the directory of the parent file.
- Parsing: use the existing parser entry point in `workflow/`.
  Inspect `workflow/parse.go` (or equivalent) for the function name
  — likely `workflow.ParseFile(path string) (*Spec, error)` or
  `workflow.ParseHCL(...)`. Reuse it; do not duplicate HCL parsing.

### Step 2 — Wire the resolver into all CLI compile call sites

Update [internal/cli/apply.go:350](../internal/cli/apply.go#L350):

```go
workflowDir := filepath.Dir(workflowPath)
graph, diags := workflow.CompileWithOpts(spec, schemas, workflow.CompileOpts{
    WorkflowDir:         workflowDir,
    SubWorkflowResolver: FilesystemSubWorkflowResolver(workflowDir),
})
```

Audit `internal/cli/` for every call to `workflow.Compile` /
`workflow.CompileWithOpts`. Likely candidates:

- `internal/cli/apply.go` (multiple call sites — search for
  `CompileWithOpts`).
- `internal/cli/validate.go` (the `criteria validate` command).
- `internal/cli/compile.go` (the `criteria compile` command).
- `internal/cli/plan.go` (the `criteria plan` command).

Every site that takes a workflow path must wire the resolver. A
helper `compileWithFilesystemResolver(spec, schemas, workflowPath)`
in `apply.go` (or a new `compile_helpers.go`) is acceptable to avoid
repeating the four-line construction.

### Step 3 — Validate local-mode safety

[internal/cli/apply.go:359-389](../internal/cli/apply.go#L359-L389)
contains `ensureLocalModeSupported()` which rejects workflows
containing approval / signal-wait nodes when no orchestrator is
configured. After resolving sub-workflows, the compiled `FSMGraph`
contains the union of all node kinds across the parent and children.
Confirm that `ensureLocalModeSupported` runs *after*
`CompileWithOpts` and operates on the fully-resolved graph; if not,
move the check.

If a sub-workflow uses an `approval` node, the parent run must reject
in local mode just like a top-level approval would (until
[W06](06-local-mode-approval.md) lands its local-mode fallback).
After [W06](06-local-mode-approval.md) merges, the
local-mode-supported check loosens accordingly. Coordinate with W06
on ordering — if W06 lands first, this workstream just inherits the
new behavior; if this lands first, the existing reject-on-approval
semantics propagates correctly through nested workflows because the
graph is unioned.

### Step 4 — Land the deferred example

Author `examples/workflow_step_compose.hcl` per the W10 design
([archived/v1/10-step-iteration-and-workflow-step.md](archived/v1/10-step-iteration-and-workflow-step.md)).
Plus a referenced sub-workflow file (e.g.
`examples/workflows/sub_review.hcl`).

Constraints:

- The example must validate cleanly via `criteria validate`.
- It must run end-to-end via `criteria apply
  examples/workflow_step_compose.hcl` (no `--server`) given any
  prerequisites the example documents in its header comment.
- It should demonstrate `each.*` binding propagation, `output`
  blocks, and at least one `transition_to` from a sub-workflow
  outcome to a parent step.
- Keep it simple — illustrate the mechanism, not the full feature
  matrix. Three to five steps total across parent + child is plenty.

Add it to `make validate`'s implicit glob (already covers
`examples/*.hcl`).

### Step 5 — Decide on the runtime `engine.SubWorkflowResolver`

Inspect `internal/engine/node_workflow.go` and confirm whether the
runtime path actually invokes the engine-level
`SubWorkflowResolver`. If it does not (i.e. the compile-time
resolver inlines the sub-graph and the engine just walks it), leave
the runtime interface unchanged but document this in
`internal/engine/extensions.go` with a code comment that says "the
runtime resolver is reserved for late-binding scenarios; current
`workflow_file` compile-time resolution does not need it."

If the runtime path *does* invoke it, add the same filesystem
resolver wired to `engine.WithSubWorkflowResolver(...)` in
`apply.go:141`, `:217`, `:257`, and `:447` (every `engine.New`
call site). The implementation can wrap `FilesystemSubWorkflowResolver`
to satisfy the engine's interface.

The decision (no runtime wiring needed vs. runtime wiring required)
must be documented in reviewer notes with the file:line evidence
that supports it.

### Step 6 — Tests

Add tests:

- `internal/cli/subworkflow_resolver_test.go`:
  - Resolves a sibling file relative to workflowDir.
  - Resolves a file in a subdirectory.
  - Rejects a path outside `CRITERIA_WORKFLOW_ALLOWED_PATHS`.
  - Returns a clear error for a missing file.
  - Detects load cycles via the existing `LoadedFiles` mechanism in
    `workflow.CompileOpts` (the existing test
    [workflow/iteration_compile_test.go:445](../workflow/iteration_compile_test.go#L445)
    is the canonical reference; add a CLI-level integration test that
    exercises the same cycle through the resolver).
- An `examples/workflow_step_compose.hcl` validation test (extends
  whatever example-validation harness exists; check
  `internal/cli/validate_test.go` for the pattern).

### Step 7 — Documentation

Update [docs/workflow.md](../docs/workflow.md):

- Document `workflow_file` resolution: paths relative to the parent
  workflow's directory, confinement via
  `CRITERIA_WORKFLOW_ALLOWED_PATHS`, no caching, cycle detection.
- Reference `examples/workflow_step_compose.hcl` as the canonical
  example.

Do **not** edit `README.md`, `PLAN.md`, `AGENTS.md`, `CHANGELOG.md`.

## Behavior change

**Yes — feature completion.**

- Workflows with `step ... { type = "workflow" workflow_file = "..." }`
  now compile and run instead of erroring out.
- The runtime path's behavior is unchanged unless Step 5 finds it
  needs wiring (in which case it gains the same resolver semantics).
- Local-mode rejection of approval / signal-wait nodes propagates
  through nested workflows.
- New error paths: missing file, path outside allowed roots, parse
  errors in the loaded file, load cycle. Each error includes the
  outer step name and the offending path.

## Reuse

- `workflow.CompileOpts.SubWorkflowResolver` — already defined; do
  not redefine.
- `compileWorkflowBodyFromFile` /
  `compileWorkflowBodyInline` — already implement the schema-side
  loading logic.
- The `file()` HCL function's path-confinement helper (Phase 1
  [W07](archived/v1/07-file-expression-function.md)). Locate via
  grep for `CRITERIA_WORKFLOW_ALLOWED_PATHS`. Reuse the helper.
- The HCL parser entry point in `workflow/` (locate before
  reimplementing).
- Existing `LoadedFiles` cycle-detection list in `CompileOpts`.

## Out of scope

- Caching resolved sub-workflows. The cycle detector handles re-entry;
  performance optimization belongs in a later workstream if benchmarks
  demand it.
- Late-binding (loading sub-workflows at run-time, not compile time).
  The engine-level `SubWorkflowResolver` interface is reserved for
  this; this workstream does not add late-binding semantics.
- Multi-workflow chaining (`workflow_sequence` step type). That is a
  Phase 3 candidate.
- Modifying the `workflow_file` schema. The schema is fixed.
- Rewriting the `file()` HCL function's path confinement. Reuse it.

## Files this workstream may modify

- `internal/cli/subworkflow_resolver.go` (new).
- `internal/cli/subworkflow_resolver_test.go` (new).
- `internal/cli/apply.go` (the `:350` compile call + any other
  `CompileWithOpts` call sites in this file).
- `internal/cli/validate.go` (compile call).
- `internal/cli/compile.go` (compile call).
- `internal/cli/plan.go` (compile call).
- `internal/engine/extensions.go` (only a code comment if Step 5
  decides runtime wiring is not needed).
- `examples/workflow_step_compose.hcl` (new).
- `examples/workflows/sub_review.hcl` (new — sibling sub-workflow).
- `docs/workflow.md` (documentation).

This workstream may **not** edit `README.md`, `PLAN.md`, `AGENTS.md`,
`CHANGELOG.md`, `workstreams/README.md`, or any other workstream file.
It may **not** modify the `workflow.CompileOpts` struct shape or the
`engine.SubWorkflowResolver` interface signature.

## Tasks

- [ ] Implement `FilesystemSubWorkflowResolver` in
      `internal/cli/subworkflow_resolver.go`.
- [ ] Wire it into every `workflow.CompileWithOpts` call site in
      `internal/cli/`.
- [ ] Verify `ensureLocalModeSupported` runs on the fully-resolved
      graph; move it if not.
- [ ] Author `examples/workflow_step_compose.hcl` and the referenced
      sub-workflow.
- [ ] Decide on runtime resolver wiring (Step 5); document choice.
- [ ] Add unit tests for the resolver and a validation test for the
      example.
- [ ] Update `docs/workflow.md`.
- [ ] `make build`, `make plugins`, `make test`, `make validate`,
      `make ci` all green.

## Exit criteria

- `criteria validate examples/workflow_step_compose.hcl` exits 0.
- `criteria apply examples/workflow_step_compose.hcl` (no `--server`)
  exits 0 — assuming the example does not include approval / signal
  waits (it should not for this verification; coordinate with W06
  to add such an example after both workstreams land).
- `make validate` includes the new example.
- All unit tests in `internal/cli/subworkflow_resolver_test.go` pass.
- `make ci` green.
- The runtime-resolver decision is documented in reviewer notes.

## Tests

- `TestFilesystemSubWorkflowResolver_Sibling` — relative file in same
  dir.
- `TestFilesystemSubWorkflowResolver_Subdir` — relative file in a
  child dir.
- `TestFilesystemSubWorkflowResolver_OutsideAllowed` — path outside
  the allowed roots is rejected.
- `TestFilesystemSubWorkflowResolver_Missing` — clear error message.
- `TestFilesystemSubWorkflowResolver_Cycle` — load cycle detected
  via the compile_steps.go mechanism (extends to two-deep cycle).
- `TestExampleWorkflowStepCompose_Validates` — the new example
  passes `criteria validate`.

## Risks

| Risk | Mitigation |
|---|---|
| Reusing the file() function's path-confinement helper turns out to be impossible (helper is private to a different package) | Lift the helper to `internal/cli/paths.go` (or wherever it logically belongs) as a small refactor. Keep the change minimal and add a code comment. |
| The HCL parser entry point exposed by `workflow/` is not stable | Pin the call to the existing public function used by the rest of the CLI. If no public function exists, the CLI is already calling something — reuse that exact path. |
| The runtime resolver path *is* invoked and Step 5 expands the workstream significantly | Spend up to 1 day analyzing. If the runtime wiring is non-trivial, file a follow-up workstream and ship the compile-time wiring alone — the example workflow still works because the FSMGraph is fully inlined at compile time. |
| Local-mode rejection of approval / wait inside nested workflows surprises operators | Document explicitly in `docs/workflow.md`. After [W06](06-local-mode-approval.md) lands its local fallback, the rejection loosens and the docs update accordingly. |
| Cycle detection misses a multi-hop cycle | The existing `LoadedFiles` list is appended on every recursion (see `compile_steps.go:350`); the cycle test should include a 3-file chain. |
