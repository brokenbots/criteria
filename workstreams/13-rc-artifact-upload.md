# Workstream 13 — Release-candidate artifact upload (CI)

**Owner:** Workstream executor · **Depends on:** [W09](09-docker-dev-container-and-runtime-image.md) (the runtime image is part of the artifact bundle).

## Context

Per the team's request: every PR that targets a release or
release-candidate (e.g. `0.3.0-rc1`, `v0.3.0-rc2`) should publish a
downloadable artifact bundle so reviewers can grab a binary without
rebuilding locally.

Today the project's release process produces tagged binaries via the
existing release workflow (whatever it is — likely a manual or
post-tag GitHub release). There is **no pre-tag artifact** during the
RC review window. This workstream adds one.

The mechanism: a GitHub Actions job that builds the full set of
release artifacts (CLI binary, all adapter plugin binaries, the
runtime container image from [W09](09-docker-dev-container-and-runtime-image.md),
and `SHA256SUMS`) and uploads them via `actions/upload-artifact@v4`.
The job is **gated on the PR head ref or title** carrying an RC
marker so it does not fire on every PR (artifact storage costs +
build time matters).

## Prerequisites

- [W09](09-docker-dev-container-and-runtime-image.md) merged so
  `Dockerfile.runtime` and `make docker-runtime` exist.
- `make ci` green on `main`.
- Familiarity with the existing
  [.github/workflows/ci.yml](../.github/workflows/ci.yml) jobs (lint,
  unit-tests, e2e, proto-drift).

## In scope

### Step 1 — Define the RC trigger condition

Two trigger criteria, joined by OR:

1. The PR head ref starts with `release/` (e.g. `release/v0.3.0-rc1`,
   `release/0.3.0-rc2`).
2. The PR title contains an RC marker matching the regex
   `-rc\d+\b`.

A canonical PR for v0.3.0-rc1 would have:
- branch: `release/v0.3.0-rc1`
- title: `Release v0.3.0-rc1`

The job condition in GitHub Actions YAML:

```yaml
if: |
  startsWith(github.head_ref, 'release/') ||
  contains(github.event.pull_request.title, '-rc')
```

Document the convention in `docs/contributing/release-process.md`
(create if absent — the convention is in scope here even if the
fuller release process is not).

### Step 2 — New `release-artifacts` job in CI

Append to [.github/workflows/ci.yml](../.github/workflows/ci.yml):

```yaml
  release-artifacts:
    name: Release artifacts (RC PRs only)
    runs-on: ubuntu-latest
    if: |
      github.event_name == 'pull_request' && (
        startsWith(github.head_ref, 'release/') ||
        contains(github.event.pull_request.title, '-rc')
      )
    needs: [unit-tests, e2e]
    steps:
      - uses: actions/checkout@v4

      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
          cache: true

      - name: Sync workspace
        run: go work sync

      - name: Extract RC tag from branch or title
        id: rc
        run: |
          # Prefer the branch name; fall back to title parsing.
          tag=""
          if [[ "${GITHUB_HEAD_REF}" == release/* ]]; then
            tag="${GITHUB_HEAD_REF#release/}"
          fi
          if [[ -z "$tag" ]]; then
            tag="$(echo "${PR_TITLE}" | grep -oE 'v?[0-9]+\.[0-9]+\.[0-9]+(-rc[0-9]+)?' | head -1 || true)"
          fi
          if [[ -z "$tag" ]]; then
            echo "ERROR: could not extract RC tag from branch or title"
            exit 1
          fi
          echo "tag=${tag}" >> "$GITHUB_OUTPUT"
        env:
          PR_TITLE: ${{ github.event.pull_request.title }}

      - name: Build CLI binary
        run: make build

      - name: Build adapter plugins
        run: make plugins

      - name: Build runtime container image
        run: make docker-runtime

      - name: Save runtime image as tar
        run: |
          docker save criteria/runtime:dev -o bin/criteria-runtime.tar

      - name: Generate SHA256SUMS
        working-directory: bin
        run: sha256sum criteria criteria-adapter-* criteria-runtime.tar > SHA256SUMS

      - name: Bundle artifacts
        run: |
          mkdir -p artifact
          cp bin/criteria bin/criteria-adapter-* bin/criteria-runtime.tar bin/SHA256SUMS artifact/

      - name: Upload artifact
        uses: actions/upload-artifact@v4
        with:
          name: criteria-${{ steps.rc.outputs.tag }}
          path: artifact/
          retention-days: 30
          if-no-files-found: error
```

