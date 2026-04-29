# Workstream 14 — Phase 2 cleanup gate

**Owner:** Cleanup agent (or human committer) · **Depends on:** [W01](01-lint-baseline-mechanical-burn-down.md)–[W13](13-rc-artifact-upload.md) · **Unblocks:** Phase 3 planning + the `v0.3.0` tag.

## Context

Phase 2 closes here. This workstream is the only one in the phase
that may edit the coordination set (`README.md`, `PLAN.md`,
`AGENTS.md`, `workstreams/README.md`, `CHANGELOG.md`,
`CONTRIBUTING.md`). It runs after every other Phase 2 workstream is
merged, performs final validation, archives the phase, and cuts
`v0.3.0`.

Same close-out shape as
[archived/v1/11-phase1-cleanup-gate.md](archived/v1/11-phase1-cleanup-gate.md).
Phase 2-specific gates:

- **Lint baseline cap.** Confirm the cap from
  [W02](02-lint-ci-gate.md) is enforced in CI and the baseline
  count is at or below the cap.
- **Maintainability + Tech Debt grade lift.** A re-run of the tech
  evaluation must show those two areas at ≥ B (the explicit
  Phase 2 goal).
- **Bus-factor goal.** Report the count of non-author humans who
  merged PRs during the phase and confirm the ≥ 2 target was met
  (or, if missed, document why and forward to Phase 3).
- **`CRITERIA_SHELL_LEGACY=1` removal.** Confirm zero source
  references after [W10](10-remove-shell-legacy-escape-hatch.md).
- **Smoke run.** A workflow exercising W05 (`workflow_file`),
  W06 (local approval), W07 (`max_visits`), and W12 (lifecycle
  log) runs end-to-end without an orchestrator.
- **RC artifact verification.** The final RC PR
  ([W13](13-rc-artifact-upload.md)) shows the artifact upload
  job firing and the bundle is downloadable.
- **Runtime image smoke.** `docker run criteria/runtime:v0.3.0`
  (or `:dev` from local build) successfully runs the same smoke
  workflow inside the container.

## Prerequisites

- Every Phase 2 workstream
  ([W01](01-lint-baseline-mechanical-burn-down.md)–[W13](13-rc-artifact-upload.md))
  merged on `main`.
- All exit criteria from each workstream verified.
- `git status` clean on `main`.
- `make ci` green on `main`.

## In scope

### Step 1 — Build / lint / test

- [ ] `make proto-check-drift` exits 0.
- [ ] `make proto-lint` exits 0.
- [ ] `make build` produces `bin/criteria`.
- [ ] `make plugins` produces all `bin/criteria-adapter-*` binaries.
- [ ] `make test` (with `-race`) green across root, `sdk/`, and
      `workflow/` modules.
- [ ] `make test-conformance` green.
- [ ] `make lint-imports` green.
- [ ] `make lint-go` green.
- [ ] `make lint-baseline-check` green ([W02](02-lint-ci-gate.md)
      gate).
- [ ] `make validate` green for every example HCL, including the
      new examples introduced by [W05](05-subworkflow-resolver-wiring.md).
- [ ] `make example-plugin` green.
- [ ] `make ci` green.
- [ ] `make docker-runtime` succeeds; `make docker-runtime-smoke`
      exits 0 ([W09](09-docker-dev-container-and-runtime-image.md)).
- [ ] CLI smoke: `./bin/criteria apply examples/hello.hcl
      --events-file /tmp/events.ndjson` exits 0.
- [ ] CLI smoke: `./bin/criteria apply
      examples/workflow_step_compose.hcl` exits 0
      ([W05](05-subworkflow-resolver-wiring.md) example).

### Step 2 — Phase 2 unattended-pipeline smoke

The Phase 2 marquee feature is unattended end-to-end execution. Run
a workflow that exercises [W05](05-subworkflow-resolver-wiring.md)
+ [W06](06-local-mode-approval.md) + [W07](07-per-step-max-visits.md)
+ [W12](12-lifecycle-log-clarity.md) together:

