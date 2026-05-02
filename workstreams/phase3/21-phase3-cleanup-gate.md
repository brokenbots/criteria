# Workstream 21 — Phase 3 cleanup gate

**Phase:** 3 · **Track:** E (close) · **Owner:** Cleanup agent (or human committer) · **Depends on:** every Phase 3 workstream merged ([01](01-lint-baseline-burndown.md)–[20](20-implicit-input-chaining.md)). · **Unblocks:** the `v0.3.0` tag and Phase 4 planning.

This workstream is the **only** one in Phase 3 that may edit the coordination set: [README.md](../../README.md), [PLAN.md](../../PLAN.md), [AGENTS.md](../../AGENTS.md), [CHANGELOG.md](../../CHANGELOG.md), [CONTRIBUTING.md](../../CONTRIBUTING.md), [workstreams/README.md](../README.md). It runs after every other Phase 3 workstream is merged, performs final validation, archives the phase, and cuts `v0.3.0`.

Same close-out shape as [archived/v2/16-phase2-cleanup-gate.md](../archived/v2/16-phase2-cleanup-gate.md). Phase 3-specific gates listed below.

## Context

Phase 3 closes here. The phase's marquee output is a clean break from v0.2.0:

- `agent` block → `adapter "<type>" "<name>"` block (hard rename — [11](11-agent-to-adapter-rename.md)).
- `branch` block → `switch` block (hard rename — [16](16-switch-and-if-flow-control.md)).
- `transition_to` → `next` (hard rename — [15](15-outcome-block-and-return.md), [16](16-switch-and-if-flow-control.md)).
- `lifecycle = "open"|"close"` step attribute removed (auto-managed — [12](12-adapter-lifecycle-automation.md)).
- Inline `step.workflow { ... }` and `step.workflow_file = ...` removed (replaced by `subworkflow` block — [13](13-subworkflow-block-and-resolver.md)).
- `step.adapter = "<bare type>"` removed; `step.adapter = <type>.<name>` (intermediate) and `step.target = adapter.<type>.<name>` (final) replace it ([11](11-agent-to-adapter-rename.md), [14](14-universal-step-target.md)).
- Implicit cross-scope `Vars` aliasing removed ([08](08-schema-unification.md)).
- Single-file-only entry point removed (directory mode is the only entry — [17](17-directory-module-compile.md)).
- Workflow header attributes wrapped in `workflow "<name>" { ... }` block ([17](17-directory-module-compile.md)).

Plus additive features: `local`, top-level `output`, `environment`, `subworkflow` first-class, universal `target`, `outcome.output` projection, reserved `return` outcome, `default_outcome`, `switch`/condition-`output`, multi-file modules, `shared_variable`, `parallel` modifier, implicit input chaining.

## Prerequisites

- Every active Phase 3 workstream merged on `main`: [01](01-lint-baseline-burndown.md)–[20](20-implicit-input-chaining.md).
- All exit criteria from each workstream verified.
- `git status` clean on `main`.
- `make ci` green on `main`.
- `v0.2.0` tag exists on remote (Phase 2 W16 prerequisite carried forward).

## In scope

### Step 1 — Build / lint / test

- [ ] `make proto-check-drift` exits 0 (proto field renames from [11](11-agent-to-adapter-rename.md), additive fields from [09](09-output-block.md)).
- [ ] `make proto-lint` exits 0.
- [ ] `make build` produces `bin/criteria`.
- [ ] `make plugins` produces all `bin/criteria-adapter-*` binaries.
- [ ] `make test -race -count=2` green across root, `sdk/`, and `workflow/` modules.
- [ ] `make test -race -count=20 ./internal/engine/...` green (concurrency-pressure validation for [18](18-shared-variable-block.md), [19](19-parallel-step-modifier.md)).
- [ ] `make test-conformance` green; including new `LifecycleAutomatic` (from [12](12-adapter-lifecycle-automation.md)) and any new run-output assertions (from [09](09-output-block.md)).
- [ ] `make lint-imports` green.
- [ ] `make lint-go` green.
- [ ] `make lint-baseline-check` green; `tools/lint-baseline/cap.txt` ≤ 50 from [01](01-lint-baseline-burndown.md), and the actual count matches the cap.
- [ ] `make validate` green for every example HCL — including the new examples from each rework workstream.
- [ ] `make example-plugin` green.
- [ ] `make ci` green.
- [ ] `make docker-runtime` succeeds; `make docker-runtime-smoke` exits 0.
- [ ] `govulncheck ./...` clean across all three modules.
- [ ] CLI smoke: `./bin/criteria apply examples/hello.hcl --events-file /tmp/events.ndjson` exits 0.
- [ ] Directory-mode CLI smoke: `./bin/criteria apply examples/phase3-multi-file --events-file /tmp/events.ndjson` exits 0.

