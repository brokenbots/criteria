# Workstream 7 — Repo hygiene

**Owner:** Repo maintainer agent · **Depends on:** [W01](01-naming-convention-review.md) · **Unblocks:** [W08](08-phase0-cleanup-gate.md).

## Context

The repo was created by `git filter-repo` extraction. It carries no
LICENSE file, no SECURITY.md, no CODEOWNERS, no PR or issue templates,
no dependabot config (despite a recent dependabot PR landing —
suggesting the auto-config inferred from `go.mod`, but it isn't
explicit).

The README links to a `LICENSE` file that doesn't exist (line 75:
`See [LICENSE](LICENSE).`). That's a broken link today; before any
public release it must be a real file.

[W01](01-naming-convention-review.md)'s ADR-0001 may rename the
project — most of the templates in this workstream are name-aware
(SECURITY.md mentions "overseer"; CODEOWNERS uses an org/team name).
Sequence W07 after W01 so the templates are written with whatever
ADR-0001 settled on.

## Prerequisites

- [W01](01-naming-convention-review.md) merged with ADR-0001 in
  `Accepted` state.
- `make build`, `make test` green on `main`.

## In scope

### Step 1 — LICENSE

Pick a license. Default recommendation: **Apache-2.0** (broad
patent grant; corp-friendly). Alternatives: **MIT** (simpler, no
patent grant), **MPL-2.0** (file-level copyleft).

Add `LICENSE` at repo root. Add a `// SPDX-License-Identifier: …`
header expectation to `CONTRIBUTING.md`'s Step 5 in [W02](02-readme-and-contributor-docs.md)
(or, if W02 hasn't run yet, defer the header expectation to
[W08](08-phase0-cleanup-gate.md)).

### Step 2 — SECURITY.md

Add `SECURITY.md` at repo root:

- How to report a vulnerability (private email or GitHub Security
  Advisory).
- Supported versions (v0.x — security fixes for the latest minor;
  pre-v1.0 = no long-term support promise).
- Disclosure policy (90-day default; coordinated disclosure
  acceptable).

### Step 3 — CODEOWNERS

`.github/CODEOWNERS` declaring at minimum:

- Default owner for the repo.
- A separate owner for `proto/` (the wire contract — changes here
  ripple into the overlord repo).
- A separate owner for `sdk/` (published surface).

Use GitHub team handles, not individuals.

### Step 4 — Issue and PR templates

Under `.github/`:

- `ISSUE_TEMPLATE/bug_report.md` — reproduction steps, expected vs
  actual, version (`overseer --version`), environment.
- `ISSUE_TEMPLATE/feature_request.md` — what, why, alternatives
  considered.
- `ISSUE_TEMPLATE/config.yml` — disable blank issues; link to
  Discussions or the security advisory page.
- `pull_request_template.md` — what changed, why, how it's tested,
  workstream link if applicable, breaking-change disclosure.

Keep them short. Long templates discourage filing.

### Step 5 — Dependabot

Add `.github/dependabot.yml` covering:

- `gomod` ecosystem on the root, `sdk`, and `workflow` modules
  (weekly).
- `github-actions` ecosystem on `.github/workflows` (weekly).
- Group minor + patch updates per ecosystem to reduce PR noise.
- Ignore major-version bumps for now; require human-driven major
  bumps.

The recent dependabot PR (`#1`, otel 1.39 → 1.41) merged cleanly,
which is encouraging signal — formalize the config.

### Step 6 — Branch protection (advisory)

This isn't a code change, but the workstream should produce a
**suggested branch protection ruleset** in the workstream's
reviewer notes for `main`:

- Require PR review (1 approver minimum).
- Require status checks: `Test`, `Proto drift check`,
  `make example-plugin` once [W06](06-third-party-plugin-example.md)
  lands.
- Require linear history.
- Disallow force pushes.
- Disallow deletions.

The repo admin applies the ruleset; this workstream just proposes it.

### Step 7 — `.gitignore` housekeeping

Audit `.gitignore`:

- Confirm `bin/`, `/overseer`, `*.db`, `*.db-shm`, `*.db-wal` are
  present (they are, per the post-split sweep).
