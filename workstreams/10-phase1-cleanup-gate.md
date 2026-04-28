# Workstream 10 — Phase 1 cleanup gate

**Owner:** Cleanup agent (or human committer) · **Depends on:** [W01](01-flaky-test-fix.md)–[W09](09-copilot-agent-defaults.md) · **Unblocks:** Phase 2 planning + the `v0.2.0` tag.

## Context

Phase 1 closes here. This workstream is the only one in the phase
that may edit the coordination set (`README.md`, `PLAN.md`,
`AGENTS.md`, `workstreams/README.md`, `CHANGELOG.md`,
`CONTRIBUTING.md`). It runs after every other Phase 1 workstream
is merged, performs final validation, archives the phase, and
cuts `v0.2.0`.

This is the same close-out shape used at the end of Phase 0
([archived/v0/09-phase0-cleanup-gate.md](archived/v0/09-phase0-cleanup-gate.md)).
The wrinkle for Phase 1 is the **golangci-lint baseline-burn-down
gate**: this workstream refuses to tag `v0.2.0` if
`.golangci.baseline.yml` still contains entries pointed at
W03/W04/W06 — the entire point of the per-workstream burn-down
contract.

## Prerequisites

- Every Phase 1 workstream
  ([W01](01-flaky-test-fix.md)–[W09](09-copilot-agent-defaults.md))
  merged on `main`.
- All exit criteria from each workstream verified.
- `git status` clean on `main`.
- `make ci` green on `main`.

## In scope

### Step 1 — Build / lint / test

- [ ] `make proto-check-drift` exits 0.
- [ ] `make proto-lint` exits 0.
- [ ] `make build` produces `bin/criteria`.
- [ ] `make plugins` produces all `bin/criteria-adapter-*`
      binaries.
- [ ] `make test` (with `-race`) green across root, `sdk/`, and
      `workflow/` modules.
- [ ] `make test-conformance` green.
- [ ] `make lint-imports` green.
- [ ] `make lint-go` green (the [W02](02-golangci-lint-adoption.md)
      gate).
- [ ] `make validate` green for every example HCL, including the
      new examples introduced by [W07](07-file-expression-function.md),
      [W08](08-for-each-multistep.md), and [W09](09-copilot-agent-defaults.md).
- [ ] `make example-plugin` green.
- [ ] `make ci` green (the aggregate target).
- [ ] CLI smoke: `./bin/criteria apply examples/hello.hcl
      --events-file /tmp/events.ndjson` exits 0.
- [ ] CLI smoke: `./bin/criteria apply examples/file_function.hcl`
      exits 0 (W07 example).
- [ ] CLI smoke: `./bin/criteria apply
      examples/for_each_review_loop.hcl` exits 0 (W08 example).

### Step 2 — Determinism gate

The Phase 1 stabilization promise was deterministic CI.
Re-prove it from a clean tree:

- [ ] `make test` runs 10/10 consecutive times locally without
      retry.
- [ ] `go test -race -count=20 ./internal/engine/...
      ./internal/plugin/...` green (the W01 flake watch).
- [ ] CI's `make test` step (with the `-count=2` from W01) is
      green on the PR branch and on `main` after merge.

If any flake reappears, do not commit; remediate against W01's
deliverables before continuing.

### Step 3 — Lint baseline burn-down gate

The per-workstream burn-down contract from W02 is the gate. Run
from `main` after all Phase 1 workstreams are merged:

- [ ] `.golangci.baseline.yml` has **zero** entries pointed at
      W03 (`# W03:` comment marker). Any remaining entry means
      W03 left a god-function un-refactored.
- [ ] `.golangci.baseline.yml` has **zero** entries pointed at
      W04 (`# W04:` comment marker). Any remaining entry means
      W04 left an oversized file unsplit.
- [ ] `.golangci.baseline.yml` has **zero** `revive`/`exported`
      entries pointed at W06 in `sdk/`, `workflow/`, `events/`,
      or `cmd/criteria/`. Any remaining entry means W06 left a
      public symbol undocumented.
- [ ] Any remaining entries are **explicitly approved** by this
      workstream's reviewer notes, with severity and the Phase
      they punt to. Examples: residual `revive`/`exported` in
      `internal/...` (acceptable; Phase 2), residual
      `gocyclo`/`funlen` in test files (acceptable; relaxed by
      the `_test.go` rule).

If the gate fails, do not commit; open a remediation PR against
the offending workstream's deliverables.

### Step 4 — Coverage / benchmark gate

The W06 thresholds:

- [ ] `make test-cover` reports `internal/cli/...` ≥ 60%.
- [ ] `make test-cover` reports `internal/run/...` ≥ 60%.
- [ ] `make test-cover` reports
      `cmd/criteria-adapter-mcp/...` ≥ 50%.
- [ ] `docs/perf/baseline-v0.2.0.md` exists and contains
      measured numbers from `make bench` for `workflow.Compile`,
      engine run (100 + 1000 step variants), and plugin
      `Execute` noop.

If any threshold is missed, do not commit; remediate against
W06's deliverables.

### Step 5 — Hygiene checks

- [ ] `git ls-files | grep -E '\.db(-(shm|wal))?$'` is empty.
- [ ] `grep -rn 'OVERSEER_' --include='*.go' .` returns no
      matches (Phase 0 rename gate, kept here as a regression
      guard).
- [ ] `grep -rn 'OVERLORD_\|CASTLE_\|PARAPET_' --include='*.go' .`
      returns no matches.
- [ ] `git ls-files cmd/overseer*/ proto/overseer/ sdk/pb/overseer/`
      returns no matches.
- [ ] No orphan files in `internal/cli/testdata/compile/` or
      `internal/cli/testdata/plan/` (every input has a paired
      golden).
- [ ] `git grep -nE 'TODO|FIXME|XXX' -- ':!workstreams/'
      ':!CHANGELOG.md'` count is recorded in reviewer notes.
      Acceptable count: ≤ 5 (the Phase 0 baseline was 3); each
      remaining entry must be a deliberate, documented
      forward-pointer.

### Step 6 — User-feedback accounting

Phase 1 addressed three of the eight user-feedback files:

- [W07](07-file-expression-function.md) →
  [user_feedback/01-support-file-function-user-story.txt](../user_feedback/01-support-file-function-user-story.txt)
- [W08](08-for-each-multistep.md) →
  [user_feedback/04-make-for-each-safe-for-multi-step-chains-user-story.txt](../user_feedback/04-make-for-each-safe-for-multi-step-chains-user-story.txt)
- [W09](09-copilot-agent-defaults.md) →
  `user_feedback/09-copilot-agent-defaults-user-story.txt`
  (authored by W09)

Tasks:

- [ ] Confirm each addressed user story has a corresponding
      `examples/` entry or test that validates the fix.
- [ ] The five remaining user-feedback files (02, 03, 05, 06,
      07, 08) are not addressed in Phase 1 by design. Author a
      pointer in `PLAN.md` "Deferred / forward-pointers" naming
      them as Phase 2 candidate scope. Do not move or rename
      the files.

### Step 7 — Documentation updates (the "files NOT to modify" set)

This workstream is the only one that may make structural edits
to:

- [ ] `README.md` — confirm post–Phase 1 state. Update the
      status banner to "v0.2.0"; add a one-line note that
      Phase 1 closed and the lint/test/coverage gates are now
      enforced. Cross-link to
      `docs/contributing/lint-baseline.md` (W02) and
      `docs/security/shell-adapter-threat-model.md` (W05).
- [ ] `PLAN.md` — tick every Phase 1 workstream checkbox.
      Update "Status snapshot" to "Phase 1 closed YYYY-MM-DD".
      Update Phase 1 section to a closed/archived state
      mirroring Phase 0's archived structure. Add a "Phase 2 —
      TBD" pointer plus a candidate-scope list (the five
      deferred user-feedback files, the platform-specific
      shell sandboxing `[ARCH-REVIEW]` from W05, the
      `DurableAcrossRestart` SDK conformance lift, the parallel
      regions / nested for_each items already noted as
      deferred). Add the archive footer line:
      `*Phase 1 closed YYYY-MM-DD. Archived under [workstreams/archived/v1/](workstreams/archived/v1/).*`