Notes:

- `needs: [unit-tests, e2e]` ensures the artifact is built only after
  the standard CI gates pass. No reason to upload an artifact for a
  failing CI run.
- `retention-days: 30` is the documented retention window. Adjust if
  the team wants longer; 30 is the default and covers a typical
  RC review cycle.
- `if-no-files-found: error` is a safety check — if the build silently
  produced no binaries, the job fails loudly.
- The runtime image is saved as a tar so reviewers can `docker load`
  it without registry access.
- The `tag` extraction handles both branch names like
  `release/v0.3.0-rc1` and PR titles like
  `Release v0.3.0-rc2: <description>`. Edge-case-tested in Step 4.

### Step 3 — Document the release process convention

Create `docs/contributing/release-process.md`:

1. **What this is.** A pre-tag, RC-only artifact upload to make
   release candidates reviewable without rebuilding locally.
2. **How to trigger it.** Open a PR with one of:
   - branch name starts with `release/` (e.g. `release/v0.3.0-rc1`)
   - PR title contains `-rc<N>` (e.g. `Release v0.3.0-rc1: ...`)
3. **What gets uploaded.** The CLI binary, all adapter plugins, the
   runtime container image as a tar, and a `SHA256SUMS` file.
4. **Where to find it.** GitHub Actions tab → the PR's `release-artifacts`
   job → "Artifacts" panel.
5. **Retention.** 30 days from the workflow run.
6. **What this is not.** This is for *reviewing* an RC, not for
   distributing the final release. The final tagged release uses the
   existing release workflow (whatever exists today) and publishes
   to the standard release page.

Do **not** edit `README.md`, `PLAN.md`, `AGENTS.md`, `CHANGELOG.md`.

### Step 4 — Test the trigger logic

Validation steps (manual; document in reviewer notes):

1. Open a regular feature-branch PR (e.g. branch
   `feat/some-feature`, title `Add some feature`). Confirm the
   `release-artifacts` job is **skipped** in the CI run.
2. Rename a sandbox branch to `release/test-rc1`, push, open a PR.
   Confirm the job **runs** and produces an artifact named
   `criteria-test-rc1`.
3. On a regular branch, change the PR title to `Test: v0.0.0-rc1`.
   Confirm the job **runs** and produces an artifact named
   `criteria-v0.0.0-rc1`.
4. Confirm the artifact contains the expected files via
   `unzip -l <artifact>` or download + inspect.

If GitHub Actions does not support testing the trigger without
opening real PRs, the workstream may submit a draft PR specifically
for the validation pass. Document the URLs.

## Behavior change

**No engine behavior change. CI behavior changes only.**

- New CI job `release-artifacts` that runs only on RC PRs.
- New artifact appears in the CI run's artifact panel.
- New convention: branch names `release/*` and PR titles `*-rc*`
  trigger the artifact upload.
- No CLI flag, HCL surface, log line, or runtime change.

## Reuse

- Existing `make build`, `make plugins` targets.
- `make docker-runtime` from [W09](09-docker-dev-container-and-runtime-image.md).
- Existing `actions/checkout@v4`, `actions/setup-go@v5`,
  `actions/upload-artifact@v4` — same versions as the rest of
  `ci.yml`.
- Existing CI YAML structure. Append to it; do not refactor.

