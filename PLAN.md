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
  All eleven workstreams merged; lint baseline burn-down gate clean.
  Archived under [workstreams/archived/v1/](workstreams/archived/v1/). Note:
  `v0.2.0` was documented as tagged here but the tag was not pushed at the
  time; it ships in combination with Phase 2 below at `v0.2.0`, dated 2026-05-02.
- **Phase 2 — Maintainability + unattended MVP + Copilot tool-call finalization** — **closed 2026-05-02**.
  Fourteen of sixteen workstreams merged (W05 and W11 cancelled); `v0.2.0`
  tagged at HEAD covering combined Phase 1 + Phase 2 work. Archived under
  [workstreams/archived/v2/](workstreams/archived/v2/).
- **Phase 3 — TBD.** Architecture-team direction: HCL/runtime rework before any
  feature work. See "Phase 3 forward-pointer" below for the candidate scope
  list. Originally-planned environments / plug architecture is deferred to
  Phase 4 with a new contributor.

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

## Phase 2 — Maintainability + unattended MVP + Copilot tool-call finalization ✅ closed 2026-05-02

**Goal:** lift Maintainability and Tech Debt grades from C+/C toward B, ship the smallest set of capabilities that allow unattended end-to-end execution (local-mode approval + per-step `max_visits`), replace the Copilot adapter's brittle prose-parsed outcome with a structured `submit_outcome` tool call (W14/W15 pair, replacing the cancelled W11 outcome-aliasing approach), establish Docker as the interim runtime sandbox, honor the threat-model commitment to remove `CRITERIA_SHELL_LEGACY=1`, and absorb deferred user-feedback items UF#02, UF#03, UF#05, UF#06, UF#08.

Two workstreams from the original plan were cancelled on 2026-04-30:

- **W05** (`SubWorkflowResolver` CLI wiring) — deferred to Phase 3. The compile-time gap remains a known forward-pointer; the example `examples/workflow_step_compose.hcl` does not ship with v0.2.0.
- **W11** (reviewer outcome aliasing — host-side `outcome_aliases` HCL block) — cancelled. UF#03 is now addressed at the source by **W14 + W15** (Copilot adapter finalizes via a structured `submit_outcome` tool call against the step's declared outcome set, removing the brittle `result:` prose-parsing path).

### Phase 2 workstreams (archived to [workstreams/archived/v2/](workstreams/archived/v2/))

