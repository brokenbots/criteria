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

- [ ] Append `release-artifacts` job to `.github/workflows/ci.yml`
      with the documented trigger condition.
- [ ] Implement the tag extraction in the `Extract RC tag` step.
- [ ] Build, bundle, and upload the artifact bundle.
- [ ] Generate `SHA256SUMS`.
- [ ] Save the runtime image as a tar.
- [ ] Author `docs/contributing/release-process.md`.
- [ ] Validate via the four scenarios in Step 4; document in
      reviewer notes.
- [ ] `make ci` green on the workstream branch.

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

## Risks

| Risk | Mitigation |
|---|---|
| The trigger condition fires on unrelated PRs whose title happens to contain `-rc` | The regex `-rc\d+\b` is specific to RC numbering. False positives are possible (e.g. a feature title containing "irc-something"); document the convention so contributors avoid the literal substring `-rc<N>`. If false positives become a problem, switch to branch-name-only triggering. |
| The artifact bundle is too large for the GitHub Actions free tier | Free tier provides 500 MB per artifact, 90 days retention by default. The runtime image alone may approach this. If size is an issue, exclude the image tar from the bundle and only upload binaries; document the trade-off. Ideally test once and confirm size before merging. |
| `docker save` fails because the build job did not have Docker available | `ubuntu-latest` runners have Docker installed. Verify by reading the runner's pre-installed software list. If a different runner is used, install Docker as a step. |
| Tag extraction produces an empty string for an unusual branch name | The job fails loudly with `ERROR: could not extract RC tag`. Operators see the error in the CI log and fix the branch name or title. |
| The `release-artifacts` job slows down CI on RC PRs | RC PRs are infrequent (one or two per release). The added build time is acceptable on the human-decision side of an RC. |
| `actions/upload-artifact@v4` is not the correct major version when this workstream lands | Pin to the same version used elsewhere in `ci.yml` (search for `actions/upload-artifact` in the workflows directory). If no precedent, use the latest stable major and document. |
