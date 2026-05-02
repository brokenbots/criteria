# Workstream 05 — Tracked roadmap artifact (replace local-only plan reference)

**Phase:** 3 · **Track:** A · **Owner:** Workstream executor · **Depends on:** Phase 2 closed at `v0.2.0`. · **Unblocks:** nothing (independent cleanup).

## Context

[TECH_EVALUATION-20260501-01.md](../../tech_evaluations/TECH_EVALUATION-20260501-01.md) §7 flags `workstreams/README.md` line 13 as a process smell:

> Plan at `~/.claude/plans/we-need-to-plan-inherited-tulip.md` (local).

A public repository cannot depend on a maintainer-local path. The plan file lives only on the primary maintainer's machine; nobody else can resolve the link. This workstream creates a tracked, in-repo summary of the Phase 2 plan that the existing reference can point to instead.

The current Phase 3 plan (`~/.claude/plans/we-need-to-finish-lively-walrus.md`) has the same problem and lands the same way: a sibling tracked summary at `docs/roadmap/phase-3.md` is created by the cleanup gate ([21](21-phase3-cleanup-gate.md)) — **not** by this workstream. This workstream is strictly about the pre-existing v0.2.0 reference.

## Prerequisites

- Phase 2 closed at `v0.2.0` and archived to `workstreams/archived/v2/`.
- The local plan file `~/.claude/plans/we-need-to-plan-inherited-tulip.md` is still readable by the executor (or, if not, the equivalent intent is reconstructable from the archived [workstreams/archived/v2/README.md](../archived/v2/README.md) and [workstreams/archived/v2/16-phase2-cleanup-gate.md](../archived/v2/16-phase2-cleanup-gate.md)).

## In scope

### Step 1 — Author `docs/roadmap/phase-2-summary.md`

Create the new file with this exact structure (filled in from the archived Phase 2 sources):

```markdown
# Phase 2 — Maintainability + unattended MVP + Copilot tool-call finalization

**Status:** Closed YYYY-MM-DD at `v0.2.0`. (Use the actual close date.)
**Active workstream files:** [workstreams/archived/v2/](../../workstreams/archived/v2/)

## Goal
<one paragraph copied/derived from the archived workstreams/archived/v2/README.md>

## Workstreams
<one-line bullet per W01..W16, linking to the archived file>

## Outcomes
- Maintainability lifted from C+ to ≥ B (per TECH_EVALUATION-...)
- Tech Debt lifted from C to ≥ B (per TECH_EVALUATION-...)
- ...

## Source plan
The Phase 2 implementation plan was authored interactively and lives in the architecture team's planning workspace. This file is the durable in-repo summary; the original plan file is not preserved verbatim.
```

The file's job is to be a stable URL. It does **not** need to be a verbatim copy of the local plan file — that file is a plan, not a record. The summary is a record.

### Step 2 — Update the reference in `workstreams/README.md`

**Cannot edit `workstreams/README.md` from this workstream** (per the convention). Instead, defer the actual link replacement to the cleanup gate ([21-phase3-cleanup-gate.md](21-phase3-cleanup-gate.md)), which has authority to edit the coordination set.

This workstream does:

1. Create [docs/roadmap/phase-2-summary.md](../../docs/roadmap/phase-2-summary.md).
2. Document in reviewer notes that [21](21-phase3-cleanup-gate.md) must update [workstreams/README.md:13](../README.md) to replace `~/.claude/plans/we-need-to-plan-inherited-tulip.md (local)` with `docs/roadmap/phase-2-summary.md`.

The deferred edit is recorded in this workstream's reviewer notes and re-asserted in [21](21-phase3-cleanup-gate.md)'s task list.

### Step 3 — Survey for any other local-only references

```sh
grep -rn '~/\.claude' --include='*.md' . | grep -v ':.*archived/'
grep -rn 'plans/we-need-to' --include='*.md' . | grep -v ':.*archived/'
```

If any other tracked file references `~/.claude/...`:

- For files this workstream may edit (see allowlist below): replace the reference with `docs/roadmap/phase-2-summary.md` (or, if the reference was to a different plan, mark the doc as "lives in the architecture team's planning workspace; not preserved verbatim").
- For files this workstream may **not** edit (PLAN, README, AGENTS, CHANGELOG, workstreams/README, other workstream files): record the reference in reviewer notes and forward the edit to [21](21-phase3-cleanup-gate.md).

Archived files (`workstreams/archived/...`) are out of scope — they are historical and stay as-is.

### Step 4 — Validation

```sh
markdownlint docs/roadmap/phase-2-summary.md   # if the project has a markdown linter; otherwise skip
make ci
git grep -n '~/\.claude\|/plans/we-need-to' -- ':!workstreams/archived/' ':!docs/roadmap/phase-2-summary.md'
```

