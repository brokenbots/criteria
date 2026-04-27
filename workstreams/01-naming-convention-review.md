# Workstream 1 — Naming convention review

**Owner:** Tech-evaluator (or human reviewer) · **Depends on:** none · **Unblocks:** [W02](02-readme-and-contributor-docs.md), [W07](07-repo-hygiene.md).

## Context

Internal adoption is picking up and colleagues are pushing for public
releases. The current branding — "overseer" (executor), "overlord"
(orchestrator), "castle" (server), "parapet" (UI) — was chosen for its
internal coherence as a fantasy/military metaphor. Several of those
words read poorly in corporate / regulated environments:

- "overseer" carries historical connotations in US English that some
  organisations explicitly avoid.
- "overlord" reads as authoritarian / militaristic.
- "castle" / "parapet" are coherent but only inside the metaphor; they
  carry no signal about what the components actually do.

This workstream **does not rename anything**. Its job is to produce a
written decision — keep the current names, rename, or partial rename —
so later workstreams (README rewrite, repo hygiene, public release)
can carry consistent framing. The decision itself is the deliverable;
execution of any rename happens in a later phase.

The window is now: while the only consumer is the overlord team, the
cost of a rename is one paired PR. Once external consumers exist, the
cost grows quickly.

## Prerequisites

- None (this is the first workstream in Phase 0).

## In scope

### Step 1 — Inventory the user-visible surface

Catalogue every place a name appears in user-visible text:

- Module path (`github.com/brokenbots/overseer`).
- Binary name (`overseer`, `overseer-adapter-*`).
- Env vars (`OVERSEER_PLUGINS`, `OVERSEER_PLUGIN`, `OVERSEER_COPILOT_BIN`, `OVERSEER_COPILOT_INCLUDE_SENSITIVE_PERMISSION_DETAILS`).
- Default state dir (`~/.overseer/`).
- Proto package (`overseer.v1`).
- Docker image name (none yet — relevant only if W08 publishes one).
- README, AGENTS.md, CONTRIBUTING.md prose.
- HCL workflow language references (none use the brand name today; verify).
- Generated TS bindings (none yet).

### Step 2 — Evaluate options

At least three options should be on the table:

1. **Keep "overseer" as-is.** Document the rationale; close the door.
2. **Rename to a neutral, descriptive name** (e.g. `runflow`, `wfx`,
   `flowcli`). Cost: paired PR with overlord; one-time disruption.
3. **Rename only the user-visible parts** (binary name, brand) but
   keep `overseer` as the Go module path (cheap, but creates a
   permanent skew between marketing name and import path).

For each option, evaluate:

- Word-association concerns in target environments (US/EU corp,
  regulated industries, public open-source visibility).
- Migration cost (this repo + overlord repo + any internal docs).
- Search/SEO clarity vs the existing `overseer` ecosystem on GitHub.
- Whether the name is registrable as an npm scope and a Docker Hub
  org if those become relevant.

### Step 3 — Recommend, document, decide