```hcl
# examples/phase2_smoke.hcl (or similar)
# - Uses type = "workflow" with workflow_file = "..." (W05).
# - Contains an approval node (W06).
# - One step has max_visits = 5 with a back-edge loop (W07).
# - Run with --output concise to verify W12's [adapter: ...] tag.
```

Run:

```sh
CRITERIA_LOCAL_APPROVAL=auto-approve \
  ./bin/criteria apply examples/phase2_smoke.hcl --output concise
```

Verify:

- [ ] Run completes successfully (no orchestrator, no manual
      intervention).
- [ ] Approval node auto-approves with the expected warning.
- [ ] Nested workflow loads via the resolver.
- [ ] If the back-edge loop is engineered to trip
      `max_visits = 5`, it does so with the expected error.
- [ ] Adapter lifecycle tags appear cleanly in concise output.

If the smoke does not pass, do not commit; remediate against the
relevant workstream's deliverables.

### Step 3 — Lint baseline burn-down gate

The per-workstream burn-down contract continues from Phase 1.
Run from `main` after all Phase 2 workstreams are merged:

- [ ] `.golangci.baseline.yml` total count ≤ the value in
      `tools/lint-baseline/cap.txt` (set by W02 / lowered by W01
      and W03).
- [ ] **W04-tagged baseline entries < 40** (from 133 at v0.2.0;
      W01 target).
- [ ] **W03-tagged baseline entries ≤ 10** (from 42 at v0.2.0;
      W03 target).
- [ ] **Zero `gofmt` and `goimports` baseline entries**
      (excepting generated files; W01 target).
- [ ] **Zero proto-generated `revive` baseline entries**
      (replaced by file-level `//nolint:revive` per W01 Step 3).
- [ ] Any remaining entries are explicitly accounted for in
      reviewer notes with severity and the phase they punt to
      (acceptable: residual W06-tagged style findings, residual
      revive on intentional internal naming).

### Step 4 — Determinism gate (carry over from Phase 1)

- [ ] `make test` runs 10/10 consecutive times locally without
      retry.
- [ ] `go test -race -count=20 ./internal/engine/...
      ./internal/plugin/...` green (the W01 flake watch).
- [ ] CI's `make test` step (with `-count=2`) green on the PR
      branch and on `main` after merge.

### Step 5 — Security gate

- [ ] `grep -rn 'CRITERIA_SHELL_LEGACY' --include='*.go' .`
      returns zero matches ([W10](10-remove-shell-legacy-escape-hatch.md)).
- [ ] `grep -n 'CRITERIA_SHELL_LEGACY' docs/plugins.md` returns
      zero matches.
- [ ] `grep -n 'CRITERIA_SHELL_LEGACY' docs/security/shell-adapter-threat-model.md`
      returns matches **only** in the historical "removed in
      v0.3.0" paragraph.
- [ ] `govulncheck ./...` clean across all three modules.
- [ ] `~/.criteria/` (or test temp equivalent) is created at
      mode `0o700` after [W04](04-state-dir-permissions.md).
- [ ] `~/.criteria/runs/<run_id>/approvals/` (when used by
      [W06](06-local-mode-approval.md)) is also `0o700`.
- [ ] Branch protection on `main` requires the `Lint` job per
      [W02](02-lint-ci-gate.md). Confirm the setting is applied
      by an admin; if not, escalate before tagging.

### Step 6 — Coverage / benchmark gate

The Phase 1 W06 thresholds remain in force. Phase 2 must not
regress:

- [ ] `make test-cover` reports `internal/cli/...` ≥ 60%
      (W01-W11 may have moved this; verify).
- [ ] `make test-cover` reports `internal/run/...` ≥ 60%.
- [ ] `make test-cover` reports
      `cmd/criteria-adapter-mcp/...` ≥ 50%.
- [ ] `cmd/criteria-adapter-copilot/...` coverage does not drop
      more than 2% from the v0.2.0 baseline (65.9%) after the
      [W03](03-copilot-file-split-and-permission-alias.md) split.