- Add anything the new templates and dependabot need (`.idea/`,
  `.vscode/` if the team is split on whether to track them — leave
  alone if there's an existing convention).

## Out of scope

- Setting up a documentation site (Hugo, Docusaurus, etc.).
- Setting up a release-automation workflow (goreleaser, etc.) —
  that's part of [W08](08-phase0-cleanup-gate.md).
- Code-of-conduct authoring. (Optional; if added, follow the
  Contributor Covenant.)
- Renaming the GitHub repo or org.

## Files this workstream may modify

- `LICENSE` (new).
- `SECURITY.md` (new).
- `.github/CODEOWNERS` (new).
- `.github/ISSUE_TEMPLATE/` (new directory).
- `.github/pull_request_template.md` (new).
- `.github/dependabot.yml` (new).
- `.gitignore` (audit only).

This workstream may **not** edit `README.md` (the LICENSE link
already exists and points at the file added here, so no edit
needed; if [W02](02-readme-and-contributor-docs.md) lands first
and changes the link, fine), `PLAN.md`, `AGENTS.md`, or other
workstream files.

## Tasks

- [x] Choose a license; add `LICENSE`.
- [x] Author `SECURITY.md`.
- [x] Author `.github/CODEOWNERS`.
- [x] Author the issue / PR templates.
- [x] Author `.github/dependabot.yml`.
- [x] Audit `.gitignore`.
- [x] Capture the suggested branch-protection ruleset in the
      workstream's reviewer notes.

## Exit criteria

- All Step 1–5 files exist and are reviewed.
- The README's `LICENSE` link resolves.
- Dependabot is configured for all three ecosystems we ship
  (root gomod, sdk gomod, workflow gomod, github-actions).
- The branch-protection proposal is captured for the admin to apply.

## Tests

None directly — these are repo-hygiene artifacts. The PR template
and CODEOWNERS take effect on the next PR after merge; verify by
opening one.

## Risks

| Risk | Mitigation |
|---|---|
| License choice is reversible only with significant cost | Pick conservatively; Apache-2.0 is the lowest-risk default for a corp-aware project. Document the choice in a one-paragraph ADR if non-default. |
| CODEOWNERS team handles don't exist on the GitHub org yet | Coordinate with the org admin to create the teams before merging this workstream. The fallback is named individuals, but switch to teams as soon as possible. |
| Dependabot creates excessive PR noise | Group minor + patch by ecosystem; review weekly cadence after one month and bump to monthly if noise persists. |
| Branch protection rules block legitimate emergency fixes | The proposal allows admin override; document the override expectation in the reviewer notes. |

## Reviewer Notes

### Implementation summary

All Step 1–5 files have been created. `make build` is green. No tests are
required for this workstream (per the Tests section above).

**Files created:**
- `LICENSE` — Apache-2.0 full text. The README's existing `See [LICENSE](LICENSE)` link now resolves.
- `SECURITY.md` — private reporting via GitHub Security Advisories (preferred) or email; 90-day coordinated disclosure; supported versions table; scope boundaries.
- `.github/CODEOWNERS` — default owner `@brokenbots/maintainers`; `proto/` adds `@brokenbots/platform`; `sdk/` adds `@brokenbots/sdk`; `.github/` and `Makefile` require maintainer sign-off. **Action required:** org admin must create the team handles before merging, otherwise CODEOWNERS review is silently skipped by GitHub.
- `.github/ISSUE_TEMPLATE/bug_report.md` — reproduction steps, expected/actual, version, environment.
- `.github/ISSUE_TEMPLATE/feature_request.md` — what/why/alternatives.
- `.github/ISSUE_TEMPLATE/config.yml` — blank issues disabled; links to Security Advisories and Discussions.
- `.github/pull_request_template.md` — what/why, testing checklist, breaking-change disclosure, workstream link field.
- `.github/dependabot.yml` — weekly gomod updates for `/`, `/sdk`, `/workflow`; weekly github-actions; minor+patch grouped per ecosystem; major bumps ignored (require human-driven).

**`.gitignore` changes:**
- All required entries (`bin/`, `/overseer`, `*.db`, `*.db-shm`, `*.db-wal`) confirmed present.
- Added: `.idea/`, `.vscode/`, `*.test`, `coverage.out`.

### Suggested branch-protection ruleset for `main`

Apply via **Repository → Settings → Branches → Add rule** (or a GitHub
Ruleset if the org is on GitHub Enterprise / Teams):

| Setting | Value |
|---|---|
| Require a pull request before merging | ✅ 1 approver minimum |
| Dismiss stale reviews on new push | ✅ |
| Require status checks to pass | ✅ `Test`, `Proto drift check` |
| Require branches to be up to date | ✅ |
| Require linear history | ✅ |
| Allow force pushes | ❌ |
| Allow deletions | ❌ |
| Include administrators | ✅ (with override documented below) |

**Emergency override:** if a critical fix must bypass review (e.g. prod is
down), a repo admin may temporarily disable the rule, merge, and re-enable
immediately. Document the override in the commit message and open a follow-up
PR for any process improvement.

Once [W06](06-third-party-plugin-example.md) lands, add `make example-plugin`
as a required status check.

### License choice rationale (ADR-inline)

Apache-2.0 was selected as the default: broad patent grant, corp-friendly,
OSI-approved, and the lowest-risk choice for a project that targets enterprise
workflows. MIT would also be acceptable; MPL-2.0 was rejected because
file-level copyleft adds friction for downstream integrators.