### Step 2 — Phase 3 marquee smoke

A single workflow exercising every rework concept end-to-end. Author or use [examples/phase3-marquee/](../../examples/phase3-marquee/) (create if absent):

```hcl
workflow "phase3_marquee" { version = "0.3.0", environment = shell.ci }

variable "input_count" { type = "number", default = 3 }
local    "limit"       { value = var.input_count * 2 }

environment "shell" "ci" {
    variables = { CI = "true" }
}

adapter "shell" "default" { config = {} }

subworkflow "process_one" { source = "./subworkflows/process_one" }

step "fanout" {
    parallel = range(var.input_count)
    target   = subworkflow.process_one
    input    = { idx = each.value, limit = local.limit }
    outcome "success" { next = step.report }
    outcome "needs_review" { next = "return", output = { reason = step.this.output.reason } }
    default_outcome = "needs_review"
}

step "report" {
    target = adapter.shell.default
    input  = { command = "echo done" }
    outcome "success" { next = state.terminal_ok }
}

state "terminal_ok" { terminal = true, success = true }

output "processed" { type = "number", value = length(steps.fanout.output) }
```

Run (after `mkdir -p examples/phase3-marquee/subworkflows/process_one` with a minimal `process_one/main.hcl`):

```sh
./bin/criteria apply examples/phase3-marquee --output concise
./bin/criteria apply examples/phase3-marquee --output json
```

Verify:

- [ ] Run completes successfully.
- [ ] `subworkflow` invocations execute in parallel (per [19](19-parallel-step-modifier.md)).
- [ ] `outcome.output` projection bubbles through `next = "return"` (per [15](15-outcome-block-and-return.md)).
- [ ] Top-level `output "processed"` is emitted (per [09](09-output-block.md)).
- [ ] Adapter sessions auto-init/teardown (per [12](12-adapter-lifecycle-automation.md)).
- [ ] Environment variables injected into adapter subprocess (per [10](10-environment-block.md)).

### Step 3 — Lint baseline gate

- [ ] `grep -c '^\s*- path:' .golangci.baseline.yml` ≤ 50.
- [ ] `tools/lint-baseline/cap.txt` matches that count exactly.
- [ ] Zero `errcheck` and zero `contextcheck` baseline entries (Phase 3 W01 contract).
- [ ] No new W03/W04/W06/W10 entries; any residual entries are owner-tagged.
- [ ] `docs/contributing/lint-baseline.md` reflects the Phase 3 W01 burn-down with accurate counts.

### Step 4 — Determinism gate

- [ ] `make test` runs 10/10 consecutive times locally without retry.
- [ ] `go test -race -count=20 ./internal/engine/... ./internal/plugin/...` green (carry-over from Phase 1 W01; Phase 3 [18](18-shared-variable-block.md)/[19](19-parallel-step-modifier.md) raise the bar).
- [ ] CI's `make test` step (`-count=2`) green on the PR branch and on `main` after merge.

### Step 5 — Security / `govulncheck` gate

- [ ] `govulncheck ./...` clean across all three modules.
- [ ] `~/.criteria/` and `~/.criteria/runs/<run_id>/approvals/` mode `0o700` (carry-over from Phase 2 W04/W06).
- [ ] No new shell-sandbox regressions; existing W05/W10 invariants hold.
- [ ] CI's `tag-claim-check` job (from Phase 3 [06](06-release-process-integrity.md)) green on every PR.

### Step 6 — Coverage gate

