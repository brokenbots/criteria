# Criteria workstreams

The active phase's workstream files live at the top of this directory;
prior phases are in [`archived/`](archived/).

## Status

- **Phase 0** — post-separation cleanup — **closed 2026-04-27**. All nine
  workstreams merged; `v0.1.0` tagged. Archived under [`archived/v0/`](archived/v0/).
- **Phase 1** — stabilization + critical user fixes — **closed 2026-04-29**.
  All eleven workstreams merged; lint baseline burn-down gate clean.
  Archived under [`archived/v1/`](archived/v1/). The `v0.2.0` tag was
  documented but not pushed at this close; it ships at HEAD with the
  combined Phase 1 + Phase 2 work below.
- **Phase 2** — maintainability + unattended MVP + Docker runtime + Copilot
  tool-call finalization — **closed 2026-05-02**. Sixteen workstreams scoped,
  two cancelled (W05, W11). `v0.2.0` tagged at HEAD covering combined Phase 1
  + Phase 2 work. Archived under [`archived/v2/`](archived/v2/).
- **Phase 3** — TBD. Architecture-team direction is an HCL/runtime rework;
  see [PLAN.md](../PLAN.md) for the candidate scope and the "Phase 3
  forward-pointer" section below.

## Phase 2 workstreams (archived)

All Phase 2 workstream files have been moved to [`archived/v2/`](archived/v2/).
See [PLAN.md](../PLAN.md) for the project-level roadmap with per-workstream
links and outcomes.

## Phase 1 workstreams (archived)

All Phase 1 workstream files have been moved to [`archived/v1/`](archived/v1/).

## Phase 0 workstreams (archived)

All Phase 0 workstream files have been moved to [`archived/v0/`](archived/v0/).

## Phase 3 forward-pointer

Phase 3 is sketched in [PLAN.md](../PLAN.md) but not yet active here. Targeted
theme (per architecture_notes.md and proposed_hcl.hcl): **HCL/runtime rework
with a clean break from v0.2.0**. Twenty-one workstreams are scoped; the
detailed per-workstream files have been drafted locally and will be moved into
this directory when Phase 3 begins. The originally-planned Phase 3 environments
/ plug architecture theme is deferred to Phase 4 with a new contributor.

Headline scope:

- **Pre-rework cleanup.** Lint baseline burn-down to ≤ 50; split
  [internal/cli/apply.go](../internal/cli/apply.go) and
  [workflow/compile_steps.go](../workflow/compile_steps.go); server-mode apply
  test coverage; tracked roadmap artifact; release-process integrity.
- **Compile-time / runtime semantics.** `local "<name>"` block + constant-fold
  pass; schema unification (drop `WorkflowBodySpec`, sub-workflow IS a `Spec`,
  drop cross-scope `Vars` aliasing); top-level `output` block; `environment`
  declaration surface.
- **Language surface — clean break.** `agent` → `adapter "<type>" "<name>"`
  hard rename; adapter lifecycle automation; first-class `subworkflow` block
  with CLI resolver wiring; universal step `target` attribute; `outcome.next`
  + reserved `return` outcome; `branch` → `switch` rename; directory-level
  multi-file module compilation as the only entry shape.
- **Runtime.** `shared_variable` block; `parallel` step modifier; implicit
  input chaining.

Phase 4 candidate scope (deferred): environments / plug architecture (the
originally-planned Phase 3 theme), platform-specific shell sandboxing,
durable-resume conformance lift, remote subworkflow source schemes, `if`
block decision, per-iteration adapter sessions.

## Workstream conventions

Every workstream file declares:

- **Goal**, **Prerequisites**, **In scope** (with file paths and line ranges),
  **Out of scope** (explicit "do not touch" list), **Reuse pointers** (existing
  functions/interfaces to use), **Behavior change** disclosure ("yes" or "no";
  if yes, every observable difference enumerated for the reviewer), **Tests
  required**, **Exit criteria**, and a **Files this workstream may modify**
  list.
- The "may not edit" set is restated in every workstream: `README.md`,
  `PLAN.md`, `AGENTS.md`, `CHANGELOG.md`, `CONTRIBUTING.md`,
  `workstreams/README.md`, and any other workstream file. Those are the
  cleanup-gate's territory.

See [PLAN.md](../PLAN.md) for the project-level roadmap.

## Files NOT editable by workstream-executor or workstream-reviewer

The executor and reviewer agents are scoped to **the single workstream
file they are executing**. They may not edit:

- `README.md`
- `PLAN.md`
- `AGENTS.md`
- `CHANGELOG.md`
- `CONTRIBUTING.md`
- `workstreams/README.md`
- Any other workstream file in this directory

A workstream that needs changes to those files declares them in its
"Files this workstream may modify" list and must be the cleanup gate
for that phase, or it defers the edit to the cleanup gate with a
forward-pointer note in its reviewer log.

## Archived

- Phase 0 — [`archived/v0/`](archived/v0/) (closed 2026-04-27, `v0.1.0`).
- Phase 1 — [`archived/v1/`](archived/v1/) (closed 2026-04-29).
- Phase 2 — [`archived/v2/`](archived/v2/) (closed 2026-05-02, `v0.2.0`
  combined-phase tag).

The pre-separation v1.x phases live in the orchestrator repo's
`workstreams/archived/`; they are not copied here.
