# WS50 — Dependency-freshness policy + update automation (supply-chain hardening)

**Phase:** Adapter v2 · **Track:** Security hardening (post-WS48) · **Owner:** Workstream executor · **Depends on:** none (config/policy only; pairs with WS49). · **Unblocks:** WS51 (the catch-up upgrades execute against this policy). · **Base branch:** `adapter-v2` (rebase onto `main` if v0.5.0 has already merged).

## Context

Two mandates (locked):

1. **Stay current.** Be on the **latest major and minor** of every dependency.
   The one caveat: pin off the latest only when a newer version has a known
   security vulnerability affecting us, or a bug we are hit by. Patch versions
   roll up freely *within* the cooldown rule below.
2. **Defend against supply-chain attacks.** Do **not** adopt any release
   **newer than 7 days** unless it fixes a known security issue or a specific bug
   we're hit by. Freshly-published (and possibly compromised) releases get a
   cooldown window before we ingest them.

The current automation contradicts both: `.github/dependabot.yml` **ignores all
`semver-major` updates** (so majors silently rot), has **no cooldown** (it would
open a PR for a patch published minutes ago), and **omits the `tools/` module**
entirely (`go.work` uses `.`, `sdk`, `tools`, `workflow`). This WS rewrites the
policy and the automation that enforces it. It does **not** perform the actual
version bumps — that backlog is WS51.

**Do not rely on Dependabot alone.** It is slow/clunky and handles the *large*
changes that majors require poorly — in Go, a major bump is a **module-path
change** (`.../foo` → `.../foo/v2`) plus call-site edits, which Dependabot (and a
plain `go get -u`) do not perform. So Dependabot is demoted to what it is good at
(routine, low-risk minor/patch PRs with a cooldown), and the freshness picture +
major upgrades are driven by **Go tooling** (`go list -m -u all`,
[`go-mod-outdated`](https://github.com/psampaz/go-mod-outdated) for a filterable
report, and [`gomajor`](https://github.com/icholy/gomajor) for the `/vN`
module-path rewrites). The tooling is the primary mechanism; Dependabot is a
convenience layer on top.

Config/meta only — **no product code** — so it is safe to land during manual
testing of the v0.5.0 candidate.

## Prerequisites

None. Pairs naturally with WS49 (the scanner) but does not depend on it.

## In scope

### Step 1 — Write the policy down

Add `docs/dependency-policy.md` capturing the rules so humans and the update bot
agree:

- **Target:** latest **major.minor** for all ecosystems (Go modules ×4, GitHub
  Actions). Patch rolls up under the cooldown.
- **Cooldown:** never ingest a release **< 7 days old** unless it carries a
  security fix or fixes a bug we're hit by (those bypass the wait).
- **Exception path:** to hold a dependency below latest, add an `ignore`/constraint
  entry that cites the advisory or bug and a review date — mirrors the WS49
  `osv-scanner.toml` "documented + dated" convention.
- **Security updates bypass cooldown** (Dependabot/Renovate security PRs are not
  delayed): availability of a fix outranks the supply-chain wait.

### Step 2 — Go-tooling freshness report (primary mechanism)

Pin the tools in `tools/go.mod` (no floating `@latest`) and add Make targets,
covering all four modules (`.`, `sdk`, `tools`, `workflow`):

- **`make deps-outdated`** — `go list -u -m -json all` piped through
  [`go-mod-outdated`](https://github.com/psampaz/go-mod-outdated)
  (`-update -direct`) to print a filterable table of out-of-date **direct** deps.
  This is the source of truth for "are we on latest major.minor", not Dependabot.
- **`make deps-majors`** — [`gomajor`](https://github.com/icholy/gomajor) `list`
  to surface available **major** upgrades (the module-path `/vN` bumps Dependabot
  can't drive), which WS51 then applies with `gomajor get`.
- Add a **non-blocking** CI job (`deps-report`) that runs `make deps-outdated` and
  posts/job-summaries the result, so drift is visible every PR without flaking the
  build. Enforcement of "latest" stays with review + WS51, not a hard gate
  (upstream release cadence would make a hard gate flap).

### Step 3 — Demote Dependabot to routine minor/patch

Keep `.github/dependabot.yml` only for the low-risk lane:

- **Remove** the blanket `ignore: version-update:semver-major` — but note majors
  are now driven by `gomajor` (Step 2 / WS51), not expected to land cleanly via
  Dependabot; majors it does raise are signals, not merge-ready PRs.
- **Add the missing `tools/` module** (`directory: /tools`, `gomod`).
- **Add a 7-day cooldown** (`cooldown` with `default-days: 7`, and the per-type
  `semver-*-days` if finer control is wanted). Security updates are exempt by
  Dependabot's design.
- Group minor+patch to keep PR volume sane.
- Apply the same shape to the `github-actions` ecosystem (drop major-ignore, add
  cooldown).

(If a single richer tool is preferred over the Dependabot-plus-tooling split,
**Renovate** with `minimumReleaseAge: "7 days"`, `internalChecksFilter: "strict"`
and `packageRules` targeting latest major.minor is the documented alternative.
Pick one update-bot — do not run Dependabot and Renovate together. The `go list`
/ `gomajor` targets remain regardless of which bot is chosen.)

## Out of scope

- Performing the upgrades (WS51).
- The vulnerability gate itself (WS49).
- Pinning/cooldown for the separate adapter/SDK repos (each owns its own policy;
  this WS is the monorepo).

## Behavior change

**Yes (automation only).** Dependabot will start proposing major upgrades and the
`tools/` module, and will hold new releases for 7 days (security fixes exempt). No
product/runtime behavior changes; no dependency is bumped by this WS.

## Tests required

- `make deps-outdated` and `make deps-majors` run locally across all four modules
  and print the current drift (capture output — it is the WS51 backlog).
- The `deps-report` CI job runs (non-blocking) on a `workflow_dispatch`.
- `dependabot.yml` validates (GitHub schema / "Check for updates" run); confirm
  all four modules + github-actions are covered, no `semver-major` ignore remains,
  and the 7-day cooldown is set.
- `docs/dependency-policy.md` review.

## Exit criteria

- `docs/dependency-policy.md` states the latest-major.minor + 7-day-cooldown +
  security-bypass policy, and that majors are driven by `gomajor`, not Dependabot.
- `make deps-outdated` (go list + go-mod-outdated) and `make deps-majors`
  (gomajor) exist, are pinned in `tools/go.mod`, and surface the backlog; a
  non-blocking `deps-report` CI job runs them.
- `.github/dependabot.yml` covers all four Go modules + GitHub Actions, no longer
  ignores majors, and enforces the 7-day cooldown.

## Files this workstream may modify

- `.github/dependabot.yml`
- `.github/workflows/ci.yml` (**only** the non-blocking `deps-report` job; WS49
  owns `osv-scan`)
- `docs/dependency-policy.md` *(new)*
- `Makefile` (`deps-outdated`, `deps-majors` targets)
- `tools/go.mod` / `tools/go.sum` (pin `go-mod-outdated`, `gomajor` as tool deps)
- `renovate.json` *(only if Renovate is chosen over Dependabot)*

## Files this workstream may NOT edit

- Application `go.mod` / `go.sum` in `.`, `sdk`, `workflow` (the bumps are WS51;
  `tools/` is edited here only to pin the tooling).
- The `osv-scan` job in `.github/workflows/ci.yml` (WS49).
- Product/runtime source.
- `README.md`, `PLAN.md`, `AGENTS.md`, `CHANGELOG.md`, `workstreams/README.md`,
  any other workstream file.