- [ ] `AGENTS.md` — sweep for any references that became stale
      during Phase 1 (e.g. high-value-files pointers if files
      moved during W04's split).
- [ ] `workstreams/README.md` — mark Phase 1 archived; list
      "Phase 2 — TBD". Remove the Phase 1 workstream index
      entries (they live in `archived/v1/` after the move).
- [ ] `CONTRIBUTING.md` — add a one-paragraph pointer to
      `docs/contributing/lint-baseline.md` and the burn-down
      contract. If `CONTRIBUTING.md` already exists, this is an
      append; do not restructure existing content.
- [ ] `CHANGELOG.md` — add the v0.2.0 release-notes entry.
      Headline: "Stabilization phase: deterministic CI,
      golangci-lint, shell adapter hardening, and three
      user-blocking fixes (file(), multi-step for_each,
      Copilot agent defaults)." Cover, in order:
      - W01 — deterministic CI (`-count=2`, `goleak`).
      - W02 — golangci-lint adoption with documented
        burn-down contract.
      - W03 — god-function refactor (no behavior change).
      - W04 — file splits in workflow/, conformance/, and
        server transport (no behavior change).
      - W05 — shell adapter first-pass hardening + threat
        model + `CRITERIA_SHELL_LEGACY=1` opt-out.
      - W06 — coverage + benchmark baselines + GoDoc on
        public packages.
      - W07 — `file()`, `fileexists()`, `trimfrontmatter()`
        expression functions + `CRITERIA_FILE_FUNC_MAX_BYTES`
        + `CRITERIA_WORKFLOW_ALLOWED_PATHS`.
      - W08 — multi-step `for_each` iteration bodies +
        `OnForEachStep` event.
      - W09 — Copilot `reasoning_effort` no longer silently
        dropped, per-step override semantics, targeted
        diagnostic for misplaced agent-config fields.
      - Migration notes for any HCL fixture that broke under
        the new W05/W08/W09 validation.

### Step 8 — Archive

- [ ] `mkdir -p workstreams/archived/v1/`
- [ ] `git mv workstreams/0[1-9]-*.md workstreams/archived/v1/`
- [ ] `git mv workstreams/10-*.md workstreams/archived/v1/`
- [ ] Update intra-workstream links if any reviewer notes
      referenced sibling files; otherwise leave the moved files
      unchanged (relative links between archived files still
      resolve).
- [ ] Re-run the lint baseline gate from Step 3 and the legacy-name
      hygiene gate from Step 5 to confirm the archive move did
      not surface anything outside the allowlist.

### Step 9 — Tagging

- [ ] After all checks above pass and the docs/archive are
      committed: `git tag -a v0.2.0 -m "Phase 1 stabilization
      and critical user fixes"`.
- [ ] Push the tag.
- [ ] If a release-asset workflow exists, confirm the v0.2.0
      tag triggers it and the assets land. If no release
      automation exists yet, the source tag is enough for
      `go install` consumers — note that in the release notes.

### Step 10 — Sibling-agent tuning (per cleanup-agent guidance)

The cleanup agent may apply **at most two directive
additions/removals each** to
[.github/agents/workstream-executor.agent.md](../.github/agents/workstream-executor.agent.md)
and
[.github/agents/workstream-reviewer.agent.md](../.github/agents/workstream-reviewer.agent.md),
strictly limited to drift observed during Phase 1.

If no drift, leave the agent files alone.

Likely candidates surfaced during Phase 1 implementation:

- Whether the burn-down contract from W02 needs to be encoded as
  a hard rule for the executor (currently lives in
  `docs/contributing/lint-baseline.md` only).
- Whether the "no new exported symbols" constraint from W04
  should be a checked agent-level invariant.

Cap at two changes per agent file. If more drift is observed,
capture it as Phase 2 planning input rather than agent-config
changes here.

### Step 11 — Optional: post-review

- [ ] (Optional) Author `arch_reviews/v1-postreview.md`
      capturing what shipped, what surprised the team during
      stabilization, what carries into Phase 2. The Phase 0
      analogue (`arch_reviews/v0-postreview.md`) was optional
      and skipped; this is also optional.

### Step 12 — Forward-pointer triage to PLAN.md

Consolidate the `[ARCH-REVIEW]` items from every Phase 1
reviewer note into a single Phase 2 candidate-scope list under
`PLAN.md` "Deferred / forward-pointers":

- Platform-specific shell sandboxing (W05).
- The five remaining user-feedback files (02, 03, 05, 06, 07,
  08).
- `DurableAcrossRestart` SDK conformance test (carried over
  from Phase 0).
- Parallel regions and sub-workflow composition.
- `@criteria/proto-ts` npm package (carried over from Phase 0).
- Any `[ARCH-REVIEW]` items recorded in W03/W04/W06/W07/W08/W09
  reviewer notes.

This is a triage list, not a commitment. Phase 2 planning
prioritizes from it.

## Out of scope

- Performing Phase 2 planning. The `Phase 2 — TBD` marker plus
  the candidate-scope list is enough; planning is a separate
  exercise.
- Any new feature work.
- Any structural refactor not already in flight from W01–W09.
- Adding the burn-down gate or coverage gate to CI as a
  permanent enforcement (already documented as manual at the
  cleanup gate; CI enforcement is a Phase 2 nice-to-have).

## Files this workstream may modify

This is the **only** Phase 1 workstream that may edit:

- `README.md`
- `PLAN.md`
- `AGENTS.md`
- `workstreams/README.md`
- `CONTRIBUTING.md`
- `CHANGELOG.md` (adds the v0.2.0 entry)
- `workstreams/01-*.md` … `workstreams/10-*.md` (only to move
  them into `archived/v1/`)
- `.github/agents/workstream-executor.agent.md` (Step 10, ≤ 2
  edits)
- `.github/agents/workstream-reviewer.agent.md` (Step 10, ≤ 2
  edits)

It also creates:

- `workstreams/archived/v1/` (new directory).
- `arch_reviews/v1-postreview.md` (optional).

This workstream may **not** add new source code, new tests, or
new behavior changes outside the documentation and archive
operations described above.

## Tasks

- [ ] Run every Build / lint / test check (Step 1).
- [ ] Run the determinism gate (Step 2).
- [ ] Run the lint baseline burn-down gate (Step 3).
- [ ] Run the coverage / benchmark gate (Step 4).
- [ ] Run hygiene checks (Step 5).
- [ ] User-feedback accounting per Step 6.
- [ ] Update the six docs in the coordination set, including
      `CHANGELOG.md` (Step 7).
- [ ] Move workstream files to `workstreams/archived/v1/`
      (Step 8).
- [ ] Final commit lands all of the above plus a one-paragraph
      summary in reviewer notes. Do not commit if any required
      validation fails.
- [ ] Tag `v0.2.0` and push (Step 9).
- [ ] (If justified) Apply minimal sibling-agent directive
      tuning (Step 10).
- [ ] (Optional) Author `arch_reviews/v1-postreview.md`
      (Step 11).
- [ ] Append the consolidated forward-pointer list to
      `PLAN.md` per Step 12.

## Exit criteria

- All checkboxes above ticked on `main`.
- `workstreams/` contains only `README.md`, `archived/`, and
  optionally a placeholder for Phase 2 planning.
- `README.md`, `PLAN.md`, `AGENTS.md`, `workstreams/README.md`,
  `CONTRIBUTING.md`, `CHANGELOG.md` all reflect the
  post–Phase 1 state.
- The lint baseline gate (Step 3) returns no W03/W04/W06
  entries.
- The coverage gate (Step 4) returns the documented thresholds.
- `v0.2.0` tag exists on `main` and is pushed.
- `make ci` is green at the tag.

## Tests

This workstream does not add new tests. The validation lanes
from W01–W09 plus the existing CI suite are the signal.

## Risks

| Risk | Mitigation |
|---|---|
| One of W01–W09 is "merged" but didn't actually achieve its exit criteria | This workstream re-runs every gating command, including the lint baseline gate, the coverage gate, and the determinism gate. If any fails, do not commit; open a remediation PR against the offending workstream's deliverables. |
| `v0.2.0` tag is cut prematurely, then a critical bug shows up | Acceptable — cut `v0.2.1` from the fix. Pre-1.0 tags are not stability promises. |
| Sibling-agent tuning over-corrects on a single observation | Cap at two directive add/removes per agent. If more drift is observed, capture it as a Phase 2 planning input. |
| `workstreams/archived/v1/` move loses cross-references | Intra-workstream links use relative paths; after the move, links between archived files still resolve (they all moved together). Cross-links from active files (`PLAN.md`, `CHANGELOG.md`) to archived files use `archived/v1/NN-…md` form; check those after the move. |
| Coordination-file updates drift from what W01–W09 actually shipped | Re-read each workstream's reviewer notes before authoring; cross-check claims against the post–Phase-1 repo state. |
| The lint baseline gate refuses to allow `v0.2.0` because a workstream legitimately couldn't burn down a particular entry | The gate accepts approved exceptions documented in this workstream's reviewer notes with severity and Phase-2-pointer. The expectation is that exceptions are rare; if more than two exist, treat that as a signal that one or more Phase 1 workstreams under-delivered and open a remediation PR rather than waving them through. |
| Phase 2 candidate-scope list grows into a Phase 2 plan during this workstream | Out of scope. The list is a triage input; planning is a separate exercise. |
| The CHANGELOG entry becomes a wall of text that nobody reads | The Step 7 spec gives a fixed structure (one bullet per workstream, in order). Stick to it. Detailed migration guidance lives in workstream reviewer notes; CHANGELOG names the headline. |
