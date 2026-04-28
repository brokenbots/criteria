# Criteria roadmap

This file tracks active and upcoming phases for
[github.com/brokenbots/criteria](https://github.com/brokenbots/criteria).
Workstream files for the active phase live at
[workstreams/](workstreams/); prior phases archive into
`workstreams/archived/<phase>/`.

## Status snapshot

- **Phase 0 — Post-separation cleanup** — **closed 2026-04-27**. All nine
  workstreams merged; legacy-name gate clean; `v0.1.0` tagged. Archived under
  [workstreams/archived/v0/](workstreams/archived/v0/).
- **Phase 1 — TBD**. The first feature phase plans itself once Phase 0
  closes; candidate scope is captured in the Phase 0 close-out workstream.

## Phase 0 — Post-separation cleanup ✅ closed 2026-04-27

**Goal:** finish what the v1.6 split started — replace first-draft docs
with real ones, give the project the public-repo hygiene a v0.1 release
needs, and make a deliberate decision about the naming convention before
the project gains external visibility.

The split itself is complete (history-preserving extraction, flat
layout, `criteria.v1` proto package, conformance suite, `v0.1.0-rc1`
tag). What remains is the polish and the few structural follow-ups the
v1.6 plan deferred.

### Phase 0 workstreams (archived to [workstreams/archived/v0/](workstreams/archived/v0/))

- [W01](workstreams/archived/v0/01-naming-convention-review.md) ✅ — Naming convention
  review (corp-friendly evaluation; ADR output).
- [W02](workstreams/archived/v0/02-readme-and-contributor-docs.md) ✅ — Replace v1.6
  first-draft README and CONTRIBUTING with real ones.
- [W03](workstreams/archived/v0/03-public-plugin-sdk.md) ✅ — Extract a public
  plugin-author SDK from `internal/plugin/`.
- [W04](workstreams/archived/v0/04-shell-adapter-sandbox.md) ✅ — Shell adapter
  sandboxing plan and first hardening pass.
- [W05](workstreams/archived/v0/05-copilot-e2e-default-lane.md) ✅ — Bring the Copilot
  adapter end-to-end suite into the default test lane.
- [W06](workstreams/archived/v0/06-third-party-plugin-example.md) ✅ — Standalone
  third-party plugin example outside the repo (depends on W03).
- [W07](workstreams/archived/v0/07-repo-hygiene.md) ✅ — LICENSE, SECURITY.md,
  CODEOWNERS, issue/PR templates, dependabot config.
- [W08](workstreams/archived/v0/08-brand-rename-execution.md) ✅ — Execute the
  ADR-0001 rename: eradicated the legacy brand names across
  module path, binaries, env vars, proto package, and docs.
- [W09](workstreams/archived/v0/09-phase0-cleanup-gate.md) ✅ — Phase 0 close-out:
  validation, legacy-name merge gate, archive, tag `v0.1.0`.

*Phase 0 closed 2026-04-27. Archived under [workstreams/archived/v0/](workstreams/archived/v0/).*

## Phase 1 — TBD

Planning artifact TBD. Phase 1 scope will be defined in a new workstream set once
the team identifies the next development priorities.

## Deferred / forward-pointers

These items are known but not in Phase 0 scope:

- **Durable resume across orchestrator restart.** The conformance
  suite skips `DurableAcrossRestart` ([sdk/conformance/resume.go](sdk/conformance/resume.go))
  pending the durable-resume capability landing on the orchestrator
  side. The skip lifts when the orchestrator ships its durability work.
- **Parallel regions and sub-workflow composition** in HCL. Tracked
  for a future language phase.
- **`@criteria/proto-ts` npm package.** No TypeScript consumers in
  this repo; if a future consumer needs TS bindings, plan it then.

## Conventions

- One workstream file per discrete unit of work. Workstreams declare
  prerequisites, in-scope tasks, out-of-scope items, exit criteria,
  and tests. The workstream-executor agent works one file at a time.
- The workstream-executor and workstream-reviewer agents may **not**
  edit `README.md`, `PLAN.md`, `AGENTS.md`, or workstream files other
  than the one currently being executed. The cleanup agent (or a
  human) is the only writer for those.
- Phase close-out uses `workstreams/archived/<phase>/`. Phase 0
  archives to `workstreams/archived/v0/` when W09 lands.
