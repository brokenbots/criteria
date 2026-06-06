# WS51 — Catch-up dependency upgrades (reach latest major.minor, clear vulns)

**Phase:** Adapter v2 · **Track:** Security hardening (post-WS48) · **Owner:** Workstream executor · **Depends on:** WS49 (scanner to verify "clean"), WS50 (policy to upgrade against). · **Unblocks:** flips the WS49 osv gate to blocking. · **Base branch:** `adapter-v2` (rebase onto `main` if v0.5.0 has already merged).

## Context

"No one was paying attention," so the dependency tree has drifted — including
**outstanding major-version bumps** (Dependabot was configured to ignore them, see
WS50) and likely some dependencies carrying known advisories (see WS49). This WS
is the execution backlog: bring every module to the **latest major.minor** per the
WS50 policy, clear all osv-scanner findings, and then flip the WS49 gate to
blocking.

This **does** change `go.mod`/`go.sum` and may require source edits to absorb
breaking changes, so unlike WS49/WS50 it is **not** safe to land under the frozen
v0.5.0 candidate. **Sequencing:** scope it now (this file), execute it in parallel
on its own branch, and merge **after** the v0.5.0 candidate clears manual testing
(or onto `main` post-merge) so the RC under test isn't disturbed.

## Prerequisites

WS49 (osv-scanner available to confirm clean) and WS50 (policy + Dependabot
rewrite) merged. Full green CI baseline before starting, to attribute breakage.

## In scope

### Step 1 — Inventory (tooling, not Dependabot)

Use the WS50 Go tooling — **do not** wait on Dependabot PRs (slow, and it can't
drive Go major/module-path bumps) — across all four modules (`.`, `sdk`, `tools`,
`workflow`) and GitHub Actions:

- **`make deps-outdated`** (`go list -u -m -json all` + `go-mod-outdated -direct`)
  → direct deps behind latest minor/patch.
- **`make deps-majors`** (`gomajor list`) → available **major** (`/vN`) upgrades.
- osv-scanner output (WS49) → deps with advisories. These are **priority** and
  bypass the WS50 7-day cooldown.
- Note any dep that must stay pinned below latest, with the advisory/bug reason
  (feeds the WS50 exception list).

### Step 2 — Upgrade, module by module

Work one module at a time to keep blast radius small; after each: `go mod tidy`,
`go build ./...`, `go test ./... -race`, `go work sync`, and the full gate
(`make lint vuln-scan validate`).

- **Patch/minor:** `go get` the target; honor the WS50 7-day cooldown (don't adopt
  a release < 7 days old unless it fixes a security issue or a bug we're hit by).
- **Majors:** drive with **`gomajor get <module>@latest`**, which rewrites the
  module path (`/vN`) and import sites — the large change Dependabot/`go get -u`
  won't do. One PR per major where feasible (reviewability); absorb remaining
  breaking API changes in source. If a major is infeasible now, record the reason
  + revisit date in `docs/dependency-policy.md`'s exception list rather than
  silently ignoring it.
- Keep the Go toolchain (`go 1.26.3` in each `go.mod` + `go.work`) consistent
  across modules.

### Step 3 — Clear vulnerabilities

Drive osv-scanner to **zero** unignored findings. Any residual must be a
documented, dated `osv-scanner.toml` entry (WS49 convention) with a tracking note
— not an open hole.

### Step 4 — Flip the gate to blocking

Once the scan is clean, complete the WS49 flip: remove `continue-on-error` from
the `osv-scan` job and add it to `all-checks` `needs:` (if WS49 landed it
report-only). Note the branch-protection required-checks update as an owner
action if managed outside the repo.

## Out of scope

- Adding the scanner / writing the policy (WS49 / WS50).
- Dependency changes in the separate adapter/SDK repos (each owns its own).
- Feature work riding along with the bumps — upgrades only; behavior-neutral.

## Behavior change

**Dependencies only, behavior-neutral intent.** Versions move to latest
major.minor; any *observable* change forced by a breaking upstream API is
enumerated per-PR for the reviewer. Product behavior should be unchanged; the test
suite + e2e are the guardrail.

## Tests required

- Full suite green per module after each upgrade: `go test ./... -race`
  (root + `sdk` + `workflow`), `make test-conformance`, `make build plugins`,
  `make validate`, `make example-plugin`.
- `make lint`, `make spec-check`, import boundaries, lint baseline within cap.
- `make vuln-scan` / CI `osv-scan` reports **zero** unignored findings.
- For each major bump: a short note of the breaking change absorbed + the
  behavior-equivalence argument.

## Exit criteria

- All four Go modules + GitHub Actions on latest major.minor (or a documented,
  dated exception in `docs/dependency-policy.md`).
- osv-scanner clean; the WS49 gate is **blocking** and in `all-checks`.
- Full CI green on the branch.

## Files this workstream may modify

- `go.mod` / `go.sum` in `.`, `sdk`, `tools`, `workflow`; `go.work`
- Source under `internal/`, `cmd/`, `workflow/`, `sdk/` **only** as required to
  absorb breaking upstream changes (no feature work)
- `.github/workflows/*.yml` action version pins
- `.github/workflows/ci.yml` + `osv-scanner.toml` **only** for the WS49 gate flip
- `docs/dependency-policy.md` exception list

## Files this workstream may NOT edit

- `.github/dependabot.yml` (WS50).
- The WS49 scanner job *shape* (only the report-only → blocking flip).
- `README.md`, `PLAN.md`, `AGENTS.md`, `CHANGELOG.md`, `workstreams/README.md`,
  any other workstream file.