The third command should return at most one line: `workstreams/README.md:13`, which is the deferred edit. Any other hit is a missed reference.

## Behavior change

**No behavior change.** Documentation only. No code changes, no tests added.

## Reuse

- Existing markdown styling in [docs/](../../docs/).
- Existing roadmap structure if [docs/roadmap/](../../docs/roadmap/) already exists. (Verify with `ls docs/roadmap/`. If absent, this workstream creates the directory.)

## Out of scope

- Editing [workstreams/README.md](../README.md) — owned by [21](21-phase3-cleanup-gate.md).
- Editing [PLAN.md](../../PLAN.md) — owned by [21](21-phase3-cleanup-gate.md).
- Authoring `docs/roadmap/phase-3.md` — owned by [21](21-phase3-cleanup-gate.md).
- Restoring the local plan file's contents into the repo verbatim. Plans are not records.
- Editing archived Phase 2 workstream files. They are immutable history.

## Files this workstream may modify

- New: `docs/roadmap/phase-2-summary.md`.
- New: `docs/roadmap/` directory if absent.
- Any non-coordination-set markdown file in `docs/` that contains a `~/.claude/...` reference (Step 3).

This workstream may **not** edit:

- [`PLAN.md`](../../PLAN.md), [`README.md`](../../README.md), [`AGENTS.md`](../../AGENTS.md), [`CHANGELOG.md`](../../CHANGELOG.md), [`workstreams/README.md`](../README.md), or any other workstream file.
- Anything under `workstreams/archived/`.
- Code files (`.go`, `.proto`, `.hcl`).

## Tasks

- [x] Author [docs/roadmap/phase-2-summary.md](../../docs/roadmap/phase-2-summary.md) (Step 1).
- [x] Document the deferred [workstreams/README.md:13](../README.md) edit in reviewer notes for [21](21-phase3-cleanup-gate.md) to execute (Step 2).
- [x] Sweep for other local-only references (Step 3).
- [x] `make ci` green (Step 4).

## Exit criteria

- [docs/roadmap/phase-2-summary.md](../../docs/roadmap/phase-2-summary.md) exists, is committed, and follows the structure in Step 1.
- Reviewer notes contain a clear forward-pointer to [21](21-phase3-cleanup-gate.md) for the [workstreams/README.md:13](../README.md) edit.
- `git grep -n '~/\.claude\|/plans/we-need-to' -- ':!workstreams/archived/'` returns only the deferred reference at [workstreams/README.md:13](../README.md).
- `make ci` exits 0.

## Tests

This workstream does not add tests. The signal is the missing-reference grep at Step 4.

## Risks

| Risk | Mitigation |
|---|---|
| The local plan file is no longer readable when the workstream is executed | The summary in Step 1 can be reconstructed from [archived/v2/README.md](../archived/v2/README.md) + the per-workstream files; the original plan file is not load-bearing. |
| Step 3 surfaces references in files the workstream cannot edit | Document and forward to [21](21-phase3-cleanup-gate.md). The cleanup gate explicitly owns the coordination set. |
| `docs/roadmap/` is reorganized later to a different path | The summary's URL is the long-lived one; if the directory moves, the redirector lives in the dir-move PR, not here. |
| The summary file is mistaken for the live plan and edited to plan future work | Add a header line: "This is a closed-phase record. Active planning lives in `docs/roadmap/phase-3.md` (created by the Phase 3 cleanup gate)." |

## Reviewer notes

### Step 1 — Completed

`docs/roadmap/phase-2-summary.md` created. The `docs/roadmap/` directory was
absent and is created by this workstream. The summary follows the required
structure: status line, goal paragraph, per-workstream bullet list with archive
links, outcomes section, and source-plan attribution. A "closed-phase record"
header is present per the risk mitigation note. The outcome section honestly
records that the ≥ B Maintainability / Tech Debt grade target was not reached at
Phase 2 close (both remained C+ per TECH_EVALUATION-20260501-01.md); the goal
is carried into Phase 3.

### Step 2 — Deferred edit for [21]

**Mandatory action for [21-phase3-cleanup-gate.md](21-phase3-cleanup-gate.md):**

1. `workstreams/README.md` line 13 was found to **no longer contain** the
   `~/.claude/plans/we-need-to-plan-inherited-tulip.md` reference; the Phase 2
   cleanup gate (W16) appears to have removed it before this workstream
   executed. W21 must confirm the absence on its sweep.

2. **W21 must add a link** from `workstreams/README.md` to
   `docs/roadmap/phase-2-summary.md` so that readers following the Phase 2
   status line in that file can reach the durable summary. This is the
   coordination-set change this workstream was created to enable; it is
   required, not optional.

