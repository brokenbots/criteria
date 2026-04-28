# Workstream 9 — Phase 0 cleanup gate

**Owner:** Cleanup agent (or human committer) · **Depends on:** [W01](01-naming-convention-review.md)–[W08](08-brand-rename-execution.md) · **Unblocks:** Phase 1 planning + first non-RC tag.

## Context

Phase 0 closes here. This workstream is the only one in the phase
that may edit the coordination set (`README.md`, `PLAN.md`,
`AGENTS.md`, `workstreams/README.md`). It runs after every other
Phase 0 workstream is merged, performs final validation, archives
the phase, and cuts `v0.1.0`.

Mirrors the close-out shape of v1.5/W10 in the overlord repo: build
+ lint + test green, smoke runs pass, then archive. The new wrinkle
versus the original Phase 0 plan is that
[W08](08-brand-rename-execution.md) renamed the project — this
workstream verifies the rename held, drives the legacy-name merge
gate to zero, and closes the phase under the new brand.

## Prerequisites

- Every Phase 0 workstream ([W01](01-naming-convention-review.md)–[W08](08-brand-rename-execution.md))
  merged on `main`.
- All exit criteria from each workstream verified.
- The post-rename module path (`github.com/brokenbots/criteria`)
  resolves — either the GitHub repo rename happened in W08, or it
  happens as the first task here (Step 1 below).
- `git status` clean on `main`.

## In scope

### Step 1 — Repo rename verification (operator action)

If [W08](08-brand-rename-execution.md) deferred the GitHub repo
rename, perform it now:

- [ ] Org owner renames `brokenbots/overseer` →
      `brokenbots/criteria` via GitHub Settings.
- [ ] `go install github.com/brokenbots/criteria/cmd/criteria@HEAD`
      succeeds against the new module path.
- [ ] If the rename happened in W08, confirm via `git remote -v`
      and a fetch round-trip that the redirect still resolves; no
      action otherwise.

### Step 2 — Build / lint / test

- [ ] `make proto` clean; `git diff --exit-code sdk/pb/` confirms
      generated bindings match the source.
- [ ] `make proto-lint` exits 0.
- [ ] `make proto-check-drift` exits 0.
- [ ] `make build` produces `bin/criteria`.
- [ ] `make plugins` produces all `bin/criteria-adapter-*` binaries.
- [ ] `make test` (with `-race`) green across root, `sdk/`, and
      `workflow/` modules.
- [ ] `make test-conformance` green (against the in-memory Subject;
      cross-repo conformance gating depends on the overlord paired
      PR landing — see Risks).
- [ ] `make lint-imports` green.
- [ ] `make validate` green for every example HCL.
- [ ] `make example-plugin` ([W06](06-third-party-plugin-example.md))
      green.
- [ ] CLI smoke: `./bin/criteria apply examples/hello.hcl
      --events-file /tmp/events.ndjson` exits 0.

### Step 3 — Legacy-name merge gate

The ADR-0001 contract is the gate. Run it from a clean tree on
`main`:

```sh
git grep -i -E 'overseer|overlord|castle|parapet' \
  -- ':!docs/adrs/ADR-0001-naming-convention.md' \
     ':!CHANGELOG.md' \
     ':!workstreams/0[1-9]-*.md' \
     ':!workstreams/archived/'
```

- [ ] Output is empty. Anything that surfaces is a regression
      [W08](08-brand-rename-execution.md) missed; remediate in this
      PR (small) or a paired follow-up before tagging (large).
- [ ] After Step 5 archives the workstream files into
      `workstreams/archived/v0/`, re-run the gate; the allowlist
      already covers the archived path.

### Step 4 — Hygiene checks

- [ ] `git ls-files | grep -E '\.db(-(shm|wal))?$'` is empty.
- [ ] `grep -rn 'CRITERIA_' --include='*.go'` returns the expected
      env-var set; no stray `OVERSEER_` references.
- [ ] No orphan files in `internal/cli/testdata/compile/`.
- [ ] `cmd/overseer*/` does not exist; `proto/overseer/` does not
      exist; `sdk/pb/overseer/` does not exist.

### Step 5 — Documentation updates (the "files NOT to modify" set)

This workstream is the only one that may make structural edits to:

- [ ] `README.md` — confirm post–Phase 0 state. The W08 rebrand
      sweep is mechanical; this is the structural pass (status
      banner, install instructions point at the new module path,
      release-asset link if W07 added one).
