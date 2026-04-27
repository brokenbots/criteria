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

- [ ] Inventory the user-visible naming surface.
- [ ] Evaluate at least three options against the criteria above.
- [ ] Author `docs/adrs/ADR-0001-naming-convention.md`.
- [ ] Author `docs/adrs/README.md` as a one-line ADR index.
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
