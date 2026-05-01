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
- **Phase 1 — Stabilization and critical user fixes** — **closed 2026-04-29**.
  All eleven workstreams merged; lint baseline burn-down gate clean; `v0.2.0` tagged.
  Archived under [workstreams/archived/v1/](workstreams/archived/v1/).
- **Phase 2 — TBD.** See "Deferred / forward-pointers" below for the candidate scope list.

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

## Phase 1 — Stabilization and critical user fixes ✅ closed 2026-04-29

**Goal:** harden CI, adopt golangci-lint with a per-workstream baseline
burn-down, sandbox the shell adapter, ship coverage/benchmark/GoDoc
baselines, and unblock four user-reported issues (the `file()`
expression family, step-level iteration with a nested `workflow` step
type, Copilot agent defaults, and a `count`-style construct).

### Phase 1 workstreams (archived to [workstreams/archived/v1/](workstreams/archived/v1/))

- [W01](workstreams/archived/v1/01-flaky-test-fix.md) ✅ — flaky test fix (deterministic CI: `-count=2`, `goleak`).
- [W02](workstreams/archived/v1/02-golangci-lint-adoption.md) ✅ — golangci-lint adoption with per-workstream baseline burn-down contract.
- [W03](workstreams/archived/v1/03-god-function-refactor.md) ✅ — god-function refactor (no behavior change).
- [W04](workstreams/archived/v1/04-split-oversized-files.md) ✅ — oversized-file splits in `workflow/`, `conformance/`, server transport.
- [W05](workstreams/archived/v1/05-shell-adapter-sandbox.md) ✅ — shell adapter first-pass sandboxing + threat model + `CRITERIA_SHELL_LEGACY=1` opt-out.
- [W06](workstreams/archived/v1/06-coverage-bench-godoc.md) ✅ — coverage thresholds, benchmark baselines, GoDoc on public packages.
- [W07](workstreams/archived/v1/07-file-expression-function.md) ✅ — `file()` / `fileexists()` / `trimfrontmatter()` HCL functions.
- [W08](workstreams/archived/v1/08-for-each-multistep.md) ✅ — multi-step `for_each` iteration bodies. **Superseded within Phase 1 by W10**: the runtime model is replaced; the user story stays satisfied via W10's `type = "workflow"` step.
- [W09](workstreams/archived/v1/09-copilot-agent-defaults.md) ✅ — Copilot `reasoning_effort` no longer silently dropped; per-step override; targeted diagnostic for misplaced agent-config fields.
- [W10](workstreams/archived/v1/10-step-iteration-and-workflow-step.md) ✅ — step-level `for_each` and `count` on any step type; new `type = "workflow"` step with inline or `workflow_file` body; indexed outputs; full `each.*` binding set; `on_failure` modes; explicit `output` blocks. Removes W08's top-level `for_each` block.
- [W11](workstreams/archived/v1/11-phase1-cleanup-gate.md) ✅ — Phase 1 cleanup gate: validation lanes, lint baseline burn-down gate, coverage gate, archive, tag `v0.2.0`.

*Phase 1 closed 2026-04-29. Archived under [workstreams/archived/v1/](workstreams/archived/v1/).*

## Phase 2 — Maintainability + unattended MVP + Copilot tool-call finalization (in progress)

**Goal:** lift Maintainability and Tech Debt grades from C+/C to ≥ B, ship the smallest set of capabilities that allow unattended end-to-end execution (local-mode approval + per-step `max_visits`), replace the Copilot adapter's brittle prose-parsed outcome with a structured `submit_outcome` tool call (new W14/W15 pair, replacing the cancelled W11 outcome-aliasing approach), establish Docker as the interim runtime sandbox, honor the v0.3.0 commitment to remove `CRITERIA_SHELL_LEGACY=1`, and absorb deferred user-feedback items UF#02, UF#03, UF#05, UF#06, UF#08.

Two workstreams from the original plan were cancelled on 2026-04-30:

- **W05** (`SubWorkflowResolver` CLI wiring) — deferred to Phase 3. The compile-time gap remains a known forward-pointer; the example `examples/workflow_step_compose.hcl` does not ship with v0.3.0.
- **W11** (reviewer outcome aliasing — host-side `outcome_aliases` HCL block) — cancelled. UF#03 is now addressed at the source by **W14 + W15** (Copilot adapter finalizes via a structured `submit_outcome` tool call against the step's declared outcome set, removing the brittle `result:` prose-parsing path).

### Phase 2 workstreams (active set, target `v0.3.0`)

Workstream files live at [workstreams/](workstreams/).