## Out of scope

- Multi-arch artifact builds (linux/arm64, darwin). Phase 2 ships
  linux/amd64 only; multi-arch is a follow-up if asked for.
- Code signing (GPG, sigstore). Out.
- Publishing the runtime image to a registry from the RC PR. Image
  is uploaded as a tar artifact only; registry publish is the final
  release process.
- Auto-creating a GitHub release draft. The artifact is linked from
  the PR; the human committer creates the actual release.
- Changing the existing `lint`, `unit-tests`, `e2e`, `proto-drift`
  jobs. Untouched.
- Building Windows binaries. The CLI is Linux/macOS focused.

## Files this workstream may modify

- `.github/workflows/ci.yml` (append the `release-artifacts` job).
- `docs/contributing/release-process.md` (new).
- `Makefile` (no changes expected; the new job uses existing
  targets).

This workstream may **not** edit `README.md`, `PLAN.md`, `AGENTS.md`,
`CHANGELOG.md`, `workstreams/README.md`, or any other workstream file.
It may **not** edit any code under `internal/`, `cmd/`, `workflow/`,
`sdk/`, or `events/` — the artifacts are the existing binaries.

## Tasks

- [x] Append `release-artifacts` job to `.github/workflows/ci.yml`
      with the documented trigger condition.
- [x] Implement the tag extraction in the `Extract RC tag` step.
- [x] Build, bundle, and upload the artifact bundle.
- [x] Generate `SHA256SUMS`.
- [x] Save the runtime image as a tar.
- [x] Author `docs/contributing/release-process.md`.
- [x] Validate via the four scenarios in Step 4; document in
      reviewer notes.
- [x] `make ci` green on the workstream branch.

## Exit criteria

- A PR with branch `release/v0.3.0-rcX` produces a downloadable
  artifact named `criteria-v0.3.0-rcX`.
- A PR with title containing `-rc1` (and any branch name) also
  produces the artifact.
- A regular PR (no RC marker) does **not** trigger the job.
- The artifact contains: `criteria`, `criteria-adapter-copilot`,
  `criteria-adapter-mcp`, `criteria-adapter-noop`,
  `criteria-runtime.tar`, `SHA256SUMS`.
- `SHA256SUMS` is verifiable: a reviewer can `sha256sum -c`
  successfully.
- The runtime image tar is loadable: `docker load -i criteria-runtime.tar`
  succeeds.
- `docs/contributing/release-process.md` documents the convention.
- `make ci` green.

## Tests

This workstream does not add Go tests. Verification is the four
scenarios in Step 4, captured in reviewer notes with PR / run
URLs.

## Reviewer notes

#### Implementation (2026-04-30)

**Files changed:**
- `.github/workflows/ci.yml` — appended the `release-artifacts` job
  after `proto-drift`. Exact spec from the workstream was used verbatim.
  `needs: [unit-tests, e2e]` gates the artifact build on CI success.
  `if-no-files-found: error` ensures a silent empty build fails loudly.
- `docs/contributing/release-process.md` — new file documenting the
  trigger convention, artifact contents, download path, retention window,
  and verification commands.

**`make ci` result:** all gates pass (build, tests, lint-imports,
lint-go, lint-baseline-check, validate, example-plugin). Baseline
remains at 70/70 — no new suppressions added.

**Security pass:** the tag extraction uses only `$GITHUB_HEAD_REF` and
`$PR_TITLE` (passed as an env var, not shell-interpolated), and writes
to `$GITHUB_OUTPUT` only. No secrets are accessed. `docker save` writes
only to the local `bin/` directory. `sha256sum` and `cp` are
standard Linux utilities with no injection surface.

**Step 4 live validation** (complete — all four scenarios executed on GitHub Actions):

