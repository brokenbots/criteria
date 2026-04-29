# Workstream 8 — Contributor on-ramp (bus-factor mitigation)

**Owner:** Workstream executor · **Depends on:** [W01](01-lint-baseline-mechanical-burn-down.md) (so the first-PR walkthrough has live good-first-issue material).

## Context

The v0.2.0 tech evaluation
([tech_evaluations/TECH_EVALUATION-20260429-01.md](../tech_evaluations/TECH_EVALUATION-20260429-01.md)
section 5) puts **Maintainability at C+** primarily because of bus
factor:

```
git log --since="6 months ago" --pretty="%an" | sort | uniq -c
  133 Dave Sanderson
    2 dependabot[bot]
    1 Phase 1.1 Agent
    1 copilot-swe-agent[bot]
```

Zero merged human contributors other than the maintainer. The eval
explicitly recommends:

> Phase 2 should set a numeric goal.
>
> - Label 5 issues good-first-issue (the W04 lint fixes are excellent first PRs).
> - Write `docs/contributing/your-first-pr.md` with a concrete walkthrough.
> - Set a numeric goal (e.g., 2 non-author PRs merged by end of Phase 2) and report on it in the Phase 2 cleanup gate.

This workstream lands all three. It is documentation + repo hygiene;
no code changes.

## Prerequisites

- [W01](01-lint-baseline-mechanical-burn-down.md) merged. The first-PR
  walkthrough uses the residual W04 mechanical lint fixes as its
  worked example, so the baseline must already be partially burned
  down.
- `make ci` green on `main`.

## In scope

### Step 1 — Author `docs/contributing/your-first-pr.md`

A concrete walkthrough that takes a new contributor from zero to a
merged PR. Sections:

1. **Welcome and what to expect** — 2 paragraphs. Note that the repo
   uses an explicit per-workstream model and that small, single-file
   PRs are the norm.
2. **Pick an issue** — point at the `good-first-issue` label on the
   issue tracker; explain the labels in use.
3. **Set up your environment** — point at `CONTRIBUTING.md` for the
   `make bootstrap` flow. Do not duplicate.
4. **Worked example: a lint baseline burn-down PR** — pick a single
   residual `gofmt` or `goimports` entry from `.golangci.baseline.yml`
   and walk through:
   - Locate the file/line from the baseline entry.
   - Run `gofmt -w <file>` (or `goimports -w <file>`).
   - Remove the entry from `.golangci.baseline.yml`.
   - Lower `tools/lint-baseline/cap.txt` by 1 (per
     [W02](02-lint-ci-gate.md)).
   - Run `make ci`.
   - Open the PR with the linked good-first-issue.
5. **What the PR review looks like** — explain the workstream-reviewer
   role at a high level, that small PRs typically get a fast review,
   and what the contributor can expect (e.g. comments, possible R1/R2
   blocker tags, etc.).
6. **What to do next** — point at the issue tracker for further
   good-first-issue items and the larger workstream files in
   [workstreams/](../workstreams/) for structured contribution.

The doc should be ≤ 300 lines and read in one sitting. Use real file
paths and real commands; do not paraphrase.

### Step 2 — Label five `good-first-issue` items

Five issues on the GitHub repo, labeled `good-first-issue`, each with
a clear scope, file path, expected effort estimate (≤ 2 hours), and
an explicit "this is a good first contribution because..." line.

Candidates:

1. A specific gofmt/goimports baseline entry from
   [W01](01-lint-baseline-mechanical-burn-down.md) (the residual ≤ 40
   W04 entries — pick one of the easiest).
2. The `Stat().Mode().Perm() == 0o700` regression-test addition from
   [W04](04-state-dir-permissions.md) (if not already in scope when
   W04 lands; otherwise replace with another).
3. Adding a unit test for the `validateReasoningEffort` function in
   the new `copilot_util.go` ([W03](03-copilot-file-split-and-permission-alias.md))
   covering the four valid values plus an invalid one.
4. Documenting one of the existing example workflows in a header
   comment block (pick an `examples/*.hcl` that has no header comment
   today).
5. Adding an entry to `make help` for any target that lacks a `##`
   description.

If any of those five overlap with another in-flight workstream,
substitute equivalent low-risk tasks. The workstream executor must
file the issues themselves (using `gh issue create` or the GitHub
UI); document the issue numbers in reviewer notes.

### Step 3 — Update `CONTRIBUTING.md`

Add a short "First-time contributors" section at the top of
[CONTRIBUTING.md](../CONTRIBUTING.md) that:

- Links to `docs/contributing/your-first-pr.md`.
- Names the `good-first-issue` label.
- States the project's response-time target for a first PR (e.g.
  "the maintainer aims to review first-time contributor PRs within
  one week").

This is a small surgical edit — do not rewrite the existing content.

### Step 4 — Document the numeric goal in `PLAN.md`

The plan calls for "≥2 non-author humans land merged PRs by end of
Phase 2". `PLAN.md` is owned by the cleanup-gate agent
([W14](14-phase2-cleanup-gate.md)) — this workstream does **not**
edit `PLAN.md` directly. Instead, leave a clear paragraph in the
workstream's reviewer notes that W14 should copy into `PLAN.md`'s
Phase 2 section:

> Phase 2 contributor goal: ≥ 2 non-author humans land merged PRs by
> end of Phase 2. Source: tech eval section 5
> ([TECH_EVALUATION-20260429-01.md](tech_evaluations/TECH_EVALUATION-20260429-01.md)).
> Status reported in [W14](workstreams/14-phase2-cleanup-gate.md).

W14 is responsible for copying this into `PLAN.md` and reporting on
the actual count at phase close.

### Step 5 — Update issue templates if applicable

Inspect [.github/ISSUE_TEMPLATE/](../.github/ISSUE_TEMPLATE). If a
template covers good-first-issue intent (e.g. "Suggest a small
improvement"), leave it. If not, add a one-line note in the existing
templates pointing at the `good-first-issue` label and
`docs/contributing/your-first-pr.md`.

This is an optional polish step — skip if the templates already
serve. Document the choice in reviewer notes.

### Step 6 — Validate

- `make ci` green (no code change, but the doc must not break any
  existing link checker if one is configured).
- `docs/contributing/your-first-pr.md` reads cleanly end to end on
  GitHub's markdown rendering.
- All linked file paths and commands exist and execute.
- Five issues are filed and labeled.

## Behavior change

**No code behavior change.** Documentation + GitHub repo hygiene only.

- New file `docs/contributing/your-first-pr.md`.
- New section in `CONTRIBUTING.md`.
- Five new issues filed on GitHub (this is metadata, not repo
  content).
- Issue templates may gain a one-line addition.

No CLI flag, HCL surface, log, or runtime behavior is altered.

## Reuse

- Existing `CONTRIBUTING.md` structure. Insert; do not rewrite.
- Existing `docs/contributing/lint-baseline.md` — link to it from the
  first-PR walkthrough.
- Existing `Makefile` `help` target — the walkthrough should
  reference it as the source of truth for available commands.
- Existing `.github/ISSUE_TEMPLATE/` files — extend, do not replace.

## Out of scope

- Editing `PLAN.md`, `README.md`, `AGENTS.md`, `CHANGELOG.md`. Those
  are W14's domain; this workstream provides the source text for W14
  to copy.
- Onboarding the first non-author contributor. The goal is to *enable*
  contribution; actual recruitment happens organically.
- Mentoring program design. Out of scope for Phase 2.
- Rewriting `CONTRIBUTING.md`. Insert a section; do not refactor.
- A code-of-conduct file. If the project doesn't have one, that's a
  separate question — not in this workstream.

## Files this workstream may modify

- `docs/contributing/your-first-pr.md` (new).
- `CONTRIBUTING.md` (insert "First-time contributors" section near
  the top).
- `.github/ISSUE_TEMPLATE/*.md` (optional one-line additions; skip
  if not needed).

This workstream may **not** edit `README.md`, `PLAN.md`, `AGENTS.md`,
`CHANGELOG.md`, `workstreams/README.md`, or any other workstream file.
It may **not** edit any code under `internal/`, `cmd/`, `workflow/`,
`sdk/`, or `events/`.

## Tasks

- [ ] Author `docs/contributing/your-first-pr.md`.
- [ ] Insert the "First-time contributors" section in
      `CONTRIBUTING.md`.
- [ ] File five `good-first-issue` issues on GitHub; record numbers
      in reviewer notes.
- [ ] Optionally extend `.github/ISSUE_TEMPLATE/*.md` (skip if not
      needed; document choice).
- [ ] Provide the PLAN.md goal paragraph for [W14](14-phase2-cleanup-gate.md)
      in reviewer notes.
- [ ] `make ci` green.

## Exit criteria

- `docs/contributing/your-first-pr.md` exists, ≤ 300 lines, reads end
  to end, and contains a concrete worked example using a real lint
  baseline entry.
- `CONTRIBUTING.md` has a "First-time contributors" section that
  links to the new doc.
- Five GitHub issues labeled `good-first-issue` with the documented
  shape (file path, effort estimate, scope statement).
- W14 has a clear paragraph to copy into `PLAN.md` for the Phase 2
  contributor goal.
- `make ci` green.

## Tests

This workstream does not add tests. Verification is human reading +
clicking the GitHub issue links.

## Risks

| Risk | Mitigation |
|---|---|
| The five labeled issues get claimed by no one | The goal is *enablement*, not guaranteed contribution. W14 reports the actual contributor count at phase close; if the goal is missed, Phase 3 inherits a follow-up workstream that addresses why (visibility, scope, friction). |
| The first-PR walkthrough goes stale as W01/W02 land follow-ups | Date the doc with the Phase 2 tag and add a "last reviewed" line. Future workstreams that change the lint flow update the doc as part of their own scope. |
| Filed issues collide with W14's archival sweep | W14 archives workstream files, not GitHub issues. No collision. |
| The contributor sets up a fork and hits a setup snag not covered by the walkthrough | The walkthrough explicitly defers to `CONTRIBUTING.md` for setup; if `CONTRIBUTING.md` is wrong, fix it as part of this workstream's scope (it's allowed to edit). |
