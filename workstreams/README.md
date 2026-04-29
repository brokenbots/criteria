# Criteria workstreams

The active phase's workstream files live at the top of this directory;
prior phases are in [`archived/`](archived/).

## Status

- **Phase 0** — post-separation cleanup — **closed 2026-04-27**. All nine
  workstreams merged; `v0.1.0` tagged. Archived under [`archived/v0/`](archived/v0/).
- **Phase 1** — stabilization + critical user fixes — **closed 2026-04-29**.
  All eleven workstreams merged; lint baseline burn-down gate clean; `v0.2.0` tagged.
  Archived under [`archived/v1/`](archived/v1/).
- **Phase 2** — TBD. See [PLAN.md](../PLAN.md) "Deferred / forward-pointers" for the candidate scope list.

See [PLAN.md](../PLAN.md) for the project-level roadmap.

## Phase 2 workstreams (TBD)

No workstream files yet. See [PLAN.md](../PLAN.md) "Deferred / forward-pointers" for the candidate scope list.

## Phase 1 workstreams (archived)

All Phase 1 workstream files have been moved to [`archived/v1/`](archived/v1/).

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
"Files this workstream may modify" list and must be the cleanup gate
for that phase, or it defers the edit to the cleanup gate with a
forward-pointer note in its reviewer log.

## Archived

Phase 0 is archived under [`archived/v0/`](archived/v0/).
Phase 1 is archived under [`archived/v1/`](archived/v1/).
The pre-separation v1.x phases live in the orchestrator repo's `workstreams/archived/`; they are
not copied here.