- **Scenario 1** — regular PR, no RC marker: PR #47 (branch
  `ci/scenario1-regular-pr`, title `ci: regular feature PR — no RC
  marker`). The `Release artifacts (RC PRs only)` job shows conclusion
  `skipped` in run
  https://github.com/brokenbots/overseer/actions/runs/25176609963.
  ✓

- **Scenario 2** — `release/test-rc1` branch trigger (exact spec):
  PR #49 (branch `release/test-rc1`, title `Release test-rc1 (W13
  Scenario 2 validation)`). Job ran and produced artifact
  `criteria-test-rc1` (128 MB) in run
  https://github.com/brokenbots/overseer/actions/runs/25177574297.
  ✓

- **Scenario 3** — title-only trigger, non-`release/` branch: PR #48
  (branch `ci/scenario3-title-trigger`, title `Test: v0.0.0-rc1 (W13
  Scenario 3 validation)`). Job ran and produced artifact
  `criteria-v0.0.0-rc1` (128 MB) in run
  https://github.com/brokenbots/overseer/actions/runs/25176611093.
  ✓

- **Scenario 4** — artifact contents, checksum verification, and
  runtime-image loadability. Artifact from PR #45 downloaded and
  extracted locally.

  ```
  Archive:  criteria-v0.0.0-rc1.zip
    Length      Date    Time    Name
  ---------  ---------- -----   ----
        428  04-30-2026 16:08   SHA256SUMS
   27523530  04-30-2026 16:08   criteria
   21741197  04-30-2026 16:08   criteria-adapter-copilot
   19554597  04-30-2026 16:08   criteria-adapter-mcp
   19317660  04-30-2026 16:08   criteria-adapter-noop
  168259584  04-30-2026 16:08   criteria-runtime.tar
  ---------                     -------
  256396996                     6 files
  ```

  `sha256sum -c SHA256SUMS` — all five files: `OK`. ✓

  `docker load -i criteria-runtime.tar` — Docker 29.3.1 (macOS):

  ```
  Loaded image: criteria/runtime:dev
  ```
  ✓

**Extraction logic fix (2026-04-30 pass 3):** Step 2 was changed from
`v?X.Y.Z(-rcN)?` (optional suffix) to `v?X.Y.Z-rcN` (required suffix)
so that a title like `Release v1.2.3 prep -rc1` can no longer produce
the bare semver `v1.2.3` as an artifact tag. Updated regression test
(10 cases, all PASS):
```
PASS  branch release/test-rc1           => test-rc1
PASS  branch release/v0.3.0-rc1         => v0.3.0-rc1
PASS  title semver+rc (non-release br)  => v0.0.0-rc1
PASS  title -rcN only (no semver)       => rc2
PASS  title random -rc1 without ver     => rc1
PASS  Bugfix foo-rc — no digit          => <empty>   (job fails loudly)
PASS  Release v1.2.3 prep -rc1          => rc1       (was v1.2.3 — now fixed)
PASS  Release v1.2.3 stable (no RC)     => <empty>   (job fails loudly)
PASS  regular feature PR                => <empty>
PASS  title irc without digit           => <empty>
```

## Risks

| Risk | Mitigation |
|---|---|
| The trigger condition fires on unrelated PRs whose title happens to contain `-rc` | The regex `-rc\d+\b` is specific to RC numbering. False positives are possible (e.g. a feature title containing "irc-something"); document the convention so contributors avoid the literal substring `-rc<N>`. If false positives become a problem, switch to branch-name-only triggering. |
| The artifact bundle is too large for the GitHub Actions free tier | Free tier provides 500 MB per artifact, 90 days retention by default. The runtime image alone may approach this. If size is an issue, exclude the image tar from the bundle and only upload binaries; document the trade-off. Ideally test once and confirm size before merging. |
| `docker save` fails because the build job did not have Docker available | `ubuntu-latest` runners have Docker installed. Verify by reading the runner's pre-installed software list. If a different runner is used, install Docker as a step. |
| Tag extraction produces an empty string for an unusual branch name | The job fails loudly with `ERROR: could not extract RC tag`. Operators see the error in the CI log and fix the branch name or title. |
| The `release-artifacts` job slows down CI on RC PRs | RC PRs are infrequent (one or two per release). The added build time is acceptable on the human-decision side of an RC. |
| `actions/upload-artifact@v4` is not the correct major version when this workstream lands | Pin to the same version used elsewhere in `ci.yml` (search for `actions/upload-artifact` in the workflows directory). If no precedent, use the latest stable major and document. |

#### Review 2026-04-30 — changes-requested

#### Summary
The workflow and release-process doc are in place, and `make ci` is green locally, but this is not approvable yet. Two blockers remain: the title-trigger contract and the title-to-tag extraction logic do not accept the same set of PR titles, and the required live PR validation for the GitHub Actions behavior is still entirely pending. I did not find a separate shell-injection, secret-handling, or path-safety issue in the reviewed workflow steps.

#### Plan Adherence
- The `release-artifacts` job, artifact bundling, checksum generation, runtime-image tar export, and `docs/contributing/release-process.md` are implemented in the allowed files.
- Step 1 is only partially satisfied: `.github/workflows/ci.yml:135-165` and `docs/contributing/release-process.md:14-29` document/title-gate on `-rc`, but the extractor only succeeds when the title also contains a parseable semver token.
- Step 4 and the corresponding exit criteria are still unmet: `workstreams/13-rc-artifact-upload.md:297-308` explicitly leaves every live validation scenario pending and provides no PR or workflow-run URLs.

#### Required Remediations
- **Blocker** — `.github/workflows/ci.yml:135-165`, `docs/contributing/release-process.md:14-29`: align the trigger contract with the extraction contract. Right now a title-only PR can satisfy the documented/workflow RC trigger and still fail before upload because the extractor requires a semantic-version token. **Acceptance criteria:** either tighten the workflow condition and docs so title-based triggering only occurs for the exact parseable RC title format the extractor supports, or broaden extraction so every documented RC-title format yields a non-empty artifact tag. Include one negative-case proof showing a non-release PR title does not run the job.
- **Blocker** — `workstreams/13-rc-artifact-upload.md:297-308` and Step 4 / Exit criteria: complete the required live GitHub validation and record the evidence. **Acceptance criteria:** add PR/run URLs proving (1) a regular PR skips `release-artifacts`, (2) a `release/test-rc1` branch PR runs and uploads `criteria-test-rc1`, (3) a non-`release/` branch PR with title `Test: v0.0.0-rc1` runs and uploads `criteria-v0.0.0-rc1`, and (4) the downloaded artifact contains the expected files. Also include evidence that `sha256sum -c SHA256SUMS` succeeds and `docker load -i criteria-runtime.tar` succeeds on the downloaded artifact, because both are explicit exit criteria.

#### Test Intent Assessment
Existing repository validation is still strong enough to show the workflow/doc edits did not break the normal build, test, lint, or example paths. The missing piece is contract-level proof for the GitHub Actions behavior itself: there is still no executed evidence for the skip path, the two run paths, the published artifact name, the downloaded artifact contents, checksum verification, or runtime-image loadability. A local reproduction of the extraction snippet covered the happy paths (`release/test-rc1`, `Test: v0.0.0-rc1`) but also showed `random -rc1 without version` produces an empty tag, so the current checks do not yet prove the intended title-trigger behavior.

#### Validation Performed
- `make ci` — passed locally.
- Local reproduction of the RC tag extraction logic — `release/test-rc1` => `test-rc1`; `Test: v0.0.0-rc1` => `v0.0.0-rc1`; `Add some feature` => empty; `random -rc1 without version` => empty.
- `make docker-runtime` — could not be completed locally in this environment because the Docker daemon was unavailable, so runtime-image validation still needs the live CI evidence above.

#### Review 2026-04-30-02 — changes-requested

##### Summary
The new pass closes part of the prior review: the skip path, both upload paths, artifact downloadability, and checksum verification are now evidenced. This is still not approvable because the title-trigger contract remains inconsistent with the documented RC marker rules, and the Step 4 validation log is still incomplete: it substitutes Scenario 2 with a different branch shape and still does not provide a successful `docker load` on the downloaded runtime tar. I did not find a separate shell-injection, secret-handling, or path-safety issue in the workflow steps I reviewed.

#### Plan Adherence
- `.github/workflows/ci.yml` and `docs/contributing/release-process.md` remain within the allowed file set and implement the requested artifact build, bundle, upload, and documentation flow.
- Step 4 is only partially satisfied. The recorded live runs now prove: a regular PR skips the job, a `release/v0.0.0-rc1` PR uploads `criteria-v0.0.0-rc1`, and a title-only PR uploads `criteria-v0.0.0-rc1`. The downloaded artifact also contains the expected six files and its `SHA256SUMS` file verifies successfully.
- Step 1 is still only partially satisfied: `.github/workflows/ci.yml:135-166` triggers on any title containing `-rc`, while `docs/contributing/release-process.md:16-30` documents `-rc<N>` / semver+rc title formats and the extractor only partially normalizes those cases.
- Step 4 / Exit criteria are still unmet at `workstreams/13-rc-artifact-upload.md:306-345`: Scenario 2 was not executed as written (`release/test-rc1` => `criteria-test-rc1`), and the `docker load -i criteria-runtime.tar` exit criterion is explicitly waived rather than evidenced.

#### Required Remediations
- **Blocker** — `.github/workflows/ci.yml:135-166`, `docs/contributing/release-process.md:16-30`: the title-trigger contract is still broader than the documented RC marker rules and can produce bad outcomes. With the current extractor, `Bugfix foo-rc` still satisfies the job `if:` but yields an empty tag, and `Release v1.2.3 prep -rc1` yields `v1.2.3`, which is not an RC artifact tag. **Acceptance criteria:** make the job trigger, the title parser, and the documentation agree on the exact title formats that are allowed; ensure title-triggered artifacts always resolve to an RC tag (`<semver>-rcN` or `rcN`), never a plain semver; and include proof for at least one boundary case that currently misbehaves.
- **Blocker** — `workstreams/13-rc-artifact-upload.md:306-317`: complete Scenario 2 exactly as specified in Step 4. The current evidence uses `release/v0.0.0-rc1`, but the plan required a sandbox branch named `release/test-rc1` and an uploaded artifact named `criteria-test-rc1`. **Acceptance criteria:** add the PR URL and workflow-run URL for a live `release/test-rc1` validation and record the uploaded artifact name.
- **Blocker** — `workstreams/13-rc-artifact-upload.md:320-345`: provide actual evidence that `docker load -i criteria-runtime.tar` succeeds on the downloaded artifact. `docker save` succeeding in CI is not the same contract. **Acceptance criteria:** run `docker load -i criteria-runtime.tar` against the downloaded RC artifact on a host with a running Docker daemon and record the successful command output (or a linked log) in the reviewer notes. Do not self-waive this exit criterion.

#### Test Intent Assessment
The current evidence is materially stronger than the previous pass: repository CI is green, the GitHub Actions skip/run paths are real, both artifact-upload paths produce downloadable bundles, and the downloaded bundle contents plus checksum verification prove the artifact is structurally correct. The remaining gaps are still contract-level: there is no live proof for the non-semver `release/<tag>` branch case, no successful `docker load` of the shipped tar, and the title parser still accepts or misclassifies boundary-case titles in ways the docs do not describe.

#### Validation Performed
- `make ci` — passed locally.
- `gh run view 25175923821 --repo brokenbots/overseer --json ...` — confirmed `release/v0.0.0-rc1` run success and `Release artifacts (RC PRs only)` job success.
- `gh run view 25176609963 --repo brokenbots/overseer --json ...` — confirmed the regular-PR scenario and `Release artifacts (RC PRs only)` job conclusion `skipped`.
- `gh run view 25176611093 --repo brokenbots/overseer --json ...` — confirmed the title-only RC scenario and `Release artifacts (RC PRs only)` job success.
- `gh run download 25175923821 -n criteria-v0.0.0-rc1 ...` and `gh run download 25176611093 -n criteria-v0.0.0-rc1 ...` — both artifact downloads succeeded, confirming the recorded artifact names exist on GitHub.
- `sha256sum -c SHA256SUMS` in the downloaded run-45 artifact — all five files verified `OK`.
- `docker load -i criteria-runtime.tar` in the downloaded run-45 artifact — not verifiable in this environment because the local Docker daemon was unavailable (`Cannot connect to the Docker daemon ...`); no alternate success evidence was recorded in the workstream notes.
- Local extractor probe against the workflow snippet — `Hotfix -rc2 for storage` => `rc2`; `Bugfix foo-rc` => empty; `Release v1.2.3 prep -rc1` => `v1.2.3`.

#### Review 2026-04-30-03 — approved

##### Summary
The prior blockers are resolved and the workstream now meets the acceptance bar. The exact `release/test-rc1` validation path is recorded with a real PR and successful workflow run, the named artifact exists on GitHub, the title-based extractor no longer produces bare semver artifact tags, and the Step 4 notes now include checksum verification plus a successful `docker load` result for the downloaded runtime tar.

#### Plan Adherence
- Step 1 is satisfied: `.github/workflows/ci.yml` keeps the requested RC-only gate, and the extractor in `.github/workflows/ci.yml:152-172` now requires a semver `-rcN` suffix before emitting a semver-based artifact tag, with an `-rcN` fallback for title-only markers.
- Step 2 is satisfied: the `release-artifacts` job builds the CLI, plugins, runtime image tar, checksum file, bundles the expected outputs, and uploads them with the requested retention and safety settings.
- Step 3 is satisfied: `docs/contributing/release-process.md` documents the trigger convention, artifact contents, retrieval path, verification commands, and the title-extraction/failure behavior that operators need to understand.
- Step 4 and the exit criteria are satisfied: the notes now include live evidence for the skip path, the exact `release/test-rc1` branch-trigger path, the title-only trigger path, the artifact file list, successful checksum verification, and successful runtime-image loading.

#### Test Intent Assessment
This workstream’s contract is GitHub Actions behavior rather than Go runtime behavior, and the current evidence now exercises that contract at the right level. The skip case proves the gating behavior, the two positive PR scenarios prove both trigger paths and artifact names, the downloaded bundles prove the published contents, and the updated extractor regression cases show the title parser no longer regresses to plain semver tags on ambiguous RC titles.

#### Validation Performed
- `make ci` — passed locally on current `HEAD`.
- `gh pr view 49 --repo brokenbots/overseer --json ...` — confirmed PR #49 exists for the exact `release/test-rc1` Scenario 2 validation.
- `gh run view 25177574297 --repo brokenbots/overseer --json ...` — confirmed the `release/test-rc1` run succeeded.
- `gh run download 25177574297 --repo brokenbots/overseer -n criteria-test-rc1 ...` — succeeded, confirming the exact Scenario 2 artifact name exists on GitHub.
- Replayed the current extractor logic locally — `Release v1.2.3 prep -rc1` => `rc1`, `Bugfix foo-rc` => empty, `Hotfix -rc2 for storage` => `rc2`, `Release v0.3.0-rc1: ship it` => `v0.3.0-rc1`, `release/test-rc1` => `test-rc1`.
- Reviewed the recorded Step 4 evidence in this workstream for artifact contents, `sha256sum -c SHA256SUMS`, and successful `docker load -i criteria-runtime.tar`.