- [ ] `make test-cover` reports the post-Phase-3 floors:
  - `internal/cli/...` ≥ 65%.
  - `internal/engine/...` ≥ 80%.
  - `internal/plugin/...` ≥ 70%.
  - `internal/transport/server/...` ≥ 70% (raised by Phase 3 [04](04-server-mode-coverage.md) from 63.4%).
  - `executeServerRun`, `runApplyServer`, `setupServerRun`, `drainResumeCycles` ≥ 60% each (from Phase 3 [04](04-server-mode-coverage.md), originally 0%).
  - `workflow/...` ≥ 75%.
  - `sdk/...` ≥ 75%.
  - `sdk/conformance/...` ≥ 80%.
- [ ] No package coverage drops by more than 2% from the v0.2.0 baseline.

### Step 7 — Legacy-removal grep gate

The clean break requirement. From repo root, every check below MUST return zero matches in production code (tests and migration docs are the only allowed call sites — those are permitted by passing `':!*_test.go' ':!CHANGELOG.md' ':!docs/'`):

```sh
git grep -nE '\bAgentSpec\b|\bAgentNode\b' -- ':!*_test.go' ':!CHANGELOG.md' ':!docs/' ':!workstreams/'
git grep -n '"agent,block"' -- ':!*_test.go' ':!CHANGELOG.md' ':!docs/' ':!workstreams/'
git grep -n 'hcl:"agent,optional"' -- ':!*_test.go' ':!CHANGELOG.md' ':!docs/' ':!workstreams/'
git grep -nE '\bBranchSpec\b|\bBranchNode\b|\bArmSpec\b' -- ':!*_test.go' ':!CHANGELOG.md' ':!docs/' ':!workstreams/'
git grep -n '"branch,block"' -- ':!*_test.go' ':!CHANGELOG.md' ':!docs/' ':!workstreams/'
git grep -n 'hcl:"transition_to"' -- ':!*_test.go' ':!CHANGELOG.md' ':!docs/' ':!workstreams/'
git grep -n 'hcl:"lifecycle' -- ':!*_test.go' ':!CHANGELOG.md' ':!docs/' ':!workstreams/'
git grep -nE '\bWorkflowBodySpec\b|\bbuildBodySpec\b' -- ':!*_test.go' ':!CHANGELOG.md' ':!docs/' ':!workstreams/'
git grep -n 'hcl:"workflow_file' -- ':!*_test.go' ':!CHANGELOG.md' ':!docs/' ':!workstreams/'
git grep -n 'childSt.Vars = st.Vars' -- ':!CHANGELOG.md' ':!workstreams/'
```

Each command MUST return zero. If any returns matches, the corresponding workstream did not finish its rename — open a remediation PR before tagging.

### Step 8 — Tag-claim guard self-test

- [ ] `./tools/release/extract-tag-claims.sh` emits at minimum: `v0.1.0`, `v0.2.0`, and the `v0.3.0` claim from this workstream's CHANGELOG / README updates.
- [ ] After this workstream's docs commits but before tagging, the `tag-claim-check` CI job fires on the docs PR and **fails** because `v0.3.0` is not yet on remote. Confirm the failure is explicit; this is the expected check working correctly.
- [ ] After the tag is pushed, the same job (re-run on a refresher PR) succeeds.

### Step 9 — Tech evaluation re-run

- [ ] File `tech_evaluations/TECH_EVALUATION-<v0.3.0-tag>.md` covering Architecture, Code Quality, Test Quality, Documentation, Security, Maintainability, Tech Debt, Performance, SDK / Wire Contract, Release / Operations.
- [ ] **Maintainability ≥ B** (was C+ at v0.2.0; lifted by [01](01-lint-baseline-burndown.md), [02](02-split-cli-apply.md), [03](03-split-compile-steps.md), [05](05-tracked-roadmap-artifact.md)).
- [ ] **Tech Debt ≥ B** (was C+ at v0.2.0; lifted by [01](01-lint-baseline-burndown.md), [04](04-server-mode-coverage.md), [13](13-subworkflow-block-and-resolver.md), [06](06-release-process-integrity.md)).
- [ ] **Architecture ≥ B+** (was B at v0.2.0; lifted by the rework — schema unification, subworkflow first-class, automatic lifecycle).
- [ ] **Release / Operations ≥ B-** (was C; lifted by [06](06-release-process-integrity.md)).
- [ ] All other grades unchanged or improved.
- [ ] If any of these targets is missed, do not tag; open a remediation PR.

