# Workstream 8 — Phase 0 cleanup gate

**Owner:** Cleanup agent (or human committer) · **Depends on:** [W01](01-naming-convention-review.md)–[W07](07-repo-hygiene.md) · **Unblocks:** Phase 1 planning + first non-RC tag.

## Context

Phase 0 closes here. This workstream is the only one in the phase
that may edit the coordination set (`README.md`, `PLAN.md`,
`AGENTS.md`, `workstreams/README.md`). It runs after every other
Phase 0 workstream is merged, performs final validation, archives
the phase, and cuts `v0.1.0`.

Mirrors the close-out shape of v1.5/W10 in the overlord repo: build
+ lint + test green, smoke runs pass, then archive.

## Prerequisites

- Every Phase 0 workstream ([W01](01-naming-convention-review.md)–[W07](07-repo-hygiene.md))
  merged on `main`.
- All exit criteria from each workstream verified.
- `git status` clean on `main`.

## In scope

### Build / lint / test

- [ ] `make proto` clean; `git diff --exit-code sdk/pb/` confirms
      generated bindings match the source.
- [ ] `make proto-lint` exits 0.
- [ ] `make proto-check-drift` exits 0.
- [ ] `make build` produces `bin/overseer`.
- [ ] `make plugins` produces all `bin/overseer-adapter-*` binaries.
- [ ] `make test` (with `-race`) green across root, `sdk/`, and
      `workflow/` modules.
- [ ] `make test-conformance` green.
- [ ] `make lint-imports` green.
- [ ] `make validate` green for every example HCL.
- [ ] `make example-plugin` ([W06](06-third-party-plugin-example.md))
      green if the target was added.
- [ ] CLI smoke: `./bin/overseer apply examples/hello.hcl --events-file /tmp/events.ndjson`
      exits 0.

### Hygiene checks

- [ ] `git ls-files | grep -E '\.db(-(shm|wal))?$'` is empty.
- [ ] `grep -rn 'overlord' --include='*.go' --include='*.proto' --include='*.md' --include='*.hcl' --include='Makefile'`
      returns only intentional cross-references (e.g. links to the
      overlord repo on GitHub). Anything that looks like residual
      stale text gets fixed here.
- [ ] `grep -rn 'OVERLORD_' --include='*.go'` returns empty.
- [ ] No orphan files in `internal/cli/testdata/compile/`.

### Documentation updates (the "files NOT to modify" set)

This workstream is the only one that may edit:

- [ ] `README.md` — confirm it reflects the post–Phase 0 state. If
      [W02](02-readme-and-contributor-docs.md) already landed the
      rewrite, this is a final pass. If a rename from
      [W01](01-naming-convention-review.md) happened, sweep any
      remaining strings.
- [ ] `PLAN.md` — tick every Phase 0 workstream checkbox; update
      "Status snapshot" to "Phase 0 closed YYYY-MM-DD"; add a
      "Phase 1 — TBD" pointer, or list the next phase if it's
      already planned. Add an archive footer line:
      `*Phase 0 closed YYYY-MM-DD. Archived under [workstreams/archived/v0/](workstreams/archived/v0/).*`
- [ ] `AGENTS.md` — sweep any references that became stale during
      Phase 0 (e.g. if [W03](03-public-plugin-sdk.md) moved the
      plugin SDK location, fix the high-value-files pointers).
- [ ] `workstreams/README.md` — mark Phase 0 archived; list
      "Phase 1 — TBD" or the next planning artifact.

### Archive

- [ ] `mkdir -p workstreams/archived/v0/`
- [ ] `git mv workstreams/0[1-8]-*.md workstreams/archived/v0/`
- [ ] Update intra-workstream links if any reviewer notes referenced
      sibling files; otherwise leave the moved files unchanged.

### Tagging

- [ ] After all checks above pass and the docs/archive are committed:
      `git tag -a v0.1.0 -m "Phase 0 cleanup gate"`.
- [ ] Push the tag.
- [ ] If [W07](07-repo-hygiene.md) introduced a release-asset
      workflow (Docker image, goreleaser binaries, etc.), confirm
      the v0.1.0 tag triggers it and the assets land. If no release
      automation exists yet, that's fine — the source tag is
      enough for `go install` consumers.

### Sibling-agent tuning (per cleanup-agent guidance)

The cleanup agent may apply **at most two directive
additions/removals each** to
[.github/agents/workstream-executor.agent.md](../.github/agents/workstream-executor.agent.md)
and
[.github/agents/workstream-reviewer.agent.md](../.github/agents/workstream-reviewer.agent.md),
strictly limited to drift observed during Phase 0.

If no drift, leave the agent files alone.

### Optional: post-review

- [ ] (Optional) Author `arch_reviews/v0-postreview.md` capturing
      what shipped, what surprised the team during the standalone
      bring-up, what carries into Phase 1.

## Out of scope

- Planning Phase 1. The "Phase 1 — TBD" marker is enough; planning
  is a separate exercise.
- Any new feature work.
- Any structural refactor not already in flight from W01–W07.
- Renaming the repo or org. If [W01](01-naming-convention-review.md)
  recommended a rename, that rename is its own phase.

## Files this workstream may modify

This is the **only** Phase 0 workstream that may edit:

- `README.md`
- `PLAN.md`
- `AGENTS.md`
- `workstreams/README.md`
- `workstreams/01-*.md` … `workstreams/08-*.md` (only to move them
  into `archived/v0/`).

It also creates:

- `workstreams/archived/v0/` (new directory).
- `arch_reviews/v0-postreview.md` (optional).

## Tasks

- [ ] Run every Build / lint / test check.
- [ ] Run every Hygiene check.
- [ ] Update the four docs in the coordination set.
- [ ] Move workstream files to `workstreams/archived/v0/`.
- [ ] Final commit lands all of the above plus a one-paragraph
      summary in reviewer notes. Do not commit if any required
      validation fails.
- [ ] Tag `v0.1.0` and push.
- [ ] (If justified) Apply minimal sibling-agent directive tuning.
- [ ] (Optional) Author `arch_reviews/v0-postreview.md`.

## Exit criteria

- All checkboxes above ticked on `main`.
- `workstreams/` contains only `README.md`, `archived/`, and
  optionally a placeholder for Phase 1 planning.
- `README.md`, `PLAN.md`, `AGENTS.md`, `workstreams/README.md` all
  reflect the post–Phase 0 state.
- `v0.1.0` tag exists on `main` and is pushed.
- `make` validation lanes are all green at the tag.

## Tests

This workstream does not add new tests. The validation lanes from
W01–W07 plus the existing CI suite are the signal.

## Risks

| Risk | Mitigation |
|---|---|
| One of W01–W07 is "merged" but didn't actually achieve its exit criteria | This workstream re-runs every gating command. If any fails, do not commit; open a remediation PR against the offending workstream's deliverables. |
| `v0.1.0` tag is cut prematurely, then a critical bug shows up | Acceptable — cut `v0.1.1` from the fix. Pre-1.0 tags are not stability promises. |
| Sibling-agent tuning over-corrects on a single observation | Cap at two directive add/removes per agent. If more drift is observed, capture it as a Phase 1 planning input, not as agent-config change in this PR. |
| `workstreams/archived/v0/` move loses cross-references | The intra-workstream links use relative paths; after the move, links between archived files still resolve (they all moved together). Cross-links from active files to archived files use `archived/v0/NN-…md` form; check those after the move. |
| Coordination-file updates drift from what W01–W07 actually shipped | Re-read each workstream's reviewer notes before authoring; cross-check claims against the post-Phase-0 repo state. |
