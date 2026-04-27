# Overseer workstreams

The active phase's workstream files live at the top of this directory;
prior phases are in [`archived/`](archived/).

## Status

- **Phase 0** — post-separation cleanup; establishing overseer as its
  own project after the v1.6 split from the overlord monorepo. **Active.**

See [PLAN.md](../PLAN.md) for the project-level roadmap.

## Phase 0 workstreams (active)

The set is sequenced by dependency, not strictly serial. Workstreams
without an explicit dependency line are independent and may be picked
up in parallel.

**A. Direction-setting (do this first; it informs later docs)**

- [W01](01-naming-convention-review.md) — Naming convention review
  (corp-friendly evaluation; ADR-0001 output).

**B. Project-as-its-own (independent)**

- [W02](02-readme-and-contributor-docs.md) — Replace v1.6 first-draft
  README and CONTRIBUTING.
- [W07](07-repo-hygiene.md) — LICENSE, SECURITY.md, CODEOWNERS,
  issue/PR templates, dependabot config.

**C. Structural follow-ups deferred from v1.6 (independent of A/B)**

- [W03](03-public-plugin-sdk.md) — Extract a public plugin-author SDK
  from `internal/plugin/`.
- [W04](04-shell-adapter-sandbox.md) — Shell adapter sandboxing plan
  and first hardening pass.
- [W05](05-copilot-e2e-default-lane.md) — Copilot E2E into the
  default test lane.

**D. Depends on C**

- [W06](06-third-party-plugin-example.md) — Standalone third-party
  plugin example outside the repo (depends on [W03](03-public-plugin-sdk.md)).

**E. Phase close-out**

- [W08](08-phase0-cleanup-gate.md) — Phase 0 cleanup gate: validation,
  archive, tag `v0.1.0`.

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
gate (W08), or (b) defers the edit to W08 with a forward-pointer note
in its reviewer log.

## Archived

There is no archived phase yet. The pre-separation v1.x phases live
in the overlord repo's `workstreams/archived/`; they are not copied
here. Phase 0 will be the first archived phase under
`workstreams/archived/v0/` once W08 lands.