- [W01](workstreams/archived/v2/01-lint-baseline-mechanical-burn-down.md) ✅ — Lint baseline mechanical burn-down.
- [W02](workstreams/archived/v2/02-lint-ci-gate.md) ✅ — Lint CI gate (baseline-stays-flat enforcement).
- [W03](workstreams/archived/v2/03-copilot-file-split-and-permission-alias.md) ✅ — Split `copilot.go`; Copilot permission-kind alias (UF#02).
- [W04](workstreams/archived/v2/04-state-dir-permissions.md) ✅ — `~/.criteria/` mode hardened to `0o700`.
- [W05](workstreams/archived/v2/05-subworkflow-resolver-wiring.md) — *Cancelled 2026-04-30; deferred to Phase 3.*
- [W06](workstreams/archived/v2/06-local-mode-approval.md) ✅ — Local-mode approval and signal wait via `CRITERIA_LOCAL_APPROVAL` (UF#05).
- [W07](workstreams/archived/v2/07-per-step-max-visits.md) ✅ — Per-step `max_visits` (UF#08).
- [W08](workstreams/archived/v2/08-contributor-on-ramp.md) ✅ — Contributor on-ramp; numeric bus-factor goal.
- [W09](workstreams/archived/v2/09-docker-dev-container-and-runtime-image.md) ✅ — VS Code dev container + operator runtime image.
- [W10](workstreams/archived/v2/10-remove-shell-legacy-escape-hatch.md) ✅ — Removed `CRITERIA_SHELL_LEGACY=1`.
- [W11](workstreams/archived/v2/11-reviewer-outcome-aliasing.md) — *Cancelled 2026-04-30; UF#03 addressed by W14+W15.*
- [W12](workstreams/archived/v2/12-lifecycle-log-clarity.md) ✅ — Adapter lifecycle log clarity; `OnAdapterLifecycle` sink hook (UF#06).
- [W13](workstreams/archived/v2/13-rc-artifact-upload.md) ✅ — RC artifact upload.
- [W14](workstreams/archived/v2/14-copilot-tool-call-wire-contract.md) ✅ — Copilot tool-call wire contract: `pb.ExecuteRequest.AllowedOutcomes`; SDK bump.
- [W15](workstreams/archived/v2/15-copilot-submit-outcome-adapter.md) ✅ — Copilot `submit_outcome` adapter: tool-call outcome finalization; removed `result:` prose parsing (UF#03).
- [W16](workstreams/archived/v2/16-phase2-cleanup-gate.md) ✅ — Phase 2 cleanup gate: validation, lint-baseline gate, archive, tag `v0.2.0`.

*Phase 2 closed 2026-05-02. Archived under [workstreams/archived/v2/](workstreams/archived/v2/). Tech evaluation re-run filed at [tech_evaluations/TECH_EVALUATION-20260501-01.md](tech_evaluations/TECH_EVALUATION-20260501-01.md).*

### Phase 2 retrospective notes

- **Bus-factor goal (W08).** The Phase 2 target was ≥ 2 non-author humans landing merged PRs. Result: **0 non-author human PRs.** Commit count since `v0.1.0`: 64 Dave Sanderson, 2 Copilot bot, 1 dependabot, 1 copilot-swe-agent. The first-time-contributor walkthrough (`docs/contributing/your-first-pr.md`) and `good-first-issue` labels both shipped, but no external contributor has yet picked one up. Carry forward to Phase 3 with the same target raised to ≥ 2 (the goal applies to non-author *humans*, so the bots do not count).
- **Tag-claim discipline.** The pre-existing `v0.2.0` claim in CHANGELOG and PLAN was a forward reference, not an actual tag — the tech evaluation flagged this as the #1 critical-severity tech debt. The W16 cleanup tag fixes this by pushing `v0.2.0` to remote at HEAD, with the CHANGELOG entry expanded to cover both phases.
- **Tech-debt grades.** Per [tech_evaluations/TECH_EVALUATION-20260501-01.md](tech_evaluations/TECH_EVALUATION-20260501-01.md): Maintainability lifted from C+ to **C+** (the prior B target was missed — the project remains effectively single-maintainer until non-author PRs land); Tech Debt lifted from C to **C+** (cap is exactly full at 70/70, leaving no headroom for Phase 3 structural changes — Phase 3 W01 burns this down before any rework lands).

## Phase 3 — TBD

Architecture-team direction (per architecture_notes.md and proposed_hcl.hcl): the next phase is an HCL/runtime rework. The originally-planned Phase 3 environments / plug architecture theme is deferred to Phase 4 with a new contributor.

Phase 3 candidate scope (target `v0.3.0`, clean break from v0.2.0):

- **Pre-rework cleanup.** Lint baseline burn-down to ≤ 50 (W01). Split [internal/cli/apply.go](internal/cli/apply.go) (W02) and [workflow/compile_steps.go](workflow/compile_steps.go) (W03). Server-mode apply test coverage (W04). Tracked roadmap artifact (W05) replacing the local-only plan reference. Release-process integrity: CI tag-claim guard + signed release workflow (W06).
- **Compile-time/runtime semantics.** `local "<name>"` block + constant-fold pass that broadens `file()` validation across every attribute slot (W07). Schema unification — drop `WorkflowBodySpec`, sub-workflow IS a `Spec`, drop cross-scope `Vars` aliasing (W08). Top-level `output "<name>"` block + new `run.outputs` event (W09). `environment "<type>" "<name>"` declaration surface with env-var injection (W10).
- **Language surface — clean break.** `agent` block hard-renamed to `adapter "<type>" "<name>"` (W11). Adapter lifecycle automation (drop `lifecycle = "open"|"close"`) (W12). First-class `subworkflow "<name>"` block + CLI `SubWorkflowResolver` wiring (W13). Universal step `target = adapter.<type>.<name> | subworkflow.<name> | step.<name>` attribute (W14). `outcome.next` (replacing `transition_to`) + reserved `return` outcome + `default_outcome` (W15). `branch` block hard-renamed to `switch` with `condition.match`/`next`/`output` (W16). Directory-level multi-file module compilation as the only entry shape (W17).
- **Runtime.** `shared_variable` block (engine-locked mutable scoped state) (W18). `parallel` step modifier for concurrent execution across list items (W19). Implicit input chaining (default `step.input` to previous step output) (W20).
- **Close gate.** Phase 3 cleanup gate (W21): legacy-removal grep gate, lint baseline ≤ 50, tech-eval re-run, archive, tag `v0.3.0`.

The detailed plan and per-workstream files are staged locally and have not yet been committed to `main`. They land as Phase 3 begins.

## Deferred / forward-pointers (Phase 4 and beyond)

- **Environments / plug architecture** — the originally-planned Phase 3 theme. A new layer in [internal/plugin/loader.go:124](internal/plugin/loader.go) (the `exec.Command(path)` site) wraps an adapter subprocess inside an isolation environment. First reference implementation: a Docker environment, building on Phase 2 W09. New contributor's slot.
- **Platform-specific shell sandboxing.** macOS `sandbox-exec` / Linux seccomp profiles.
- **Remaining user-feedback files.** UF#07 (verbose standalone output) and any other items in `user_feedback/` not absorbed by Phase 1 or Phase 2.
- **Durable resume across orchestrator restart.** The conformance suite skips `DurableAcrossRestart` ([sdk/conformance/resume.go](sdk/conformance/resume.go)) pending the durable-resume capability landing on the orchestrator side. The skip lifts when the orchestrator ships its durability work.
- **`@criteria/proto-ts` npm package.** No TypeScript consumers in this repo; if a future consumer needs TS bindings, plan it then.
- **Remote subworkflow source schemes** (`git://`, `https://`). Phase 3 lands local-path resolution; remote schemes are a follow-up.
- **`if` block.** Decision deferred from Phase 3 W16 — `switch` covers the surface; `if` would be syntactic sugar.
- **Per-iteration adapter sessions** for the `parallel` step modifier. Default is shared session; per-iteration is future ergonomics.
- **Bus-factor.** Carry the Phase 2 ≥ 2 non-author-human PR target forward to Phase 3.

## Conventions

- One workstream file per discrete unit of work. Workstreams declare
  prerequisites, in-scope tasks, out-of-scope items, exit criteria,
  and tests. The workstream-executor agent works one file at a time.
- The workstream-executor and workstream-reviewer agents may **not**
  edit `README.md`, `PLAN.md`, `AGENTS.md`, `CHANGELOG.md`,
  `CONTRIBUTING.md`, `workstreams/README.md`, or workstream files
  other than the one currently being executed. The cleanup agent
  (or a human) is the only writer for those.
- Phase close-out uses `workstreams/archived/<phase>/`. Phase 0
  archived to `archived/v0/`, Phase 1 to `archived/v1/`, Phase 2 to
  `archived/v2/`. Phase 3 archives to `archived/v3/` when its cleanup
  gate lands.
