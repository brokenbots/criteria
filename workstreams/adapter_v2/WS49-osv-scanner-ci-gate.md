# WS49 — osv-scanner vulnerability gate in CI

**Phase:** Adapter v2 · **Track:** Security hardening (post-WS48) · **Owner:** Workstream executor · **Depends on:** none (CI/meta only). · **Unblocks:** WS51 (the catch-up upgrades clear findings, then this gate flips to blocking). · **Base branch:** `adapter-v2` (rebase onto `main` if v0.5.0 has already merged).

## Context

Mandate (locked): **no more shipping code with known security vulnerabilities.**
The repo has no vulnerability scanning today (`grep -rn osv .github/` → nothing),
so a dependency with a published advisory can land silently. We add
[osv-scanner](https://github.com/google/osv-scanner) (Google's OSV database
client) as a CI check across all four Go modules and the GitHub Actions, and make
it a **required gate**.

Sequencing matters: the dependency tree is currently behind (see WS50/WS51 — "no
one was paying attention"), so a blocking gate added *before* the catch-up
upgrades would turn CI red immediately. This WS therefore lands the scanner in
**report-only** mode if the first run is not clean, with an explicit step to flip
it to blocking once WS51 clears the backlog (or immediately, if the first run is
already clean). The flip is the exit criterion shared with WS51.

This is CI/meta only — **no product code changes** — so it is safe to land while
the v0.5.0 candidate is under manual testing.

## Prerequisites

None. Independent of WS46–48.

## In scope

### Step 1 — Scanner job

Add an `osv-scan` job to `.github/workflows/ci.yml` (mirror the existing job
shape: `actions/checkout@v4`, `actions/setup-go@v5` with `go-version-file: go.mod`).
Run osv-scanner over the workspace so all four modules
(`.`, `sdk`, `tools`, `workflow`) and their `go.sum` lockfiles are covered, plus
the GitHub Actions workflows. Prefer the pinned official action
(`google/osv-scanner-action`, pinned by SHA) or `go run github.com/google/osv-scanner/...`
pinned in `tools/go.mod`; do not float `@latest`.

### Step 2 — Config + documented allowlist

Add an `osv-scanner.toml` at the repo root. Use it only for **documented,
time-boxed** exceptions — each `[[IgnoredVulns]]` entry MUST carry an `id`, a
`reason`, and a future `ignoreUntil` date (a review expiry), so an unfixable or
false-positive finding is an explicit, auditable decision rather than a silent
skip. The default posture is "no ignores."

### Step 3 — Wire into the required gate

- If the initial scan is **clean**: make `osv-scan` fail the build on any finding
  and add it to the `all-checks` job's `needs:` list
  (`needs: [lint, unit-tests, e2e, proto-drift, osv-scan]`).
- If the initial scan is **not clean**: land the job with
  `continue-on-error: true` (report-only) and **do not** add it to `all-checks`
  yet; record the open findings in this file. WS51 clears them and performs the
  flip (remove `continue-on-error`, add to `all-checks`). The branch-protection
  required-checks list must be updated to include "All checks passed" coverage of
  the new job — note this as an owner action if branch protection is managed
  outside the repo.

### Step 4 — Local parity

Add a `make vuln-scan` target that runs the same scan locally (same pinned
version) so contributors can reproduce CI before pushing. Document it in
`CONTRIBUTING.md` (defer the doc edit to the cleanup gate if out of scope here).

## Out of scope

- Upgrading dependencies to clear findings (WS51).
- Dependency-freshness policy + Dependabot cooldown (WS50).
- Secret scanning, SAST, container image scanning (future hardening).

## Behavior change

**Yes (CI only).** A new `osv-scan` CI job runs on every PR/push. Once flipped to
blocking (Step 3 / WS51), a PR introducing a dependency with a known OSV advisory
fails CI until upgraded or explicitly time-boxed in `osv-scanner.toml`. No
product/runtime behavior changes.

## Tests required

- A `workflow_dispatch` run of `ci.yml` on the branch showing the `osv-scan` job
  executes across all four modules (capture the run URL).
- `make vuln-scan` runs locally and reproduces the CI result.
- If landing blocking: the run is green. If report-only: the open findings are
  enumerated in this file with the owning upgrade (cross-ref WS51).

## Exit criteria

- osv-scanner runs in CI over all four Go modules + GitHub Actions.
- `osv-scanner.toml` exists; any ignore is justified + dated.
- `make vuln-scan` gives local parity.
- The job is **blocking** and in `all-checks` — done here if the tree is already
  clean, otherwise completed by WS51 after the catch-up upgrades.

## Open findings (report-only landing — handed to WS51)

The first scan was **not clean**, so per Step 3 the `osv-scan` job landed
report-only (`continue-on-error: true`, not in `all-checks`). osv-scanner v2.3.8
reports **26 known vulnerabilities** across the workspace go.mods (run
`make vuln-scan` to reproduce). WS51 clears these and flips the gate to blocking:

| Package | Current | Fixed in | Advisories |
| --- | --- | --- | --- |
| `github.com/in-toto/in-toto-golang` | 0.9.0 | 0.11.0 | GHSA-pmwq-pjrm-6p5r |
| `github.com/sigstore/cosign/v2` | 2.6.3 | 3.0.5 *(major: `/v2`→`/v3`)* | GO-2026-4529 |
| `github.com/sigstore/rekor` | 1.4.3 | 1.5.0 | GHSA-273p-m2cw-6833, GHSA-4c4x-jm2x-pf9j, GO-2026-4354, GO-2026-4355 |
| `github.com/sigstore/sigstore` | 1.10.3 | 1.10.4 | GHSA-fcv2-xgw5-pqxf, GO-2026-4358 |
| `github.com/sigstore/timestamp-authority/v2` | 2.0.3 | 2.0.6 | GHSA-xm5m-wgh2-rrg3 |
| `github.com/theupdateframework/go-tuf/v2` | 2.3.0 | 2.4.1 | GHSA-846p-jg2w-w324, GHSA-fphv-w9fq-2525, GHSA-jqc5-w2xx-5vq4, GO-2026-4348, GO-2026-4349, GO-2026-4377 |
| `golang.org/x/crypto` | 0.51.0 | 0.52.0 | GO-2026-5005, -5006, -5013..-5021, -5023, -5033 (13) |
| `golang.org/x/net` | 0.54.0 | 0.55.0 | GO-2026-5025..-5030 (6) |
| `stdlib` | 1.26.3 | 1.26.4 | GO-2026-5037, GO-2026-5038, GO-2026-5039 |

Most originate from the WS46–48 signing dependency tree (sigstore/in-toto/tuf)
plus a Go toolchain bump (`stdlib` 1.26.3→1.26.4). No `osv-scanner.toml` ignores
were added — every finding is fixable by upgrade in WS51.

> **GitHub Actions note:** osv-scanner v2.3.8 does not bundle a workflow
> extractor, so action advisories are covered by the Dependabot `github-actions`
> ecosystem (WS50) rather than this job.

## Files this workstream may modify

- `.github/workflows/ci.yml` (new `osv-scan` job; `all-checks` `needs`)
- `osv-scanner.toml` *(new)*
- `Makefile` (`vuln-scan` target)
- `CONTRIBUTING.md` *(if in scope; else defer to cleanup gate)*

## Files this workstream may NOT edit

- Any `go.mod` / `go.sum` (dependency changes are WS50/WS51).
- `.github/dependabot.yml` (WS50).
- Product/runtime source under `internal/`, `cmd/`, `workflow/`.
- `README.md`, `PLAN.md`, `AGENTS.md`, `CHANGELOG.md`, `workstreams/README.md`,
  any other workstream file.