- [W01](workstreams/01-lint-baseline-mechanical-burn-down.md) — Lint baseline mechanical burn-down.
- [W02](workstreams/02-lint-ci-gate.md) — Lint CI gate (baseline-stays-flat enforcement).
- [W03](workstreams/03-copilot-file-split-and-permission-alias.md) — Split `copilot.go`; Copilot permission-kind alias (UF#02).
- [W04](workstreams/04-state-dir-permissions.md) — `~/.criteria/` mode hardened to `0o700`.
- [W05](workstreams/05-subworkflow-resolver-wiring.md) — *Cancelled 2026-04-30; deferred to Phase 3.*
- [W06](workstreams/06-local-mode-approval.md) — Local-mode approval and signal wait via `CRITERIA_LOCAL_APPROVAL` (UF#05).
- [W07](workstreams/07-per-step-max-visits.md) — Per-step `max_visits` (UF#08).
- [W08](workstreams/08-contributor-on-ramp.md) — Contributor on-ramp; numeric bus-factor goal.
- [W09](workstreams/09-docker-dev-container-and-runtime-image.md) — VS Code dev container + operator runtime image.
- [W10](workstreams/10-remove-shell-legacy-escape-hatch.md) — Remove `CRITERIA_SHELL_LEGACY=1`.
- [W11](workstreams/11-reviewer-outcome-aliasing.md) — *Cancelled 2026-04-30; UF#03 addressed by W14+W15.*
- [W12](workstreams/12-lifecycle-log-clarity.md) — Adapter lifecycle log clarity; `OnAdapterLifecycle` sink hook (UF#06).
- [W13](workstreams/13-rc-artifact-upload.md) — RC artifact upload.
- [W14](workstreams/14-copilot-tool-call-wire-contract.md) — Copilot tool-call wire contract: `pb.ExecuteRequest.AllowedOutcomes`; SDK bump.
- [W15](workstreams/15-copilot-submit-outcome-adapter.md) — Copilot `submit_outcome` adapter: tool-call outcome finalization with 3-attempt reprompt; remove `result:` prose parsing (UF#03).
- [W16](workstreams/16-phase2-cleanup-gate.md) — Phase 2 cleanup gate: validation, lint-baseline gate, tech-eval re-run, archive, tag `v0.3.0`.

## Deferred / forward-pointers

Phase 2 candidate scope (triage list, not a commitment — Phase 2 planning prioritizes from this):

- **Platform-specific shell sandboxing** (W05 `[ARCH-REVIEW]`): macOS `sandbox-exec` / Linux seccomp profiles. Deferred pending Phase 2 security prioritization.
- **Remaining user-feedback files** (deferred by design in Phase 1): `02`, `03`, `05`, `06`, `07`, `08` in `user_feedback/`. No action on these in Phase 1.
- **Durable resume across orchestrator restart.** The conformance
  suite skips `DurableAcrossRestart` ([sdk/conformance/resume.go](sdk/conformance/resume.go))
  pending the durable-resume capability landing on the orchestrator
  side. The skip lifts when the orchestrator ships its durability work. Carried over from Phase 0.
- **Parallel regions and sub-workflow composition** in HCL. Tracked
  for a future language phase.
- **`@criteria/proto-ts` npm package.** No TypeScript consumers in
  this repo; if a future consumer needs TS bindings, plan it then. Carried over from Phase 0.
- **`workflow_file` full runtime resolution** (W10 partial): `SubWorkflowResolver` is not wired into the CLI compile path; `workflow_file` validation requires a resolver at compile time. The example `examples/workflow_step_compose.hcl` is deferred until this wiring lands. Phase 2 originally scoped this as W05 but cancelled it on 2026-04-30 to prioritize Copilot tool-call outcome finalization (W14/W15); carry forward to Phase 3.
- **Lint baseline burn-down (W03/W04 residual entries)**: 42 W03-tagged and 133 W04-tagged entries remain in `.golangci.baseline.yml`. W03 residual covers `handlePermissionRequest`, `permissionDetails`, and extracted helper functions outside the original four-function scope; W04 residual covers gofmt/goimports/unused findings on split files. These are approved exceptions for Phase 2 cleanup. See workstream reviewer notes for full accounting.
- **Lint baseline enforcement in CI** (`make lint-go` is currently manual; CI enforcement as a permanent gate is a Phase 2 nice-to-have).

## Conventions

- One workstream file per discrete unit of work. Workstreams declare
  prerequisites, in-scope tasks, out-of-scope items, exit criteria,
  and tests. The workstream-executor agent works one file at a time.
- The workstream-executor and workstream-reviewer agents may **not**
  edit `README.md`, `PLAN.md`, `AGENTS.md`, or workstream files other
  than the one currently being executed. The cleanup agent (or a
  human) is the only writer for those.
- Phase close-out uses `workstreams/archived/<phase>/`. Phase 1
  archives to `workstreams/archived/v1/` when W11 lands.
