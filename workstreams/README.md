# Criteria workstreams

The active phase's workstream files live at the top of this directory;
prior phases are in [`archived/`](archived/).

## Status

- **Phase 0** — post-separation cleanup — **closed 2026-04-27**. All nine
  workstreams merged; `v0.1.0` tagged. Archived under [`archived/v0/`](archived/v0/).
- **Phase 1** — TBD. See [PLAN.md](../PLAN.md) for details.

See [PLAN.md](../PLAN.md) for the project-level roadmap.

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
gate (W09), or (b) defers the edit to W09 with a forward-pointer note
in its reviewer log. W08's brand-rename sweep is the documented
exception: it makes mechanical rebrand edits across the
coordination-set files but does not touch their structure.

## Archived

Phase 0 is archived under [`archived/v0/`](archived/v0/). The pre-separation
v1.x phases live in the orchestrator repo's `workstreams/archived/`; they are
not copied here.