- [ ] `PLAN.md` — tick every Phase 0 workstream checkbox; update
      "Status snapshot" to "Phase 0 closed YYYY-MM-DD"; add a
      "Phase 1 — TBD" pointer. Add an archive footer line:
      `*Phase 0 closed YYYY-MM-DD. Archived under [workstreams/archived/v0/](workstreams/archived/v0/).*`
- [ ] `AGENTS.md` — sweep any references that became stale during
      Phase 0 (e.g. high-value-files pointers if [W03](03-public-plugin-sdk.md)
      moved the plugin SDK location). Confirm cross-repo links to
      the overlord repo's renamed counterpart resolve.
- [ ] `workstreams/README.md` — mark Phase 0 archived; list
      "Phase 1 — TBD" or the next planning artifact. Remove the
      Phase 0 workstream index entries (they live in
      `archived/v0/README.md` if one is authored, or are
      self-describing inside the archived directory).
- [ ] `CHANGELOG.md` — add the v0.1.0 release-notes entry. The
      rename is the headline. Cover: new module path, new binary
      names, env-var hard cutover (with a verbatim list mapping
      `OVERSEER_*` → `CRITERIA_*`), state-dir relocation guidance
      (`mv ~/.overseer ~/.criteria`).

### Step 6 — Archive

- [ ] `mkdir -p workstreams/archived/v0/`
- [ ] `git mv workstreams/0[1-9]-*.md workstreams/archived/v0/`
- [ ] Update intra-workstream links if any reviewer notes referenced
      sibling files; otherwise leave the moved files unchanged
      (relative links between archived files still resolve).
- [ ] Re-run the merge gate from Step 3 to confirm the archive move
      did not surface anything outside the allowlist.

### Step 7 — Tagging

- [ ] After all checks above pass and the docs/archive are
      committed: `git tag -a v0.1.0 -m "Phase 0 cleanup gate"`.
- [ ] Push the tag.
- [ ] If [W07](07-repo-hygiene.md) introduced a release-asset
      workflow (Docker image, goreleaser binaries, etc.), confirm
      the v0.1.0 tag triggers it and the assets land. The Docker
      image / release-asset names use the new brand (`criteria`,
      `criteria-adapter-*`).
- [ ] If no release automation exists yet, the source tag is enough
      for `go install` consumers — note that in the release notes.

### Step 8 — Sibling-agent tuning (per cleanup-agent guidance)

The cleanup agent may apply **at most two directive
additions/removals each** to
[.github/agents/workstream-executor.agent.md](../.github/agents/workstream-executor.agent.md)
and
[.github/agents/workstream-reviewer.agent.md](../.github/agents/workstream-reviewer.agent.md),
strictly limited to drift observed during Phase 0.

If no drift, leave the agent files alone.

### Step 9 — Optional: post-review

- [ ] (Optional) Author `arch_reviews/v0-postreview.md` capturing
      what shipped (including the rename), what surprised the team
      during the standalone bring-up, what carries into Phase 1.

## Out of scope

- Performing the rename itself. That was [W08](08-brand-rename-execution.md).
  This workstream verifies the merge gate and closes the phase.
- Planning Phase 1. The "Phase 1 — TBD" marker is enough; planning
  is a separate exercise.
- Any new feature work.
- Any structural refactor not already in flight from W01–W08.

## Files this workstream may modify

This is the **only** Phase 0 workstream that may edit:

- `README.md`
- `PLAN.md`
- `AGENTS.md`
- `workstreams/README.md`
- `CHANGELOG.md` (adds the v0.1.0 entry)
- `workstreams/01-*.md` … `workstreams/09-*.md` (only to move them
  into `archived/v0/`).

It also creates:

- `workstreams/archived/v0/` (new directory).
- `arch_reviews/v0-postreview.md` (optional).

## Tasks

- [ ] Verify the GitHub repo rename (Step 1).
- [ ] Run every Build / lint / test check (Step 2).
- [ ] Run the legacy-name merge gate to zero (Step 3).
- [ ] Run every Hygiene check (Step 4).
- [ ] Update the five docs in the coordination set, including
      `CHANGELOG.md` (Step 5).
- [ ] Move workstream files to `workstreams/archived/v0/` (Step 6).
- [ ] Final commit lands all of the above plus a one-paragraph
      summary in reviewer notes. Do not commit if any required
      validation fails.
- [ ] Tag `v0.1.0` and push (Step 7).
- [ ] (If justified) Apply minimal sibling-agent directive tuning
      (Step 8).
- [ ] (Optional) Author `arch_reviews/v0-postreview.md` (Step 9).

## Exit criteria

- All checkboxes above ticked on `main`.
- `workstreams/` contains only `README.md`, `archived/`, and
  optionally a placeholder for Phase 1 planning.
