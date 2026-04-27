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

W06 has already merged (`f2cf101`) and `make example-plugin` is already a step
inside the `Test` CI job (`.github/workflows/ci.yml`). It is covered by the
`Test` required status check — no separate admin action is needed for this item.

### License choice rationale (ADR-inline)

Apache-2.0 was selected as the default: broad patent grant, corp-friendly,
OSI-approved, and the lowest-risk choice for a project that targets enterprise
workflows. MIT would also be acceptable; MPL-2.0 was rejected because
file-level copyleft adds friction for downstream integrators.

## Reviewer Notes

### Review 2026-04-27 — changes-requested

#### Summary

All Step 1–5 artifacts are present and structurally complete. `make build` and `make test` are green. Exit criteria are substantially met. Three nits require executor remediation before approval: a potentially broken Discussions link in the issue template config, a vague email fallback in `SECURITY.md`, and a stale "once W06 lands" deference in the branch-protection proposal (W06 has already merged and `make example-plugin` already runs inside the `Test` CI job). No architectural concerns. No security blockers.

#### Plan Adherence

- **Step 1 — LICENSE**: ✅ `LICENSE` present with full Apache-2.0 canonical text. README `LICENSE` link resolves. License choice rationale captured.
- **Step 2 — SECURITY.md**: ✅ (with nit) Private reporting via GitHub Security Advisories (primary) and email (secondary). 90-day coordinated disclosure. Supported versions table. Scope boundaries. Email fallback is vague — see Required Remediations #2.
- **Step 3 — CODEOWNERS**: ✅ Default `@brokenbots/maintainers`; `proto/` adds `@brokenbots/platform`; `sdk/` adds `@brokenbots/sdk`; `.github/` and `Makefile` add maintainers. Warning about placeholder team handles present.
- **Step 4 — Issue and PR templates**: ✅ (with nit) `bug_report.md`, `feature_request.md`, `config.yml`, and `pull_request_template.md` all present and well-formed. `config.yml` Discussions URL may 404 — see Required Remediations #1.
- **Step 5 — Dependabot**: ✅ All four ecosystems covered (root gomod, sdk gomod, workflow gomod, github-actions). Weekly cadence. Minor+patch grouped per ecosystem. Major-version bumps ignored.
- **Step 6 — Branch protection (advisory)**: ✅ (with nit) All required ruleset elements captured. The deference "once W06 lands" is stale — W06 has merged and `make example-plugin` is already a gated step in the `Test` CI job — see Required Remediations #3.
- **Step 7 — .gitignore housekeeping**: ✅ All required entries confirmed present. `.idea/`, `.vscode/`, `*.test`, and `coverage.out` added.

#### Required Remediations

- **R1 — `.github/ISSUE_TEMPLATE/config.yml` Discussions URL (nit)**
  - File: `.github/ISSUE_TEMPLATE/config.yml` line 8
  - Problem: `https://github.com/brokenbots/overseer/discussions` will 404 if GitHub Discussions is not enabled on the repository. A broken link in the issue template config is a bad first experience for contributors trying to ask questions.
  - Acceptance criteria: Either (a) confirm in the executor's implementation notes that GitHub Discussions is enabled on the repo and the URL resolves, or (b) replace the Discussions link with a reachable alternative (e.g., remove the entry if no Discussions/forum channel exists yet, or point to a valid URL). The config must not include a link that 404s for users.

- **R2 — `SECURITY.md` email fallback vagueness (nit)**
  - File: `SECURITY.md` line 24
  - Problem: "Send details to the maintainers at the address listed in the GitHub org contact page" is not actionable. A reporter looking for an email address needs a direct, unambiguous contact path. If the org contact page changes or doesn't list an email, the fallback silently disappears.
  - Acceptance criteria: Replace the indirect reference with one of: (a) a concrete email address (e.g. `security@brokenbots.net` or similar), or (b) explicit text stating that GitHub Security Advisories is the only supported reporting channel and no public email is provided. The fallback must be deterministic and not depend on external page content.

- **R3 — Stale W06 deferral in branch-protection proposal (nit)**
  - Location: `workstreams/07-repo-hygiene.md`, Implementation summary, "Suggested branch-protection ruleset" section, final paragraph.
  - Problem: "Once [W06](06-third-party-plugin-example.md) lands, add `make example-plugin` as a required status check." W06 has already merged (`f2cf101`). Furthermore, `make example-plugin` is already a step inside the `Test` CI job (`.github/workflows/ci.yml` line 43) — it is not a separate status check and requires no additional admin action.
  - Acceptance criteria: Update the final paragraph in the branch-protection proposal to reflect that W06 has already landed and that `make example-plugin` is already covered within the `Test` required status check. No deferred admin action is needed for this item.

#### Test Intent Assessment

No automated tests exist for this workstream, which is correct per the workstream's own "Tests" section. The artifacts are configuration and documentation files that take effect on the next PR after merge. Test intent is N/A.

#### Validation Performed

```
make build   → success (bin/overseer produced)
make test    → all packages pass (cached)
git diff main..HEAD --stat → 10 files changed, 488 insertions(+), 7 deletions(-); matches expected file set
git ls-files LICENSE SECURITY.md .github/CODEOWNERS .github/dependabot.yml .github/pull_request_template.md .github/ISSUE_TEMPLATE/bug_report.md .github/ISSUE_TEMPLATE/feature_request.md .github/ISSUE_TEMPLATE/config.yml → all 8 files present
README.md line 149 grep for LICENSE → resolves to newly added file
```
