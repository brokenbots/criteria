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