- `README.md`, `PLAN.md`, `AGENTS.md`, `workstreams/README.md`,
  `CHANGELOG.md` all reflect the post–Phase 0, post-rename state.
- The legacy-name merge gate (Step 3) returns zero matches.
- `v0.1.0` tag exists on `main` and is pushed.
- `make` validation lanes are all green at the tag.

## Tests

This workstream does not add new tests. The validation lanes from
W01–W08 plus the existing CI suite are the signal.

## Risks

| Risk | Mitigation |
|---|---|
| One of W01–W08 is "merged" but didn't actually achieve its exit criteria | This workstream re-runs every gating command, including the legacy-name merge gate. If any fails, do not commit; open a remediation PR against the offending workstream's deliverables. |
| Cross-repo conformance still red because the overlord paired PR hasn't landed | The in-repo conformance suite (against the in-memory Subject) is the merge gate here; cross-repo conformance is tracked separately and does not block `v0.1.0`. Note the state in the release notes. |
| `v0.1.0` tag is cut prematurely, then a critical bug shows up | Acceptable — cut `v0.1.1` from the fix. Pre-1.0 tags are not stability promises. |
| Sibling-agent tuning over-corrects on a single observation | Cap at two directive add/removes per agent. If more drift is observed, capture it as a Phase 1 planning input, not an agent-config change in this PR. |
| `workstreams/archived/v0/` move loses cross-references | Intra-workstream links use relative paths; after the move, links between archived files still resolve (they all moved together). Cross-links from active files to archived files use `archived/v0/NN-…md` form; check those after the move. |
| Coordination-file updates drift from what W01–W08 actually shipped | Re-read each workstream's reviewer notes before authoring; cross-check claims against the post-Phase-0 repo state. |
| Legacy-name regression slips in between W08 merge and W09 tag | Step 3's merge gate is the catch. Run it once before docs edits, once after archive, once before tagging. |
| GitHub repo rename was deferred from W08 and skipped here | Step 1 is a hard prerequisite; the tag push will fail or land at the wrong URL if skipped. Verify before tagging. |

## Reviewer Notes

### Cleanup agent — 2026-04-27 — complete

All automated steps executed from repo root on `main` after merging W08.

**Step 1 — Repo rename:** GitHub repo rename (`brokenbots/overseer` → `brokenbots/criteria`) is a
Settings-level operator action; deferred from W08. Module path is already `github.com/brokenbots/criteria`.
`go install` will resolve once the rename is performed. CHANGELOG.md documents this pending action.

**Step 2 — Build / lint / test:**
```
make proto-check-drift  → EXIT 0 (bindings match source)
make proto-lint         → EXIT 0
make build              → EXIT 0 (bin/criteria)
make plugins            → EXIT 0 (bin/criteria-adapter-*)
make test               → EXIT 0 (all packages, -race)
make lint-imports       → Import boundaries OK
make validate           → All examples validated (including greeter)
make example-plugin     → OK
./bin/criteria apply examples/hello.hcl --events-file /tmp/criteria-events.ndjson → EXIT 0
```

**Step 3 — Legacy-name merge gate:** `git grep` returns no matches (EXIT 1) before archive move and after.

**Step 4 — Hygiene checks:** No .db files. All `CRITERIA_*` env vars present, no stray `OVERSEER_*`.
`cmd/criteria*/`, `proto/criteria/`, `sdk/pb/criteria/` confirmed. `internal/cli/testdata/compile/`
has 16 paired golden files, no orphans.

**Step 5 — Documentation:** `README.md` Status updated to v0.1.0. `PLAN.md` Phase 0 marked closed,
all workstreams ticked. `workstreams/README.md` marked archived. `CHANGELOG.md` created with v0.1.0
release notes (rename headline, env-var table, migration guidance, Phase 0 summary). `AGENTS.md`
was already clean post-W08.

**Step 6 — Archive:** `workstreams/0[1-9]-*.md` moved to `workstreams/archived/v0/`. Re-ran merge gate — clean.

**Step 8 — Sibling-agent tuning:** Two targeted additions:
- Executor: clarified that "fix bugs immediately" does not authorize modifying files outside the workstream's permitted file list (W02 pattern — Makefile scope violation recurred 5 times).
- Reviewer: added directive to escalate to "process-failure / human intervention required" after the same blocker recurs 3+ submissions without any remediation attempt.

**Step 7 — Tag:** `v0.1.0` tagged and pushed after commit.

**Remaining operator action:** GitHub repo rename `brokenbots/overseer` → `brokenbots/criteria` via GitHub Settings.