### Step 10 — Documentation updates (the "files NOT to modify" set)

This workstream is the only one that may make structural edits to:

- [ ] [README.md](../../README.md):
  - Update the status banner to "v0.3.0".
  - Add a one-line note that Phase 3 closed and the language went through a clean break.
  - Add a section "Migrating from v0.2.0 to v0.3.0" linking to the CHANGELOG migration note.
  - Cross-link [docs/contributing/release-process.md](../../docs/contributing/release-process.md) (per [06](06-release-process-integrity.md) deferred-edit note).
  - Update install command examples to reference the new release artifacts from [06](06-release-process-integrity.md).
  - Replace any `agent`-block example with the `adapter` shape.

- [ ] [PLAN.md](../../PLAN.md):
  - Tick every Phase 3 workstream checkbox.
  - Add Phase 3 section similar to the Phase 1 / Phase 2 sections, with workstreams listed and outcomes summarized.
  - Update "Status snapshot" to "Phase 3 closed YYYY-MM-DD".
  - Add a "Phase 4 — TBD" pointer plus the carry-forward candidate-scope list:
    - Environments / plug architecture (the originally-planned Phase 3 theme — new contributor's slot).
    - macOS sandbox-exec / Linux seccomp profiles (carried over).
    - Verbose output mode (UF#07).
    - `DurableAcrossRestart` SDK conformance lift (orchestrator dependency).
    - Per-iteration adapter sessions (parallel modifier extension).
    - Remote subworkflow source schemes (`git://`, `https://`).
    - `if` block (decision deferred from [16](16-switch-and-if-flow-control.md)).
    - SetSharedVariable RPC if option-A from [18](18-shared-variable-block.md) proves insufficient.
  - Add archive footer: `*Phase 3 closed YYYY-MM-DD. Archived under [workstreams/archived/v3/](workstreams/archived/v3/).*`

- [ ] [AGENTS.md](../../AGENTS.md):
  - Sweep for stale references (file paths after [02](02-split-cli-apply.md), [03](03-split-compile-steps.md) splits).
  - Replace any `agent`-block reference with `adapter`.

- [ ] [workstreams/README.md](../README.md):
  - Replace the local plan reference at line 13 (per [05](05-tracked-roadmap-artifact.md) deferred edit) with `docs/roadmap/phase-2-summary.md`.
  - Mark Phase 3 archived; list "Phase 4 — TBD".
  - Remove the Phase 3 workstream index entries (they live in `archived/v3/` after the move).

- [ ] [CONTRIBUTING.md](../../CONTRIBUTING.md):
  - Confirm the [archived/v2/08-contributor-on-ramp.md](../archived/v2/08-contributor-on-ramp.md) "First-time contributors" section still applies.
  - Update the lint-baseline cap procedure if the cap dropped.
  - Reference [docs/contributing/release-process.md](../../docs/contributing/release-process.md).

- [ ] [CHANGELOG.md](../../CHANGELOG.md): Add the v0.3.0 release-notes entry. Headline: **"Clean break from v0.2.0: HCL/runtime rework, subworkflow features, automatic adapter lifecycle, directory-mode modules."** Cover, in order:
  - W01 — lint baseline burn-down to ≤ 50.
  - W02 — split [internal/cli/apply.go](../../internal/cli/apply.go) into focused files.
  - W03 — split [workflow/compile_steps.go](../../workflow/compile_steps.go) along step-kind lines.
  - W04 — server-mode apply test coverage (≥ 60% on previously 0% functions; transport ≥ 70%).
  - W05 — tracked roadmap artifact replacing the local-only plan reference.
  - W06 — release process integrity (`tag-claim-check` CI guard; real release workflow on tag push).
  - W07 — `local` block + compile-time fold pass; broaden `file()` validation; undeclared `var.*` references are now compile errors.
  - W08 — schema unification (`WorkflowBodySpec` removed; sub-workflow IS a Spec; cross-scope `Vars` aliasing removed). **Breaking.**
  - W09 — top-level `output` block; new `run.outputs` event.
  - W10 — `environment "<type>" "<name>"` declaration surface; env-var injection into adapter subprocesses.
  - W11 — `agent` → `adapter "<type>" "<name>"` hard rename. **Breaking.** Migration text below.
  - W12 — adapter lifecycle automation (`lifecycle = "open"|"close"` removed; auto-init at scope start, auto-tear at terminal). **Breaking.** Migration text below.
  - W13 — first-class `subworkflow "<name>"` block + CLI `SubWorkflowResolver` wiring; `--subworkflow-root` flag.
  - W14 — universal step `target` attribute. `step.adapter` / `step.agent` removed. **Breaking.** Migration text below.
  - W15 — `outcome.next` (replacing `transition_to`); reserved `return` outcome; `outcome.output` projection; `default_outcome`. **Breaking.**
  - W16 — `branch` → `switch` hard rename; `condition.match` / `condition.next` / `condition.output`. **Breaking.** Migration text below.
  - W17 — directory-level module compilation; workflow header in `workflow "<name>" { ... }` block. **Breaking.**
  - W18 — `shared_variable` block (engine-locked mutable scoped state).
  - W19 — `parallel` step modifier (concurrent execution across list items).
  - W20 — implicit input chaining (default `step.input` to previous step output).

  **Migration notes.** Append a "v0.2.0 → v0.3.0 migration guide" section enumerating every breaking removal verbatim from the per-workstream reviewer notes:
  - `agent` block migration (from [11](11-agent-to-adapter-rename.md) Step 6 reviewer notes).
  - `lifecycle` step attribute removal (from [12](12-adapter-lifecycle-automation.md) Step 7).
  - `step.adapter` / `step.agent` migration (from [14](14-universal-step-target.md) Step 6).
  - `transition_to` → `next` (from [15](15-outcome-block-and-return.md)).
  - `branch` → `switch` (from [16](16-switch-and-if-flow-control.md) Step 4).
  - Workflow header block (from [17](17-directory-module-compile.md) Step 2).
  - Inline `step.workflow { ... }` removal (from [13](13-subworkflow-block-and-resolver.md)).
  - Cross-scope `var.*` aliasing removal (from [08](08-schema-unification.md)).

  **Removed (clean break).** Enumerate every removed surface explicitly so it is unambiguous what no longer parses:
  - Top-level `agent` block.
  - `step.agent` attribute.
  - `step.adapter` attribute (bare type form).
  - `step.lifecycle` attribute.
  - `step.workflow` inline block.
  - `step.workflow_file` attribute.
  - `step.type = "workflow"` attribute.
  - Top-level `branch` block (and `arm` / `default { transition_to }`).
  - `transition_to` attribute (everywhere).
  - Top-level workflow attributes `name`/`version`/`initial_state`/`target_state` outside `workflow "<name>" { }` block.

  Tag: `v0.3.0`.

- [ ] [sdk/CHANGELOG.md](../../sdk/CHANGELOG.md):
  - Bump for the `agent_name` → `adapter_name` proto field rename ([11](11-agent-to-adapter-rename.md)).
  - Bump for any additive fields from [09](09-output-block.md).

### Step 11 — Archive

- [ ] `mkdir -p workstreams/archived/v3/`.
- [ ] `git mv workstreams/phase3/0[1-9]-*.md workstreams/archived/v3/`.
- [ ] `git mv workstreams/phase3/1[0-9]-*.md workstreams/archived/v3/`.
- [ ] `git mv workstreams/phase3/20-*.md workstreams/archived/v3/`.
- [ ] `git mv workstreams/phase3/21-*.md workstreams/archived/v3/` (this workstream itself; do this last in the final archive commit).
- [ ] `rmdir workstreams/phase3/` (the staging directory).
- [ ] Re-run the lint baseline gate (Step 3) and the legacy-removal grep gate (Step 7) to confirm the archive move did not surface anything outside the allowlist.

### Step 12 — Author the Phase 3 roadmap summary

Symmetric to Phase 3 W05's [docs/roadmap/phase-2-summary.md](../../docs/roadmap/phase-2-summary.md):

- [ ] Author `docs/roadmap/phase-3.md` with the format from [05-tracked-roadmap-artifact.md](05-tracked-roadmap-artifact.md). Workstream list, outcomes, "Source plan" disclaimer.

### Step 13 — Tagging

- [ ] After all checks above pass and the docs/archive are committed: `git tag -a v0.3.0 -m "Phase 3: HCL/runtime rework, subworkflow features, clean break from v0.2.0"`.
- [ ] Push the tag.
- [ ] Confirm the [release.yml](../../.github/workflows/release.yml) workflow from [06](06-release-process-integrity.md) triggers and produces:
  - Per-os/arch tarballs.
  - `criteria-runtime-v0.3.0.tar`.
  - `SHA256SUMS` with cosign signature.
  - GitHub Release with all artifacts attached.
- [ ] If the release workflow fails, the tag is on remote but the release is incomplete. Operator manually re-runs once secrets are configured (or the workflow bug is fixed) — do not delete the tag.

### Step 14 — Sibling-agent tuning

The cleanup agent may apply at most two directive additions/removals each to:

- [.github/agents/workstream-executor.agent.md](../../.github/agents/workstream-executor.agent.md)
- [.github/agents/workstream-reviewer.agent.md](../../.github/agents/workstream-reviewer.agent.md)

strictly limited to drift observed during Phase 3.

Likely candidates:

- Whether the broadened legacy-rejection contract (multiple block names AND multiple attribute names rejected) needs reinforcement in the executor's "do not introduce legacy shapes" rule.
- Whether the cap-stays-flat lint rule needs strengthening because Phase 3 had multiple structural rewrites that could have masked complexity additions.
- Whether the multi-file directory mode introduces a new "every example must be in a directory" expectation the executor should default to.

If no drift, leave the agent files alone. Cap at two changes per agent file.

### Step 15 — Optional: post-review

- [ ] After tagging, file a tracking issue for Phase 4 planning that summarizes:
  - Deferred items list (Step 10's PLAN.md updates).
  - The new contributor's onboarding scope (the originally-planned Phase 3: environments / plug architecture).
  - The lint baseline state (target: drop further from ≤ 50 toward ≤ 30 in Phase 4).

## Behavior change

**No behavior change.** This workstream archives, validates, and tags. All code changes happened in [01](01-lint-baseline-burndown.md)–[20](20-implicit-input-chaining.md).

The coordination-set edits ([README.md](../../README.md), [PLAN.md](../../PLAN.md), [AGENTS.md](../../AGENTS.md), [CHANGELOG.md](../../CHANGELOG.md), [CONTRIBUTING.md](../../CONTRIBUTING.md), [workstreams/README.md](../README.md), [sdk/CHANGELOG.md](../../sdk/CHANGELOG.md)) reflect (not introduce) the work that landed in the active Phase 3 set.

## Reuse

- Existing close-out shape from [archived/v2/16-phase2-cleanup-gate.md](../archived/v2/16-phase2-cleanup-gate.md). Extend, do not redesign.
- Existing `make ci`, `make lint-baseline-check`, `make test-cover`, `make bench` targets.
- Tech-eval template from [tech_evaluations/TECH_EVALUATION-20260501-01.md](../../tech_evaluations/TECH_EVALUATION-20260501-01.md).
- Per-workstream reviewer notes — the source for migration text in CHANGELOG.

## Out of scope

- Adding new code or features. Cleanup gate only.
- Re-doing any Phase 3 workstream's deliverables. If a workstream is incomplete, this gate fails and that workstream re-opens.
- Phase 4 scoping. Forward-pointers in PLAN.md only; full planning happens after `v0.3.0` is tagged.

## Files this workstream may modify

The only workstream that may edit:

- [README.md](../../README.md)
- [PLAN.md](../../PLAN.md)
- [AGENTS.md](../../AGENTS.md)
- [CHANGELOG.md](../../CHANGELOG.md)
- [CONTRIBUTING.md](../../CONTRIBUTING.md)
- [workstreams/README.md](../README.md)
- [sdk/CHANGELOG.md](../../sdk/CHANGELOG.md)
- `workstreams/archived/v3/*.md` (via `git mv` from `workstreams/phase3/`).
- `tech_evaluations/TECH_EVALUATION-<v0.3.0-tag>.md` (new).
- New: `docs/roadmap/phase-3.md`.
- [.github/agents/workstream-*.agent.md](../../.github/agents/) (capped at two changes each, only if drift observed).

This workstream may **not** edit any code under `internal/`, `cmd/`, `workflow/`, `sdk/` (except `CHANGELOG.md`), or `events/`. If a code change is needed, it belongs in a remediation PR against the relevant Phase 3 workstream.

## Tasks

- [ ] Build / lint / test gate (Step 1).
- [ ] Phase 3 marquee smoke (Step 2).
- [ ] Lint baseline gate (Step 3).
- [ ] Determinism gate (Step 4).
- [ ] Security / govulncheck gate (Step 5).
- [ ] Coverage gate (Step 6).
- [ ] Legacy-removal grep gate (Step 7).
- [ ] Tag-claim guard self-test (Step 8).
- [ ] Tech evaluation re-run (Step 9).
- [ ] Documentation updates (Step 10).
- [ ] Archive (Step 11).
- [ ] Phase 3 roadmap summary (Step 12).
- [ ] Tag `v0.3.0` (Step 13).
- [ ] Sibling-agent tuning (Step 14).
- [ ] Optional post-review (Step 15).

## Exit criteria

- All gates in Steps 1–9 pass.
- Step 7 legacy-removal grep returns zero in production code for every check.
- Tech evaluation shows Maintainability ≥ B, Tech Debt ≥ B, Architecture ≥ B+, Release/Ops ≥ B-.
- Phase 3 workstreams archived under `workstreams/archived/v3/`.
- `workstreams/phase3/` directory removed.
- `v0.3.0` tag pushed; release workflow ran (or the failure is documented and tracked).
- All coordination-set files updated.
- `docs/roadmap/phase-3.md` exists.
- `workstreams/README.md` line 13 no longer references `~/.claude/...`.

## Tests

This workstream does not add tests; it runs the existing test and validation matrix and confirms exit criteria. Manual verification steps from Step 2, Step 8, and Step 13 are captured in reviewer notes with PR / run / image-tag references.

## Risks

| Risk | Mitigation |
|---|---|
| One of the four grade lifts (Maintainability, Tech Debt, Architecture, Release/Ops) is missed at the tech-eval re-run | Do not tag `v0.3.0` until the gap is closed. Open a remediation PR against the relevant Phase 3 workstream. |
| The legacy-removal grep gate finds a missed identifier in production code | Open a one-line follow-up PR against the owning workstream (or directly here if the fix is purely cosmetic). Do not tag until the grep is clean. |
| The `release.yml` workflow from [06](06-release-process-integrity.md) fails on first real tag because of unconfigured signing secrets | Document the secret prerequisite. The tag remains valid; the operator manually re-runs the workflow once secrets are configured. |
| The marquee smoke (Step 2) exposes a regression introduced by an interaction between rework workstreams | Treat as a Phase 3 blocker; the gate fails and the relevant workstream re-opens. The smoke is deliberately scheduled at the gate to surface integration issues. |
| `tag-claim-check` (Step 8) fails on the docs PR because v0.3.0 is not yet on remote | This is the expected behavior — confirm the failure is descriptive ("doc claims tag v0.3.0 but origin has no such tag"). After the tag is pushed, a refresher PR sees the check go green. |
| Cap reduction below 50 fails because a Phase 3 workstream introduced complexity that survived the cap-stays-flat enforcement (a sibling missed something) | The cleanup gate verifies. If the cap is over, identify which workstream added the entries and remediate before tagging. |
| The coordination-set edits in Step 10 are voluminous and easy to get wrong | The workstream lists every concrete file edit explicitly. Use the per-workstream reviewer notes' migration text verbatim — do not re-derive. |
| `v0.3.0` is tagged but the release workflow does not produce a GitHub Release for some reason | The tag remains on remote (immutable). The operator can re-run the workflow manually via GitHub Actions UI. Document the recovery path in [docs/contributing/release-process.md](../../docs/contributing/release-process.md). |
