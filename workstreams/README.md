# Criteria workstreams

The active phase's workstream files live at the top of this directory;
prior phases are in [`archived/`](archived/).

## Status

- **Phase 0** — post-separation cleanup — **closed 2026-04-27**. All nine
  workstreams merged; `v0.1.0` tagged. Archived under [`archived/v0/`](archived/v0/).
- **Phase 1** — stabilization + critical user fixes — **closed 2026-04-29**.
  All eleven workstreams merged; lint baseline burn-down gate clean; `v0.2.0` tagged.
  Archived under [`archived/v1/`](archived/v1/).
- **Phase 2** — maintainability + unattended MVP + Docker runtime — **kicked off**. Fourteen workstreams scoped; targeting `v0.3.0`. Plan at `~/.claude/plans/we-need-to-plan-inherited-tulip.md` (local). See [PLAN.md](../PLAN.md) for the project-level roadmap.

## Phase 2 workstreams

Phase 2 brings Maintainability and Tech Debt up from C+/C to ≥ B, ships the smallest set of features that allow unattended end-to-end execution (`workflow_file` resolver + local-mode approval + per-step `max_visits`), establishes Docker as the interim runtime sandbox, honors the v0.3.0 commitment to remove `CRITERIA_SHELL_LEGACY=1`, and absorbs four deferred user-feedback items (UF#02, UF#03, UF#05, UF#06, UF#08).

- [W01](01-lint-baseline-mechanical-burn-down.md) — Lint baseline mechanical burn-down (gofmt/goimports/unused; reclassify proto-generated `revive`).
- [W02](02-lint-ci-gate.md) — Lint CI gate; baseline-stays-flat enforcement via `tools/lint-baseline/cap.txt`.
- [W03](03-copilot-file-split-and-permission-alias.md) — Split `copilot.go` (793 LOC) into focused siblings; land Copilot permission-kind alias (UF#02).
- [W04](04-state-dir-permissions.md) — Tighten `~/.criteria/` directory mode to `0o700`.
- [W05](05-subworkflow-resolver-wiring.md) — Wire `SubWorkflowResolver` into the CLI compile path; ship `examples/workflow_step_compose.hcl`.
- [W06](06-local-mode-approval.md) — Local-mode approval and signal wait via `CRITERIA_LOCAL_APPROVAL` (UF#05).
- [W07](07-per-step-max-visits.md) — Per-step `max_visits` to bound runaway loops (UF#08).
- [W08](08-contributor-on-ramp.md) — Author `docs/contributing/your-first-pr.md`; label five `good-first-issue` items; numeric Phase 2 contributor goal.
- [W09](09-docker-dev-container-and-runtime-image.md) — VS Code dev container + operator runtime image as the interim runtime sandbox.
- [W10](10-remove-shell-legacy-escape-hatch.md) — Remove `CRITERIA_SHELL_LEGACY=1` per the v0.2.0 threat-model commitment.
- [W11](11-reviewer-outcome-aliasing.md) — Optional `outcome_aliases` HCL block; richer unmapped-outcome diagnostics (UF#03).
- [W12](12-lifecycle-log-clarity.md) — Adapter lifecycle log clarity; new `OnAdapterLifecycle` sink hook (UF#06).
- [W13](13-rc-artifact-upload.md) — Release-candidate artifact upload on PRs marked `release/*` or with `-rc<N>` titles.
- [W14](14-phase2-cleanup-gate.md) — Phase 2 cleanup gate: validation, lint-baseline gate, tech-eval re-run, archive, tag `v0.3.0`.

### Workstream conventions (Phase 2)

Every Phase 2 workstream file declares:

- **Goal**, **Prerequisites**, **In scope** (with file paths and line ranges), **Out of scope** (explicit "do not touch" list), **Reuse pointers** (existing functions/interfaces to use), **Behavior change** disclosure ("yes" or "no"; if yes, every observable difference enumerated for the reviewer), **Tests required**, **Exit criteria**, and a **Files this workstream may modify** list.
- The "may not edit" set is restated in every workstream: `README.md`, `PLAN.md`, `AGENTS.md`, `CHANGELOG.md`, `workstreams/README.md`, and any other workstream file. Those are W14's territory.

See [PLAN.md](../PLAN.md) for the project-level roadmap.

## Phase 1 workstreams (archived)

All Phase 1 workstream files have been moved to [`archived/v1/`](archived/v1/).

## Phase 0 workstreams (archived)

All Phase 0 workstream files have been moved to [`archived/v0/`](archived/v0/).

## Phase 3 forward-pointer

Phase 3 is sketched in the parent plan but not yet scoped here. Targeted theme:

- **Environments / plug architecture.** A new layer in `internal/plugin/loader.go:124` (the `exec.Command(path)` site) that wraps an adapter subprocess inside an isolation environment. First reference implementation: a Docker environment, building on Phase 2 [W09](09-docker-dev-container-and-runtime-image.md). The architecture team explicitly framed this as the precursor to OS-level controls.
- Verbose output mode (UF#07).
- Continued lint baseline burn-down toward < 50 entries.
- `DurableAcrossRestart` SDK conformance lift (orchestrator dependency).
- Contributor goal: ≥ 3 non-author humans by end of Phase 3.

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
