# Workstream 06 — Release process integrity (CI tag-claim guard + real release workflow)

**Phase:** 3 · **Track:** A · **Owner:** Workstream executor · **Depends on:** Phase 2 closed at `v0.2.0` (the W16 cleanup gate must have pushed the `v0.2.0` tag to remote — that tag is the first input to the new guard). · **Unblocks:** [21-phase3-cleanup-gate.md](21-phase3-cleanup-gate.md) (gates the `v0.3.0` tag on this workstream's checks).

## Context

[TECH_EVALUATION-20260501-01.md](../../tech_evaluations/TECH_EVALUATION-20260501-01.md) §1 ranks **release-process integrity** as the #1 critical-severity tech debt: the `v0.2.0` claim in [PLAN.md](../../PLAN.md) and [CHANGELOG.md](../../CHANGELOG.md) was unbacked by an actual tag at the time of the eval. The v0.2.0 tag is now on remote (Phase 2 W16 prerequisite), but the same failure mode can recur in Phase 3 unless CI enforces it.

Two deliverables:

1. **Tag-claim guard** — CI fails when a tracked doc claims a tag that does not resolve via `git ls-remote --tags origin`.
2. **Real release workflow** — distinct from the existing RC artifact upload ([archived/v2/13-rc-artifact-upload.md](../archived/v2/13-rc-artifact-upload.md)). Cuts a real GitHub Release on tag push, builds + checksums + signs the binaries + the runtime image, publishes the release.

Both are blockers on the `v0.3.0` tag at [21](21-phase3-cleanup-gate.md): a Phase 3 close that ships docs claiming a tag that doesn't exist is a regression on the Phase 2 close-out's #1 finding.

## Prerequisites

- `v0.2.0` tag exists on remote (`git ls-remote --tags origin refs/tags/v0.2.0` returns a sha).
- `make ci` green on `main`.
- Repository has secrets configured for the signing path: `RELEASE_SIGNING_KEY` (cosign / minisign / GPG private key — pick one in Step 3 below), `RELEASE_SIGNING_PASSWORD`. If the secrets are not yet configured, the workstream surfaces the gap and the release workflow lands wired but disabled by repository settings; this is acceptable as long as the secret-prerequisites are documented and the workflow is otherwise mergeable.

## In scope

### Step 1 — Tag-claim guard CI job

Add a CI job in [.github/workflows/ci.yml](../../.github/workflows/ci.yml) named `tag-claim-check`. Job logic:

```yaml
tag-claim-check:
  runs-on: ubuntu-latest
  steps:
    - uses: actions/checkout@v4
      with:
        fetch-depth: 0
    - name: Extract tag claims from tracked docs
      id: extract
      run: |
        # Find any "vX.Y.Z" string preceded by "tag" or "Tag" or in a CHANGELOG
        # release-line (## [vX.Y.Z]). Output the unique tag list.
        ./tools/release/extract-tag-claims.sh > claims.txt
        cat claims.txt
    - name: Verify each claim resolves on origin
      run: |
        FAIL=0
        while IFS= read -r tag; do
          if ! git ls-remote --tags --exit-code origin "refs/tags/${tag}" >/dev/null; then
            echo "::error::Doc claims tag ${tag} but origin has no such tag"
            FAIL=1
          fi
        done < claims.txt
        exit "${FAIL}"
```

Author `tools/release/extract-tag-claims.sh`. The script must:

- Scan: [README.md](../../README.md), [PLAN.md](../../PLAN.md), [CHANGELOG.md](../../CHANGELOG.md), [workstreams/README.md](../README.md), every file under [docs/](../../docs/) and [docs/roadmap/](../../docs/roadmap/).
- **Skip:** [workstreams/archived/](../archived/) (historical claims are immutable), [tech_evaluations/](../../tech_evaluations/) (eval reports document past state), `.git/`.
- Match: lines containing `v[0-9]+\.[0-9]+\.[0-9]+` AND one of: the word "tag", "release", or `## [vX.Y.Z]` markdown heading shape.
- Emit unique tags one per line.
- Test the script locally before submission: it should emit at minimum `v0.1.0` and `v0.2.0` from the current HEAD.

Naming: `tools/release/extract-tag-claims.sh` is bash; make it executable (`chmod +x`).

Add a unit-style smoke test for the script under [tools/release/tests/](../../tools/release/tests/) (or equivalent) — given a fixture markdown file claiming `v9.9.9`, the script emits `v9.9.9`.

### Step 2 — Wire the guard into CI

In [.github/workflows/ci.yml](../../.github/workflows/ci.yml), add `tag-claim-check` to the `needs:` list of any aggregator job (so a failing tag claim breaks the PR).

The guard runs on every PR and every push to `main`. It must **not** run on tag pushes (when a new tag is being created, the new tag obviously doesn't exist on origin until after the push completes).

Gate via:

```yaml
on:
  push:
    branches: [main]
  pull_request:
```

Do not add `on: push: tags: 'v*'` — that's the release workflow (Step 3).

### Step 3 — Real release workflow

Add `.github/workflows/release.yml`. Trigger:

```yaml
on:
  push:
    tags:
      - 'v[0-9]+.[0-9]+.[0-9]+'   # only release tags; not RCs (those are -rc<N>)
```

Jobs (sequential):

1. **`build`** — checkout, set up Go via [go.mod](../../go.mod) version, run `make build` and `make plugins` for darwin-amd64, darwin-arm64, linux-amd64, linux-arm64 via `GOOS`/`GOARCH`. Produce one tarball per `os/arch`: `criteria-${TAG}-${OS}-${ARCH}.tar.gz` containing `criteria` + every `criteria-adapter-*` binary + `LICENSE` + `README.md`.
2. **`docker-image`** — build [Dockerfile.runtime](../../Dockerfile.runtime), tag as `criteria/runtime:${TAG}` and `criteria/runtime:latest`. **Do not push** to a registry yet (registry choice and credentials are explicit secrets); produce a tar (`docker save -o`) as a release artifact named `criteria-runtime-${TAG}.tar`.
3. **`checksum-and-sign`** — for every artifact from `build` and `docker-image`, compute SHA256 and append to `SHA256SUMS`. Sign `SHA256SUMS` using **cosign keyless** (preferred — uses GitHub OIDC, no key management) producing `SHA256SUMS.sig` and `SHA256SUMS.cert`. If keyless cosign is not viable in the project's CI account, fall back to `cosign sign-blob` with a key from `RELEASE_SIGNING_KEY` secret.
4. **`release`** — `gh release create ${TAG}` with all tarballs, the docker image tar, `SHA256SUMS`, `SHA256SUMS.sig`, `SHA256SUMS.cert`. Title: `${TAG}`. Body: pulled from `CHANGELOG.md` between the `## [vX.Y.Z]` heading and the next heading.

Document each step in [docs/contributing/release-process.md](../../docs/contributing/release-process.md), updating it from the current "RC artifacts only, unsigned, unpublished" stance.

### Step 4 — Document the release-vs-RC distinction

In [docs/contributing/release-process.md](../../docs/contributing/release-process.md), add a section "Release vs RC artifact":

- **RC artifact** ([archived/v2/13-rc-artifact-upload.md](../archived/v2/13-rc-artifact-upload.md)): triggered by RC PR or `-rc<N>` tag; uploads to the PR's Artifacts panel; not signed; not published.
- **Release** (this workstream): triggered by `vX.Y.Z` tag push; uploads to GitHub Releases; signed; published.

Cross-link both in [README.md](../../README.md). **This workstream cannot edit [README.md](../../README.md)** — record the cross-link addition as a deferred edit for [21](21-phase3-cleanup-gate.md).

### Step 5 — Verify against the existing `v0.2.0` tag

Run the guard locally as a self-test (the v0.2.0 tag exists on remote per the prerequisite):

```sh
./tools/release/extract-tag-claims.sh
# Expect at least: v0.1.0 v0.2.0
git ls-remote --tags origin refs/tags/v0.1.0   # exit 0
git ls-remote --tags origin refs/tags/v0.2.0   # exit 0
```

Both must exit 0. If `v0.2.0` is missing, the prerequisite was not satisfied — stop and reconcile with [21-phase3-cleanup-gate.md](21-phase3-cleanup-gate.md) (Phase 2's W16 was supposed to push it).

The release workflow is harder to dry-run without actually creating a tag. The acceptable proxy:

- Use [`act`](https://github.com/nektos/act) locally (if installed) to run the `release.yml` workflow against a synthetic `v0.0.1-test` tag. Verify each step would execute (no YAML parse error, no missing secret crash on the local mock).
- Document the dry-run in reviewer notes.
- The first real test is the actual `v0.3.0` tag at [21](21-phase3-cleanup-gate.md). If the workflow fails there, [21](21-phase3-cleanup-gate.md) blocks the close until this workstream is fixed.

### Step 6 — Validation

```sh
./tools/release/extract-tag-claims.sh    # exit 0; emits ≥ v0.1.0, v0.2.0
chmod -x tools/release/extract-tag-claims.sh; ./tools/release/extract-tag-claims.sh; chmod +x tools/release/extract-tag-claims.sh
make ci                                  # tag-claim-check job present and green
yamllint .github/workflows/release.yml   # if available
yamllint .github/workflows/ci.yml
```

`make ci` invocation must include the new `tag-claim-check` job in the matrix. Verify by inspecting the CI run.

## Behavior change

**Behavior change: yes** — for CI, not for runtime.

Observable differences:

- New CI job `tag-claim-check` runs on every PR and every push to `main`. Failure blocks merge.
- New release workflow `release.yml` runs on `vX.Y.Z` tag push. Existing `v0.1.0` and `v0.2.0` tags are not retroactively re-released.
- New script `tools/release/extract-tag-claims.sh` exists and is executable.
- [docs/contributing/release-process.md](../../docs/contributing/release-process.md) is rewritten from "no published releases" to "release workflow on tag, RC workflow on PR".

No code change. No HCL change. No SDK change. No proto change.

## Reuse

- Existing `Dockerfile.runtime` build path used by [archived/v2/09-docker-dev-container-and-runtime-image.md](../archived/v2/09-docker-dev-container-and-runtime-image.md).
- Existing RC artifact workflow as a pattern reference (do not copy verbatim — RC and release have different failure modes).
- Existing `make build`, `make plugins`, `make docker-runtime` targets. Do not reimplement.
- [`gh`](https://cli.github.com/) CLI for the GitHub Release create step (already used in CI per existing workflows).

## Out of scope

- Pushing the runtime Docker image to a registry. Registry choice (Docker Hub vs GHCR vs ECR) is a project-level decision; this workstream produces the image as a release artifact only.
- Backfilling release notes for `v0.1.0` and `v0.2.0` — those tags are already on remote; if a release is missing for them, that's a separate doc PR, not this workstream.
- Signing the binaries themselves (in addition to `SHA256SUMS`). Modern signing practice signs the checksum manifest; per-binary signing is overkill for this scope.
- TypeScript proto bindings — see [PLAN.md](../../PLAN.md) deferred items (carried forward by [21](21-phase3-cleanup-gate.md)).
- Editing [README.md](../../README.md), [PLAN.md](../../PLAN.md), [CHANGELOG.md](../../CHANGELOG.md), [workstreams/README.md](../README.md). Coordination set, owned by [21](21-phase3-cleanup-gate.md).

## Files this workstream may modify

- New: [`.github/workflows/release.yml`](../../.github/workflows/release.yml).
- [`.github/workflows/ci.yml`](../../.github/workflows/ci.yml) — add the `tag-claim-check` job.
- New: `tools/release/extract-tag-claims.sh`.
- New: `tools/release/tests/extract-tag-claims_test.sh` (or equivalent script-level test).
- New: any `tools/release/tests/testdata/*` fixtures used by the script test.
- [`docs/contributing/release-process.md`](../../docs/contributing/release-process.md) — full rewrite per Step 4.

This workstream may **not** edit:

- [`PLAN.md`](../../PLAN.md), [`README.md`](../../README.md), [`AGENTS.md`](../../AGENTS.md), [`CHANGELOG.md`](../../CHANGELOG.md), [`workstreams/README.md`](../README.md), or any other workstream file.
- Source code (`.go`, `.proto`, `.hcl`).
- Existing CI workflows other than [`ci.yml`](../../.github/workflows/ci.yml) (do not modify the RC artifact workflow).
- Generated files.

## Tasks

- [ ] Author `tools/release/extract-tag-claims.sh` (Step 1).
- [ ] Author the script's smoke test (Step 1).
- [ ] Add `tag-claim-check` job to [`ci.yml`](../../.github/workflows/ci.yml) (Step 2).
- [ ] Author [`release.yml`](../../.github/workflows/release.yml) (Step 3).
- [ ] Rewrite [`docs/contributing/release-process.md`](../../docs/contributing/release-process.md) (Step 4).
- [ ] Document the [README.md](../../README.md) cross-link addition as a deferred edit for [21](21-phase3-cleanup-gate.md) (Step 4 final paragraph).
- [ ] Self-test the guard against `v0.1.0` / `v0.2.0` (Step 5).
- [ ] Dry-run `release.yml` locally with `act` if available; document in reviewer notes (Step 5).
- [ ] `make ci` green with the new job present.

## Exit criteria

- `tools/release/extract-tag-claims.sh` exists, is executable, and its test passes.
- [`.github/workflows/ci.yml`](../../.github/workflows/ci.yml) contains the `tag-claim-check` job; it runs on PRs and pushes to `main`; it does **not** run on tag pushes.
- [`.github/workflows/release.yml`](../../.github/workflows/release.yml) exists; triggers only on `vX.Y.Z` tag push (no RC tags); produces the four artifact families (per-os/arch tarballs, docker image tar, SHA256SUMS, signature).
- [`docs/contributing/release-process.md`](../../docs/contributing/release-process.md) describes the release-vs-RC distinction.
- Guard self-test from Step 5 passes against `v0.1.0` and `v0.2.0`.
- `make ci` exits 0.
- Reviewer notes contain the deferred [README.md](../../README.md) cross-link edit for [21](21-phase3-cleanup-gate.md).

## Tests

- Script test: `tools/release/tests/extract-tag-claims_test.sh` (or equivalent) verifies the script emits expected tags from a fixture.
- Self-test: the guard succeeds against the existing remote tags `v0.1.0` and `v0.2.0`.
- The full test of `release.yml` is the actual `v0.3.0` tag push at [21](21-phase3-cleanup-gate.md). If the workflow fails there, [21](21-phase3-cleanup-gate.md) blocks the close.

## Risks

| Risk | Mitigation |
|---|---|
| The guard's regex matches a tag-shaped string that is not actually a tag claim (false positive) | Tighten the regex in `extract-tag-claims.sh`. Add a fixture test for the false-positive case. The script's role is precision, not recall — false negatives (a missed tag claim) are caught by the [21](21-phase3-cleanup-gate.md) close-out gate. |
| `cosign` keyless signing is unavailable in the project's CI account (e.g. OIDC not configured for the repo's GitHub org) | Fall back to `cosign sign-blob` with `RELEASE_SIGNING_KEY` from secrets. Document the choice in `docs/contributing/release-process.md`. |
| The release workflow runs on a tag push but signing fails because the secret is missing | The workflow surfaces the failure clearly; the tag remains on remote but the release is incomplete. Operator manually re-runs the workflow once the secret is configured. Document the recovery path. |
| Producing a docker image tar at release time is too slow for the workflow's time budget | The tar is the slowest job (`docker save` on a multi-arch image). Run it in parallel with `build`. If still too slow, accept a 15-minute total release-workflow runtime — releases are infrequent. |
| The CHANGELOG.md release-notes extraction in Step 3 picks up the wrong section because of formatting drift | Test with the existing `v0.2.0` section; if the parser fails, fix the parser, not the CHANGELOG formatting. |
| A workstream that lands after this one introduces a new doc with a tag claim and forgets the guard exists | The guard runs on every PR; the offending PR fails CI. That's the intended catch. |
