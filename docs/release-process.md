# Release process

Criteria's v2 release is guarded by four verification gates (README D57). Two of
them are automated in [`.github/workflows/release-gates.yml`](../.github/workflows/release-gates.yml)
(WS38):

| Gate | What it checks | Where |
|------|----------------|-------|
| Gate 3 (D57.3) | Remote transport end-to-end demo | reuses [`remote-e2e.yml`](../.github/workflows/remote-e2e.yml) (the WS22 smoke) |
| Gate 4 (D57.4) | Publishing flow: starter clones build → sign → publish → pull → conformance | `release-gates.yml` job `gate-4-publish-flow` |

`release-gates.yml` runs on every `v*` tag push (and via `workflow_dispatch`).
The `gates-passed` job aggregates the results into a single status:

- **Gate 3 must succeed.**
- **Gate 4 must not fail.** It is *skipped* (and allowed) until the CI publishing
  org is provisioned — see below.

## Enforcing the gates on a release

Wire `Release gates passed` as a **required status check** on tag/release
protection so a failed gate blocks publishing the release. The
[`release.yml`](../.github/workflows/release.yml) build runs in parallel; the
required check is what prevents promoting a tag whose gates are red.

## Gate 4 setup (publishing flow)

Gate 4 needs a CI-owned GHCR org and three starter-template clones. Until these
exist the job is skipped so it never blocks prematurely.

1. **Create the org and clone repos.** Create a GHCR-owning org (default name
   `criteria-ci`) and three repositories cloned from the WS27 starter templates,
   each with its `publish.yml` intact:
   - `criteria-ci/adapter-test-typescript` (from `criteria-adapter-starter-typescript`)
   - `criteria-ci/adapter-test-python` (from `criteria-adapter-starter-python`)
   - `criteria-ci/adapter-test-go` (from `criteria-adapter-starter-go`)
2. **Add credentials.** In the criteria repo settings add:
   - Variable `CRITERIA_CI_ENABLED = true`
   - Variable `CRITERIA_CI_ORG = criteria-ci` (optional; defaults to `criteria-ci`)
   - Secret `CRITERIA_CI_TOKEN` — a PAT with `repo` + `workflow` scope on the
     clone repos and `read:packages` on the org.
3. **What the gate does per SDK** (the four WS38 steps):
   1. Bumps a unique test tag on the clone (`v0.0.0-gate.<run-id>`).
   2. Pushes it and waits for the clone's `publish.yml` to conclude `success`.
   3. Pulls the resulting artifact via `criteria adapter pull` (verifies the
      cosign signature + manifest) and runs `criteria adapter info` (runtime
      `Info()` vs. manifest check, D15).
   4. Runs the WS26 conformance suite.
   5. Deletes the test tag.

## Remaining gates

Gates 1 and 2 (and the final tag/release orchestration) are covered by other
release workstreams (WS40). A fuller narrative of the whole release runbook is
the subject of the WS39 documentation refresh.
