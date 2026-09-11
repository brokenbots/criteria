# Dependency-freshness & supply-chain policy

This repo follows two locked mandates for third-party dependencies. They apply to
every ecosystem we vendor: the four Go modules (`.`, `sdk`, `tools`, `workflow`)
and the GitHub Actions used in CI.

## 1. Stay current — latest major.minor

Be on the **latest major and minor** of every dependency. Patch versions roll up
freely *within* the cooldown rule below.

The only reason to pin **below** latest is a concrete one:

- a newer version has a **known security vulnerability** that affects us, or
- a newer version carries a **bug we are actually hit by**.

Any such pin is a documented, dated exception — see
[Holding a dependency below latest](#holding-a-dependency-below-latest).

## 2. Defend against supply-chain attacks — 7-day cooldown

Do **not** adopt any release **newer than 7 days** unless it fixes a known
security issue or a specific bug we're hit by. A freshly-published (and possibly
compromised) release gets a cooldown window before we ingest it.

**Security updates bypass the cooldown.** Availability of a fix outranks the
supply-chain wait, so security-update PRs (Dependabot's security lane) are not
delayed.

## How "latest" is determined — Go tooling, not Dependabot

Dependabot is **not** the source of truth for freshness. It is slow, and it
cannot drive Go **major** upgrades: in Go a major bump is a *module-path change*
(`.../foo` → `.../foo/v2`) plus call-site edits, which neither Dependabot nor a
plain `go get -u` performs. Dependabot is demoted to the routine minor/patch lane
(see below); the freshness picture and major upgrades are driven by Go tooling,
pinned in `tools/go.mod` (no floating `@latest`):

| Command | Tool | Answers |
| --- | --- | --- |
| `make deps-outdated` | [`go-mod-outdated`](https://github.com/psampaz/go-mod-outdated) | Which **direct** deps are behind their latest minor/patch (workspace-wide). |
| `make deps-majors` | [`gomajor`](https://github.com/icholy/gomajor) | Which **major** (`/vN`) upgrades are available, per module. |
| `make vuln-scan` | [`osv-scanner`](https://github.com/google/osv-scanner) | Which deps carry a known advisory. |
| `make vulncheck` | [`govulncheck`](https://pkg.go.dev/golang.org/x/vuln/cmd/govulncheck) | Which **reachable** Go vulnerabilities affect compiled code paths, per module. |

A non-blocking `deps-report` CI job runs `make deps-outdated` on every PR and
posts the result to the job summary, so drift is visible without flaking the
build. Enforcement of "latest" stays with review, not a hard gate — upstream
release cadence would make a hard gate flap.

## Vulnerability scanning

Two scanners run in CI and have local Make targets:

- `make vuln-scan` runs `osv-scanner` across the whole workspace (`go.work` plus
  all modules). It reports any dependency with a known advisory, even if the
  vulnerable symbol is not reachable from our code.
- `make vulncheck` runs `govulncheck` separately on each module (root, `sdk/`,
  `tools/`, `workflow/`). It only reports vulnerabilities whose affected symbols
  are reachable from the module's compiled code paths, which dramatically reduces
  false positives compared to advisory-only scanning.

### Running locally

```bash
make vulncheck
```

The command scans the four workspace modules in sequence and prints one summary
per module. Clean output looks like:

```
No vulnerabilities found.
```

If a reachable vulnerability is found, `govulncheck` prints the advisory id
(e.g. `GO-2026-1234`), the affected package, the call stack that reaches it, and
exits non-zero. Because the Make target joins the four module scans with `&&`,
any finding aborts the whole run so the failing module is visible immediately.

### What failure means

A failing `govulncheck` result is a blocking CI failure. Resolve it by one of:

1. **Upgrade the dependency** to a version that fixes the vulnerability
   (`go get <module>@<version>` in the affected module, then `go mod tidy` and
   `go work sync`).
2. **Document a suppression** if the finding is a false positive for our code
   (unreachable in practice, only linked by init-side-effect, or already covered
   by an explicit `osv-scanner.toml` ignore with a review date). Suppressions
   must include the advisory id, reason, and review date so they are re-checked.

Both `make vuln-scan` and `make vulncheck` are required checks in CI.

Applying the upgrades:

- **Patch/minor:** `go get <module>@<version>` (honor the 7-day cooldown).
- **Major:** `gomajor get <module>@latest`, which rewrites the `/vN` module path
  and import sites; absorb any remaining breaking API changes in source.

## The update bot — Dependabot (routine minor/patch lane)

`.github/dependabot.yml` is configured to:

- cover **all four Go modules** (`/`, `/sdk`, `/tools`, `/workflow`) plus the
  `github-actions` ecosystem;
- **not** ignore `semver-major` updates (majors it raises are *signals*, not
  merge-ready PRs — drive them with `gomajor`);
- apply a **7-day cooldown** (`cooldown: default-days: 7`); security updates are
  exempt by Dependabot's design;
- group minor + patch updates to keep PR volume sane.

> Pick one update bot. If a single richer tool is ever preferred, **Renovate**
> with `minimumReleaseAge: "7 days"`, `internalChecksFilter: "strict"` and
> `packageRules` targeting latest major.minor is the documented alternative — do
> **not** run Dependabot and Renovate together. The `go-mod-outdated` / `gomajor`
> targets remain regardless of which bot is chosen.

## Holding a dependency below latest

To pin a dependency below its latest version, record it as a dated exception so
the decision is auditable and re-reviewed — mirroring the `osv-scanner.toml`
"documented + dated" convention. Add an entry to the table below **and** the
matching `ignore` constraint in `.github/dependabot.yml`, citing the advisory or
bug id and a review date.

| Dependency | Held at | Reason (advisory / bug) | Review by |
| --- | --- | --- | --- |
| _none_ | | | |

On the review date the exception must be cleared or re-justified.