- [ ] `make bench` runs cleanly. Compare against
      `docs/perf/baseline-v0.2.0.md`. Any benchmark regression
      > 20% fails the gate (W06 contract).

### Step 7 — User-feedback accounting

Phase 2 addresses four of the remaining six deferred user-feedback
files (the originals preserved in git history at commit `4e4a357`):

- [W03](03-copilot-file-split-and-permission-alias.md) →
  `user_feedback/02-align-copilot-permission-kinds-user-story.txt`
  (UF#02).
- [W11](11-reviewer-outcome-aliasing.md) →
  `user_feedback/03-stabilize-reviewer-outcome-handling-user-story.txt`
  (UF#03).
- [W06](06-local-mode-approval.md) →
  `user_feedback/05-allow-approval-in-local-mode-user-story.txt`
  (UF#05).
- [W07](07-per-step-max-visits.md) →
  `user_feedback/08-add-per-step-visit-limit-to-bound-loops-user-story.txt`
  (UF#08).
- [W12](12-lifecycle-log-clarity.md) →
  `user_feedback/06-reduce-adapter-process-churn-and-eof-noise-user-story.txt`
  (UF#06).

Tasks:

- [ ] Confirm each addressed user story has a corresponding test
      or example that validates the fix.
- [ ] **UF#07** (verbose standalone output) and any further
      user-feedback items deferred to Phase 3 are listed as
      candidate scope in the updated `PLAN.md`.

### Step 8 — Bus-factor goal

The Phase 2 contributor goal from [W08](08-contributor-on-ramp.md):
**≥ 2 non-author humans land merged PRs by end of Phase 2.**

Tasks:

- [ ] Run:
      ```sh
      git log v0.2.0..HEAD --pretty="%an" | sort | uniq -c
      ```
- [ ] Record the count of non-author humans (exclude
      `dependabot[bot]`, `copilot-swe-agent[bot]`, and any other
      bot accounts).
- [ ] If ≥ 2: report success in `PLAN.md` Phase 2 retrospective
      section.
- [ ] If < 2: document the gap, root-cause it (was the
      `your-first-pr.md` walkthrough discoverable?
      did the `good-first-issue` labels surface?), and add a
      remediation note to Phase 3's "Deferred / forward-pointers"
      section.

### Step 9 — RC artifact verification

The final RC PR triggered the [W13](13-rc-artifact-upload.md)
artifact upload. Verify:

- [ ] The `release-artifacts` job ran.
- [ ] The artifact named `criteria-v0.3.0-rcN` (where N is the
      final RC) is present in the run's Artifacts panel.
- [ ] Bundle contents: `criteria`, all `criteria-adapter-*`
      binaries, `criteria-runtime.tar`, `SHA256SUMS`.
- [ ] `sha256sum -c SHA256SUMS` succeeds locally on the
      downloaded bundle.
- [ ] `docker load -i criteria-runtime.tar` succeeds and the
      image runs `examples/hello.hcl` to completion.

### Step 10 — Hygiene checks

- [ ] `git ls-files | grep -E '\.db(-(shm|wal))?$'` is empty.
- [ ] `grep -rn 'OVERSEER_' --include='*.go' .` returns no
      matches (legacy-name regression guard from Phase 0).
- [ ] `grep -rn 'OVERLORD_\|CASTLE_\|PARAPET_' --include='*.go' .`
      returns no matches.
- [ ] No orphan files in `internal/cli/testdata/compile/` or
      `internal/cli/testdata/plan/`.
- [ ] `git grep -nE 'TODO|FIXME|XXX' -- ':!workstreams/'
      ':!CHANGELOG.md'` count is recorded in reviewer notes.
      Acceptable count: ≤ 5; each remaining entry must be a
      deliberate, documented forward-pointer.

### Step 11 — Tech evaluation re-run

- [ ] File `tech_evaluations/TECH_EVALUATION-<v0.3.0-tag>.md`
      with grades for Architecture, Code Quality, Test Quality,
      Documentation, Security, Maintainability, Tech Debt,
      Performance.
- [ ] **Maintainability ≥ B** (was C+ at v0.2.0).
- [ ] **Tech Debt ≥ B** (was C at v0.2.0).
- [ ] All other grades unchanged or improved.
- [ ] If either of the two C-grade lifts is missed, do not tag;
      open a remediation PR.

### Step 12 — Documentation updates (the "files NOT to modify" set)

This workstream is the only one that may make structural edits to:

- [ ] `README.md` — update status banner to "v0.3.0"; add a
      one-line note that Phase 2 closed and the marquee
      capabilities are unattended local execution
      ([W06](06-local-mode-approval.md)+[W07](07-per-step-max-visits.md)),
      `workflow_file` resolution
      ([W05](05-subworkflow-resolver-wiring.md)), and the Docker
      runtime image
      ([W09](09-docker-dev-container-and-runtime-image.md));
      cross-link to `docs/runtime/docker.md`.
- [ ] `PLAN.md` — tick every Phase 2 workstream checkbox. Update
      "Status snapshot" to "Phase 2 closed YYYY-MM-DD". Update
      Phase 2 section to a closed/archived state. Add a "Phase 3
      — TBD" pointer plus the carry-forward candidate-scope list:
      - Environments / plug architecture (the architecture team's
        request — see plan file `we-need-to-plan-inherited-tulip.md`
        if accessible, otherwise re-derive from Phase 3 of this
        workstream's parent plan).
      - macOS sandbox-exec / Linux seccomp profiles.
      - Verbose output mode (UF#07).
      - `DurableAcrossRestart` SDK conformance lift.
      - Multi-workflow chaining (`workflow_sequence`).
      - Any Phase 2 user-feedback items not absorbed.
      - Add the contributor-goal status from Step 8.
      Add the archive footer line:
      `*Phase 2 closed YYYY-MM-DD. Archived under [workstreams/archived/v2/](workstreams/archived/v2/).*`
- [ ] `AGENTS.md` — sweep for stale references; in particular
      verify the file paths in the project map still resolve
      after the [W03](03-copilot-file-split-and-permission-alias.md)
      copilot.go split.
- [ ] `workstreams/README.md` — mark Phase 2 archived; list
      "Phase 3 — TBD". Remove the Phase 2 workstream index
      entries (they live in `archived/v2/` after the move).
- [ ] `CONTRIBUTING.md` — confirm the
      [W08](08-contributor-on-ramp.md) "First-time contributors"
      section is in place. Confirm the
      [W02](02-lint-ci-gate.md) lint-baseline cap procedure is
      documented. Append a pointer to the new
      `docs/runtime/docker.md` if the dev-container path is the
      recommended onboarding flow.
- [ ] `CHANGELOG.md` — add the v0.3.0 release-notes entry.
      Headline: "Maintainability + Tech Debt to B/B+; unattended
      local execution; Docker runtime image; CRITERIA_SHELL_LEGACY
      removed."
      Cover, in order:
      - W01 — lint baseline mechanical burn-down.
      - W02 — lint CI gate (baseline-stays-flat enforcement).
      - W03 — copilot.go file split + Copilot permission-kind
        alias (UF#02).
      - W04 — state-dir permissions hardened to 0o700.
      - W05 — `workflow_file` resolver wired into the CLI;
        `examples/workflow_step_compose.hcl` ships.
      - W06 — local-mode approval and signal wait
        (`CRITERIA_LOCAL_APPROVAL`) (UF#05).
      - W07 — per-step `max_visits` (UF#08).
      - W08 — contributor on-ramp:
        `docs/contributing/your-first-pr.md`,
        `good-first-issue` labels, numeric goal in PLAN.
      - W09 — Docker dev container + operator runtime image.
      - W10 — **`CRITERIA_SHELL_LEGACY=1` removed** (breaking;
        copy the entry text from
        [W10](10-remove-shell-legacy-escape-hatch.md)'s
        reviewer notes).
      - W11 — reviewer outcome aliasing (UF#03).
      - W12 — adapter lifecycle log clarity (UF#06); new
        `OnAdapterLifecycle` sink hook.
      - W13 — release-candidate artifact upload on RC PRs.
      - Removed: `CRITERIA_SHELL_LEGACY=1` env var.
      Tag: `v0.3.0`.

### Step 13 — Archive

- [ ] `mkdir -p workstreams/archived/v2/`
- [ ] `git mv workstreams/0[1-9]-*.md workstreams/archived/v2/`
- [ ] `git mv workstreams/1[0-3]-*.md workstreams/archived/v2/`
- [ ] `git mv workstreams/14-*.md workstreams/archived/v2/`
      (this workstream itself; do this last, in the final
      archive commit).
- [ ] Update intra-workstream links if any reviewer notes
      referenced sibling files; otherwise leave the moved files
      unchanged.
- [ ] Re-run the lint baseline gate from Step 3 and the security
      gate from Step 5 to confirm the archive move did not
      surface anything outside the allowlist.

### Step 14 — Tagging

- [ ] After all checks above pass and the docs/archive are
      committed: `git tag -a v0.3.0 -m "Phase 2: maintainability,
      unattended MVP, Docker runtime"`.
- [ ] Push the tag.
- [ ] If a tagged-release workflow exists, confirm the v0.3.0
      tag triggers it and the assets land. The
      [W13](13-rc-artifact-upload.md) artifact upload is for
      *RC PRs*; the tagged-release workflow is separate.

### Step 15 — Sibling-agent tuning

The cleanup agent may apply **at most two directive
additions/removals each** to
[.github/agents/workstream-executor.agent.md](../.github/agents/workstream-executor.agent.md)
and
[.github/agents/workstream-reviewer.agent.md](../.github/agents/workstream-reviewer.agent.md),
strictly limited to drift observed during Phase 2.

Likely candidates surfaced during Phase 2 implementation:

- Whether the lint-baseline cap from
  [W02](02-lint-ci-gate.md) needs to be encoded as a hard rule
  for the executor (currently lives in
  `docs/contributing/lint-baseline.md` and the Makefile gate).
- Whether the new "no edits to PLAN/README/AGENTS/CHANGELOG +
  no edits to other workstream files" rule from the workstream
  conventions needs to be reinforced if any workstream
  accidentally touched the coordination set.
- Whether the behavior-change disclosure section was honored in
  every workstream file (W03 through W13 must each have one).

If no drift, leave the agent files alone. Cap at two changes per
agent file. If more drift surfaces, capture it as Phase 3 planning
input rather than agent-config changes here.

### Step 16 — Optional: post-review

- [ ] After tagging, file a tracking issue for the Phase 3
      planning workstream that summarizes the deferred items and
      the bus-factor status.
- [ ] If the contributor goal was met, consider whether the
      Phase 3 goal should be raised (e.g. ≥ 3 non-author PRs).

## Behavior change

**No behavior change.** This workstream archives, validates, and
tags. All code changes happened in W01–W13.

The `README.md`, `PLAN.md`, `AGENTS.md`, `CHANGELOG.md`,
`CONTRIBUTING.md`, and `workstreams/README.md` edits are the only
documentation changes; they reflect (not introduce) the work that
landed in W01–W13.

## Reuse

- Existing close-out shape from
  [archived/v1/11-phase1-cleanup-gate.md](archived/v1/11-phase1-cleanup-gate.md).
  This workstream extends, not redesigns, that pattern.
- Existing `make ci`, `make lint-baseline-check`, `make
  test-cover`, `make bench` targets.
- Tech-eval template / format from
  [TECH_EVALUATION-20260429-01.md](../tech_evaluations/TECH_EVALUATION-20260429-01.md).

## Out of scope

- Adding new code or features. Cleanup gate only.
- Re-doing any Phase 2 workstream's deliverables. If a workstream
  is incomplete, this gate fails and that workstream re-opens.
- Phase 3 scoping. Forward-pointers in `PLAN.md` only; full
  planning happens after `v0.3.0` is tagged.

## Files this workstream may modify

The only workstream that may edit:

- `README.md`
- `PLAN.md`
- `AGENTS.md`
- `CHANGELOG.md`
- `CONTRIBUTING.md`
- `workstreams/README.md`
- `workstreams/archived/v2/*.md` (via `git mv` from
  `workstreams/0[1-9]-*.md` and `workstreams/1[0-4]-*.md`).
- `tech_evaluations/TECH_EVALUATION-<v0.3.0-tag>.md` (new).
- `.github/agents/workstream-*.agent.md` (capped at two changes
  each, only if drift observed).

This workstream may **not** edit any code under `internal/`,
`cmd/`, `workflow/`, `sdk/`, or `events/`. If a code change is
needed, it belongs in a remediation PR against the relevant
workstream, not in the cleanup gate.

## Tasks

- [ ] Build / lint / test gate (Step 1).
- [ ] Phase 2 unattended-pipeline smoke (Step 2).
- [ ] Lint baseline burn-down gate (Step 3).
- [ ] Determinism gate (Step 4).
- [ ] Security gate (Step 5).
- [ ] Coverage / benchmark gate (Step 6).
- [ ] User-feedback accounting (Step 7).
- [ ] Bus-factor goal report (Step 8).
- [ ] RC artifact verification (Step 9).
- [ ] Hygiene checks (Step 10).
- [ ] Tech evaluation re-run (Step 11).
- [ ] Documentation updates (Step 12).
- [ ] Archive (Step 13).
- [ ] Tag `v0.3.0` (Step 14).
- [ ] Sibling-agent tuning (Step 15).
- [ ] Optional post-review (Step 16).

## Exit criteria

- All gates in Steps 1–11 pass.
- `tech_evaluations/TECH_EVALUATION-<v0.3.0-tag>.md` shows
  Maintainability ≥ B and Tech Debt ≥ B.
- Phase 2 workstreams archived under `workstreams/archived/v2/`.
- `v0.3.0` tag pushed.
- `README.md`, `PLAN.md`, `AGENTS.md`, `CHANGELOG.md`,
  `CONTRIBUTING.md`, `workstreams/README.md` updated to reflect
  the v0.3.0 state.
- The bus-factor goal status is reported in `PLAN.md`.

## Tests

This workstream does not add tests; it runs the existing test and
validation matrix and confirms exit criteria. Manual verification
steps from Steps 2 and 9 are captured in reviewer notes with PR /
run / image-tag references.

## Risks

| Risk | Mitigation |
|---|---|
| One of the two C-grade lifts (Maintainability or Tech Debt) is missed at the tech-eval re-run | Do not tag `v0.3.0` until the gap is closed. Open a remediation PR against the relevant Phase 2 workstream. The plan file explicitly identified these as the Phase 2 must-haves. |
| The bus-factor goal is missed | The goal is "≥ 2 non-author human PRs". If missed, do not block the tag — document the gap in `PLAN.md`, file a Phase 3 follow-up workstream that addresses contributor-recruitment friction, and proceed. |
| Branch protection on `main` is documented but not applied (W02) | The cleanup gate verifies it explicitly in Step 5; if not applied, escalate to a project admin and do not tag until the setting is in place. |
| The smoke workflow exposes a regression introduced by an interaction between W05/W06/W07 that was not caught by per-workstream tests | Treat as a Phase 2 blocker; the gate fails and the relevant workstream re-opens. The plan deliberately scheduled the smoke at the gate to surface integration issues. |
| The W10 grep verification finds `CRITERIA_SHELL_LEGACY` references the workstream missed | Open a one-line follow-up PR to remove them; do not tag until the grep is clean. The credibility commitment from the v0.2.0 threat model is hard. |
| The artifact bundle from W13 has a SHA256SUMS mismatch (e.g. file order changed) | Re-run the upload by retriggering the RC PR's CI run; if the mismatch persists, root-cause in W13 and remediate. |
| `tech_evaluations/TECH_EVALUATION-<tag>.md` is filed but rates a category lower than expected | The tech eval is independent input; if the rater disagrees with this gate's interpretation of "Maintainability ≥ B", reconcile in reviewer notes before tagging. |
