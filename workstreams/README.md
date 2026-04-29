# Criteria workstreams

The active phase's workstream files live at the top of this directory;
prior phases are in [`archived/`](archived/).

## Status

- **Phase 0** — post-separation cleanup — **closed 2026-04-27**. All nine
  workstreams merged; `v0.1.0` tagged. Archived under [`archived/v0/`](archived/v0/).
- **Phase 1** — stabilization + critical user fixes — **in progress**.
  Workstreams W01–W10 active; [W11](11-phase1-cleanup-gate.md) is the
  cleanup gate that closes the phase and tags `v0.2.0`. See
  [PLAN.md](../PLAN.md) for details.

See [PLAN.md](../PLAN.md) for the project-level roadmap.

## Phase 1 workstreams (active)

- [W01](01-flaky-test-fix.md) — flaky test fix (deterministic CI).
- [W02](02-golangci-lint-adoption.md) — golangci-lint adoption + per-workstream baseline burn-down contract.
- [W03](03-god-function-refactor.md) — god-function refactor.
- [W04](04-split-oversized-files.md) — file-size splits.
- [W05](05-shell-adapter-sandbox.md) — shell adapter sandbox + threat model.
- [W06](06-coverage-bench-godoc.md) — coverage thresholds, benchmarks, GoDoc.
- [W07](07-file-expression-function.md) — `file()` / `fileexists()` / `trimfrontmatter()` HCL functions.
- [W08](08-for-each-multistep.md) — multi-step `for_each` iteration bodies *(superseded by [W10](10-step-iteration-and-workflow-step.md); the runtime model is replaced)*.
- [W09](09-copilot-agent-defaults.md) — Copilot agent defaults + targeted diagnostics.
- [W10](10-step-iteration-and-workflow-step.md) — step-level `for_each` / `count`; new `type = "workflow"` step with inline or `workflow_file` body; indexed outputs; `each.*` extras (`_first`, `_last`, `_total`, `_prev`); `on_failure` modes. Removes W08's top-level `for_each` block.
- [W11](11-phase1-cleanup-gate.md) — Phase 1 cleanup gate (the only workstream that may edit the coordination set).

## Phase 0 workstreams (archived)

All Phase 0 workstream files have been moved to [`archived/v0/`](archived/v0/).

## Files NOT editable by workstream-executor or workstream-reviewer

The executor and reviewer agents are scoped to **the single workstream
file they are executing**. They may not edit:

- `README.md`
- `PLAN.md`
- `AGENTS.md`
- `workstreams/README.md`
- Any other workstream file in this directory

A workstream that needs changes to those files declares them in its
"Files this workstream may modify" list and either (a) is the cleanup
gate ([W11](11-phase1-cleanup-gate.md) for Phase 1; was W09 in Phase
0), or (b) defers the edit to the cleanup gate with a forward-pointer
note in its reviewer log. [W10](10-step-iteration-and-workflow-step.md)
is the documented Phase 1 exception: its Step 11 makes targeted edits
to `workstreams/README.md` and `PLAN.md` to register the new
workstream and the W10/W11 renumber. Phase 0's W08 brand-rename sweep
was the equivalent exception in that phase.

## Archived

Phase 0 is archived under [`archived/v0/`](archived/v0/). The pre-separation
v1.x phases live in the orchestrator repo's `workstreams/archived/`; they are
not copied here.
