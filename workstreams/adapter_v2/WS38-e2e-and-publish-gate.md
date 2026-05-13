# WS38 — End-to-end remote demo + publishing-flow gate

**Phase:** Adapter v2 · **Track:** Release gate · **Owner:** Workstream executor · **Depends on:** [WS22](WS22-remote-demo-runbook.md), [WS27](WS27-starter-repos.md), [WS28](WS28-reusable-publish-action.md), [WS37](WS37-v1-protocol-code-removal.md). · **Unblocks:** [WS40](WS40-v2-release-gate.md).

## Context

`README.md` D57.3 + D57.4. Two of the four release-gate verifications:

- **Gate 3** — End-to-end remote transport demo (the smoke test from WS22, gated as `CRITERIA_REMOTE_E2E=1` and run on each release tag).
- **Gate 4** — Publishing flow: the three starter-template repos (WS27) build, sign, and publish to a CI-owned GHCR org on every PR merge.

## Prerequisites

WS22, WS27, WS28, WS37.

## In scope

### Step 1 — Gate 3 wiring

Confirm the WS22 smoke test runs on each release tag in criteria's CI. Add a release-gate check that blocks tag publication if the smoke fails.

### Step 2 — Gate 4 wiring

Create a CI-owned GHCR org (`criteria-ci`) with three pre-created template-clone repos: `criteria-ci/adapter-test-typescript`, `-python`, `-go`. Each is a fresh clone of the corresponding starter from WS27 with a sample tag.

The PR-merge gate in criteria's CI:

1. Bumps the tag on each clone.
2. Pushes; the clone's publish workflow runs.
3. Pulls the resulting artifact via `criteria adapter pull`.
4. Runs the WS26 conformance suite against the pulled binary.

All four steps must succeed for the PR to merge.

### Step 3 — Documentation

Document the two gates in `docs/release-process.md` (defer the actual doc edit to WS39 if needed; this WS just provides the runnable gates).

## Out of scope

- Documentation refresh — WS39.
- Final tag/release — WS40.

## Behavior change

**N/A** — new CI gates.

## Tests required

- Gates pass on the WS40 release-tag candidate.

## Exit criteria

- Both gates exist in CI and have passed at least once.

## Files this workstream may modify

- `.github/workflows/release-gates.yml` *(new)* in criteria.
- The criteria-ci GHCR org and its three clone repos.

## Files this workstream may NOT edit

- The criteria source code (this is CI configuration).
- Other workstream files.
