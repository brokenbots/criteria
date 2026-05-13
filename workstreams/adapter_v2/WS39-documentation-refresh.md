# WS39 — Documentation refresh (cleanup gate)

**Phase:** Adapter v2 · **Track:** Release gate · **Owner:** Workstream executor (cleanup-gate role: only WS allowed to edit `README.md`, `PLAN.md`, `AGENTS.md`, `CHANGELOG.md`, `CONTRIBUTING.md`, `workstreams/README.md`). · **Depends on:** WS01–WS38 (all substantive WSes done). · **Unblocks:** [WS40](WS40-v2-release-gate.md).

## Context

This phase has rewritten huge portions of the adapter system. Documentation needs to catch up. As the cleanup gate, this WS is the one allowed to edit the README family and CHANGELOG.

## Prerequisites

All substantive WSes (WS01–WS38) merged.

## In scope

### Step 1 — `docs/adapters.md` rewrite

Replace the v0.3 content entirely. New sections:

- **Concepts**: adapter, environment, lockfile, OCI artifact, signing.
- **Quickstart**: pulling an adapter, declaring it in HCL, running a workflow.
- **Authoring**: pointer to the starter templates (WS27); SDK reference per language.
- **Secrets**: declared secrets, environment provider stack, taint propagation, shelling-out (D74).
- **Environments**: types (`shell`, `sandbox`, `container`, `remote`); policy resolution rules; per-OS support matrix.
- **Remote execution**: phone-home model; deployment patterns (k8s example link).
- **Lifecycle**: pause/resume/snapshot/inspect.
- **Security model**: process scrub, sandbox primitives per OS, redaction registry.
- **Troubleshooting**: common compile errors with fix hints.

### Step 2 — Migration guide

`docs/adapter-v2-migration.md`: for users upgrading from criteria v0.3 to v2:
- Run `criteria adapter lock` to populate the lockfile.
- Rebuild adapters against v2 SDKs (link to per-adapter migration WSes' release notes).
- Update workflow HCL: any uses of v1-only features documented.

For adapter authors: pointer to each SDK's CHANGELOG and starter template.

### Step 3 — `README.md`, `PLAN.md`, `AGENTS.md`, `CONTRIBUTING.md` updates

- `README.md` quickstart updated to reference `criteria adapter pull` and the lockfile.
- `PLAN.md`: archive this phase's workstreams (mark WS01–WS43 complete with links to merged PRs). Move `workstreams/adapter_v2/` to `workstreams/archived/v2-adapters/` (or similar) at the close of WS40.
- `AGENTS.md`: any agent-relevant patterns documented.
- `CONTRIBUTING.md`: pointer to starter templates for new adapters.

### Step 4 — `CHANGELOG.md`

A single comprehensive entry under a new release header (the version is set by WS40):

```
## [v2.0.0] — 2026-MM-DD

### Adapter system rewrite

- Protocol v2 hard cut from v1.
- Single terminology: "adapter" throughout.
- OCI-based distribution with cosign signatures; per-workflow lockfile.
- New `criteria adapter` CLI group: pull, lock, list, info, where, remove, prune, dev, publish.
- Environment block expanded with policy fields and a `remote` type for phone-home adapters.
- Snapshot/Restore and Pause/Resume lifecycle operations.
- Secrets channel + automatic log redaction + taint propagation.
- TypeScript / Python / Go SDKs with consistent helper APIs.

### Breaking changes

- v1 adapters no longer load. Existing adapters migrated to v2 in parallel.
- HCL `environment` block field set expanded; existing workflows may need a `verification = "off"` declaration if they don't ship a lockfile.

### Migration

See `docs/adapter-v2-migration.md`.
```

### Step 5 — `workstreams/README.md`

Update the phase status table to add this phase, link to `workstreams/adapter_v2/README.md` (the consolidated plan).

### Step 6 — `docs/release-process.md`

Document the four release gates (D57).

## Out of scope

- Tagging the release — WS40.
- Code changes — all done in earlier WSes.

## Behavior change

**N/A — documentation only.**

## Tests required

- Doc links checked (lychee or equivalent).
- `make docs` (if any) succeeds.

## Exit criteria

- All doc files reflect the v2 state.
- CHANGELOG entry written.

## Files this workstream may modify

- `docs/adapters.md`, `docs/adapter-v2-migration.md`, `docs/release-process.md`.
- `README.md`, `PLAN.md`, `AGENTS.md`, `CHANGELOG.md`, `CONTRIBUTING.md`.
- `workstreams/README.md`.
- `workstreams/adapter_v2/README.md` (the plan file — minor cleanup, mark final status).

## Files this workstream may NOT edit

- Source code (all WSes earlier did the work).
- Other workstream files (mark statuses only via PRs from those WSes).