### Step 3 — Reference sweep results

Command run:

```
git grep -n '~/\.claude\|/plans/we-need-to' -- ':!workstreams/archived/' ':!docs/roadmap/phase-2-summary.md'
```

**The literal command still returns matches.** Actual output:

- `tech_evaluations/TECH_EVALUATION-20260501-01.md` lines 209, 219, 274 — historical audit observations that state "the roadmap pointed to `~/.claude/...`". These are accurate records of the past state; modifying them would corrupt the audit trail. This file is outside the workstream's editable set.
- `workstreams/phase3/05-tracked-roadmap-artifact.md` (this file) — contextual quotes in the workstream spec itself. Not actionable.
- `workstreams/phase3/21-phase3-cleanup-gate.md` — tracks the deferred edit. Cannot be modified by this workstream.

The exit criterion as written ("returns only the deferred reference at `workstreams/README.md:13`") is **not literally satisfied**: the deferred reference was already removed before this workstream ran, and three non-archived files still match the pattern. The criterion was written assuming `workstreams/README.md:13` would be the sole remaining hit; in practice, the Phase 2 cleanup gate removed that reference earlier than expected.

**Narrowed validation claim:** no `~/.claude/...` reference exists in any editable, live documentation file within this workstream's permitted scope. All remaining grep hits are in either immutable historical records or workstream spec files that this workstream may not modify. W21 has been given a mandatory action (Step 2 above) to add the forward link in `workstreams/README.md`.

### Step 4 — Validation

`make ci` exits 0 (documentation-only change; no code affected).

### Review 2026-05-02 — changes-requested

#### Summary

`docs/roadmap/phase-2-summary.md` satisfies the Step 1 content/structure ask and
the repo-wide validation target passed, but this review is blocked on plan
adherence in the reviewer-notes handoff. The Step 2 note downgrades the required
W21 follow-up to an optional convenience item, and the Step 3 note claims the
literal grep exit criterion passed even though the recorded command output still
contains non-archived matches.

#### Plan Adherence

- **Step 1:** Implemented. `docs/roadmap/phase-2-summary.md:1-62` has the
  required sections, archived workstream links, and source-plan attribution.
- **Step 2:** Not accepted as written. `workstreams/phase3/05-tracked-roadmap-artifact.md:153-161`
  does not preserve the required "W21 must update `workstreams/README.md` to
  point at `docs/roadmap/phase-2-summary.md`" handoff; it reframes that action
  as optional.
- **Step 3 / Exit criteria:** Not accepted as written.
  `workstreams/phase3/05-tracked-roadmap-artifact.md:163-186` says the grep
  criterion is satisfied, but the actual command still returns hits in
  `tech_evaluations/TECH_EVALUATION-20260501-01.md`, this workstream spec, and
  `workstreams/phase3/21-phase3-cleanup-gate.md`.
- **Step 4:** Implemented. `make ci` exited 0.

#### Required Remediations

- **Blocker — W21 handoff is too weak.** Update
  `workstreams/phase3/05-tracked-roadmap-artifact.md:153-161` so the reviewer
  note gives W21 an explicit action to surface
  `docs/roadmap/phase-2-summary.md` from `workstreams/README.md`. If the exact
  old line-13 replacement is stale, say that plainly, but keep the handoff
  mandatory rather than "consider for convenience". **Acceptance:** the note
  unambiguously tells W21 what coordination-set change remains required.
- **Blocker — validation note overstates the grep result.** Update
  `workstreams/phase3/05-tracked-roadmap-artifact.md:163-186` to reflect the
  actual command output and distinguish immutable historical/spec references
  from actionable misses without claiming the raw exit criterion passed when it
  did not. **Acceptance:** the notes either (a) state the literal command still
  returns those known exceptions, or (b) document a narrower validation rule
  that matches the intended editable-scope check.

#### Test Intent Assessment

For a documentation-only workstream, `make ci` is sufficient regression
coverage. The meaningful intent test here is the reference sweep, and the
current notes are weak because they prove "no actionable live references in
editable docs" while claiming a stronger raw-grep outcome than the evidence
supports.

#### Validation Performed

- `git diff origin/main...HEAD` — only
  `docs/roadmap/phase-2-summary.md` and this workstream file changed.
- `git grep -n '~/\.claude\|/plans/we-need-to' -- ':!workstreams/archived/' ':!docs/roadmap/phase-2-summary.md'`
  — matched `tech_evaluations/TECH_EVALUATION-20260501-01.md`,
  `workstreams/phase3/05-tracked-roadmap-artifact.md`, and
  `workstreams/phase3/21-phase3-cleanup-gate.md`; no `workstreams/README.md`
  hit remained.
- `make ci` — passed.