Author **`docs/adrs/ADR-0001-naming-convention.md`** as the first ADR
in this repo. The ADR follows the
[lightweight ADR template](https://github.com/joelparkerhenderson/architecture-decision-record):

- Status (Proposed / Accepted / Superseded).
- Context (this workstream's "Context" section, condensed).
- Decision (the chosen option).
- Consequences (what changes, what doesn't, what work this unblocks
  and blocks).

If the decision is "rename", the ADR also lists the names to be used
and points at the Phase that will execute the rename. The rename is
**not** scheduled in Phase 0 unless this workstream's recommendation
is "rename now and bundle it into Phase 0"; in that case W02 and W07
inherit the new names from this ADR.

## Out of scope

- Performing any rename. That is a separate phase if the ADR
  recommends one.
- Renaming the overlord repo. Coordinate with the overlord team if
  this ADR's decision implies a rename there too.
- Branding work beyond names (logo, marketing site, etc.).

## Files this workstream may modify

- `docs/adrs/ADR-0001-naming-convention.md` (new file).
- `docs/adrs/README.md` (new file — index of ADRs in this repo).

This workstream may **not** edit `README.md`, `AGENTS.md`,
`CONTRIBUTING.md`, `PLAN.md`, or any other workstream file. If the
ADR recommends a rename, downstream workstreams (W02, W07) consume
the ADR by reference; they do not embed its conclusions until they
themselves run.

## Tasks

- [x] Inventory the user-visible naming surface.
- [x] Evaluate at least three options against the criteria above.
- [x] Author `docs/adrs/ADR-0001-naming-convention.md`.
- [x] Author `docs/adrs/README.md` as a one-line ADR index.
- [ ] Mark the ADR `Accepted` once a human reviewer signs off; do not
      merge in `Proposed` state.

## Exit criteria

- ADR-0001 exists, is in `Accepted` state, and clearly states whether
  any rename is happening, when, and what's renamed vs left alone.
- `docs/adrs/README.md` lists ADR-0001.
- No code changes.

## Tests

None. This workstream is documentation-only.

## Risks

| Risk | Mitigation |
|---|---|
| Bikeshed risk: naming discussions go in circles | Time-box to one round of options + one round of feedback. The reviewer signing off the ADR is the tiebreaker. |
| ADR claims "no rename needed" but a downstream workstream still uses the wrong tone | W02 (README) explicitly checks the ADR's conclusions when it lands, even if the conclusion is "keep current names". |
| Recommending a rename without the overlord team agreeing | Loop the overlord team in before marking the ADR Accepted. The decision is bilateral. |

## Executor notes

**Tasks 1–4 complete.** All four executable tasks were already delivered:

- **Naming surface inventory** — `docs/adrs/ADR-0001-naming-convention.md`
  Appendix A catalogues every user-visible surface: Go module paths, binary
  names, all 15 `OVERSEER_*` env vars, default state dir, proto package and
  service names, docs prose, HCL DSL keywords (none brand-coupled), and
  cross-repo references. Confirmed by `grep -r "OVERSEER_"` sweep of the tree.
- **Options evaluated** — Four options are on the table (keep as-is;
  Branded House rename; rename user-visible surface only; descriptivize
  sub-components only). Options 3 and 4 are explicitly rejected with
  rationale; Option 2 is recommended.
- **ADR-0001 authored** — `docs/adrs/ADR-0001-naming-convention.md` exists,
  covers Context, Considered options, Decision (brand: `criteria`),
  Consequences (rename surface table + merge-gate command), Migration phase
  placeholder, and three appendices (inventory, selection criteria, candidate
  shortlist with 17 entries).
- **ADR index authored** — `docs/adrs/README.md` exists and lists ADR-0001
  with title and `Proposed` status.

**Remaining blocker — Task 5 (sign-off):**  
The ADR is in `Proposed` state. Per the workstream rules, it must not be
merged as `Proposed`. The sign-off table in the ADR requires two reviewers:

1. Project lead (overseer repo) — _Pending_
2. Overlord-team representative — _Pending_

The pre-merge verification checklist in the ADR also requires the project lead
to run and record `whois`, GitHub-org, npm, Docker Hub, and USPTO TESS checks
for the candidate name `criteria` before flipping to `Accepted`.

**Exit criterion status:**
- ✅ `docs/adrs/ADR-0001-naming-convention.md` exists and clearly states the
  rename decision, what changes, and what does not.
- ✅ `docs/adrs/README.md` lists ADR-0001.
- ✅ No code changes.
- ⏳ ADR `Accepted` state — awaits the two human sign-offs and the
  pre-merge verification results documented inline in the ADR.

---

## Reviewer notes

### Review 2026-04-27 — changes-requested

#### Summary

The executor delivered a thorough, substantive ADR and index — content quality
is high and the naming surface inventory is accurate (15 env vars confirmed by
grep). However, four executor-fixable issues must be resolved before this
workstream can be considered ready for the human sign-off gate: the ADR files
are not yet committed to the branch; Appendix B has broken non-sequential
numbering; the sign-off section contradicts the Decision section; and the
executor added status notes under the reserved `## Reviewer notes` heading.
The `Accepted`-state exit criterion is a human-gated blocker that no executor
action can fully close — both sign-offs and the pre-merge verification results
must be recorded before the workstream is complete.

#### Plan Adherence

- **Task 1 — Inventory naming surface** ✅ Appendix A is thorough; 15
  `OVERSEER_*` env vars confirmed against codebase grep. HCL DSL keyword check
  (zero brand coupling) confirmed. Cross-repo refs included.
- **Task 2 — Evaluate ≥3 options** ✅ Four options evaluated; options 3 and 4
  explicitly rejected with rationale. Meets the "at least three" requirement.
- **Task 3 — Author ADR-0001** ✅ File exists at `docs/adrs/ADR-0001-naming-convention.md`,
  follows the lightweight ADR template (Status, Context, Decision,
  Consequences), includes migration-phase placeholder and candidate shortlist.
  **Blocked from merge**: file is untracked — not staged or committed to the
  branch (see Required Remediations #1).
- **Task 4 — Author `docs/adrs/README.md`** ✅ File exists and lists ADR-0001
  with status `Proposed`. **Same commit blocker as Task 3** (see #1).
- **Task 5 — Mark ADR `Accepted` after human sign-off** ⏳ Not complete;
  correctly left unchecked. Requires project lead + overlord-team sign-off and
  pre-merge verification results. Executor cannot close this unilaterally.
- **Exit criterion — ADR in `Accepted` state** ❌ ADR is in `Proposed` state.
  Human-gated; executor must prepare the branch so humans can proceed, but
  cannot flip the status autonomously.
- **Exit criterion — no code changes** ✅ Confirmed; only docs/adrs/ files and
  workstream changes present.

#### Required Remediations

- **[blocker] #1 — ADR files are untracked / uncommitted.**
  `git status` shows `docs/adrs/` as untracked files; no commit in git log
  references either file. The deliverables are invisible to reviewers until
  committed.
  _Acceptance criteria_: `git log -- docs/adrs/` shows at least one commit on
  the `01-naming-convention-review` branch containing both
  `docs/adrs/ADR-0001-naming-convention.md` and `docs/adrs/README.md`.

- **[nit] #2 — Appendix B hard-gate numbering is non-sequential.**
  Hard gates are numbered 1 and **4** (skipping 2 and 3); scored factors are
  numbered 2, 3a, 3b, 5, 6. The Decision section and Appendix C both
  cross-reference "criterion 4" for the cultural audit, which is confusing
  when it immediately follows gate 1 in the Hard gates section. The numbering
  appears to be a carry-over from a flat list that was later split into
  sections without renumbering.
  _File_: `docs/adrs/ADR-0001-naming-convention.md`, Appendix B.
  _Acceptance criteria_: Hard gates are numbered consecutively starting at 1
  (e.g., gates 1 and 2); scored factors are numbered consecutively starting
  from the next unused integer (or clearly separated and re-started at 1 with
  a note). All criterion cross-references in the Decision section and Appendix
  C are updated to match the renumbered system.

- **[nit] #3 — Sign-off section contradicts the Decision section.**
  The Sign-off section states: "The chosen top-level brand is filled into the
  Decision section at the same time [as the sign-offs]." The Decision section
  already contains the chosen brand (`criteria`). Readers attempting to follow
  the sign-off process will be confused.
  _File_: `docs/adrs/ADR-0001-naming-convention.md`, Sign-off section.
  _Acceptance criteria_: Either (a) the Decision section leaves the brand as
  a placeholder (`<TBD>`) until sign-off and the sign-off section instruction
  stays as written, or (b) the sign-off section instruction is updated to
  reflect that the brand was filled in during drafting and only the sign-off
  table itself remains to be completed. The two sections must not contradict
  each other.

- **[nit] #4 — Executor status notes placed under the reviewer-reserved
  `## Reviewer notes` heading.**
  The `## Reviewer notes` section in workstream files is reserved for the
  Workstream Reviewer to append dated review passes. The executor appended a
  progress/status summary directly under that heading (lines 136–173 of the
  current workstream file). This conflates executor status reporting with the
  review log and makes the review log harder to navigate.
  _Acceptance criteria_: The executor's status summary is moved to a separate
  `## Executor Notes` section (above `## Reviewer notes`) or removed in favour
  of a PR description entry. The `## Reviewer notes` heading is left clean for
  reviewer-only content.

#### Test Intent Assessment

Not applicable — this workstream is documentation-only. No tests are required
or present.

#### Architecture Review Required

None. All issues are within executor remediation scope.

#### Validation Performed

- `git status` — confirmed `docs/adrs/` is untracked; `workstreams/01-naming-convention-review.md` is modified.
- `git log --oneline -- docs/adrs/` — returned no commits; confirms deliverables are uncommitted.
- `grep -r "OVERSEER_" --include="*.go"` — returned exactly 15 distinct `OVERSEER_*` variables; matches Appendix A count.
- `grep -rn "OVERSEER_SHELL_LEGACY"` — appears only in `workstreams/04-shell-adapter-sandbox.md` (planned, not yet implemented); correctly absent from Appendix A.
- ADR structure checked against lightweight ADR template (Status, Context, Decision, Consequences) — ✅ present.
- Appendix B criterion cross-references in Decision section and Appendix C verified against Appendix B numbering — discrepancy confirmed (hard gates 1 and 4 in sequence).
