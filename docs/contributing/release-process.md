# Release process

This document describes how Criteria releases are built, signed, and published,
and how that differs from the RC artifact workflow used during the review window.

## Release vs RC artifact

| Dimension | RC artifact | Release |
|---|---|---|
| **Trigger** | PR with `release/<tag>` branch or `-rc<N>` title | `vX.Y.Z` tag push (no pre-release suffix) |
| **Produced by** | `release-artifacts` job in `ci.yml` | `release.yml` workflow |
| **Destination** | PR Artifacts panel (workflow run) | GitHub Releases page |
| **Signed** | No | Yes — `SHA256SUMS` signed by cosign |
| **Published** | No | Yes |
| **Retention** | 30 days (workflow artifact) | Permanent (GitHub Release) |
| **Spec** | [archived/v2/13-rc-artifact-upload.md](../../workstreams/archived/v2/13-rc-artifact-upload.md) | This document |

### RC artifact

The `release-artifacts` job in [`.github/workflows/ci.yml`](../../.github/workflows/ci.yml)
runs only on pull requests whose branch starts with `release/` or whose title
contains `-rc<N>`. It builds the current Linux/amd64 binaries, packages them
with a runtime image tar and a `SHA256SUMS` file, and uploads them to the
workflow run's Artifacts panel. This is for reviewer inspection during the
review window only. It is **not** signed and **not** published.

### Release

A release is triggered by pushing a tag of the form `vX.Y.Z` (no pre-release
suffix). The `release.yml` workflow runs four sequential jobs:

1. **`build`** — cross-compiles binaries for all four supported platforms and
   packages each as a tarball.
2. **`docker-image`** — builds the runtime image and saves it as a tar.
3. **`checksum-and-sign`** — computes `SHA256SUMS` for all artifacts and signs
   it with cosign.
4. **`release`** — creates the GitHub Release with all artifacts attached and
   release notes pulled from `CHANGELOG.md`.

---

## Supported platforms

Each release produces one tarball per platform:

| Tarball | Contents |
|---|---|
| `criteria-<tag>-linux-amd64.tar.gz` | `criteria` + adapters + `LICENSE` + `README.md` |
| `criteria-<tag>-linux-arm64.tar.gz` | same |
| `criteria-<tag>-darwin-amd64.tar.gz` | same |
| `criteria-<tag>-darwin-arm64.tar.gz` | same |
| `criteria-runtime-<tag>.tar` | Docker runtime image (load with `docker load`) |
| `SHA256SUMS` | SHA256 checksums for all of the above |
| `SHA256SUMS.sig` | cosign signature of `SHA256SUMS` |
| `SHA256SUMS.cert` | cosign signing certificate |

---

## How to trigger a release

```sh
git tag -a v0.3.0 -m "Release v0.3.0"
git push origin v0.3.0
```

The `release.yml` workflow starts automatically. Monitor it at
`https://github.com/brokenbots/criteria/actions`.

> **Important:** the `tag-claim-check` CI job verifies that every tag claimed
> in the tracked docs (`README.md`, `PLAN.md`, `CHANGELOG.md`,
> `workstreams/README.md`, `docs/`) exists on remote before a PR or push to
> `main` is accepted. Push the tag **before** (or as part of) landing changes
> that add the tag to any of these docs.

---

## Verifying a release download

```sh
# Download the tarball and checksum file from the GitHub Releases page.
tar -xzf criteria-v0.3.0-linux-amd64.tar.gz
sha256sum -c SHA256SUMS

# Verify the cosign signature (keyless — no key material needed).
cosign verify-blob \
  --certificate SHA256SUMS.cert \
  --signature SHA256SUMS.sig \
  --certificate-identity-regexp 'https://github.com/brokenbots/criteria/.github/workflows/release.yml' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' \
  SHA256SUMS
```

---

## Signing details

The checksum manifest (`SHA256SUMS`) is signed, not the individual binaries.
This is the modern signing practice and sufficient for supply-chain verification.

**Preferred path — cosign keyless (GitHub OIDC):**
No key material is stored. The `release.yml` workflow uses the GitHub Actions
OIDC token to obtain a short-lived signing certificate from Sigstore's Fulcio
CA. The workflow requires `permissions: id-token: write`. Verification uses the
certificate's Subject Alternative Name (SAN) to confirm the signature came from
this specific workflow path and OIDC issuer.

**Fallback — cosign with a stored key:**
If keyless signing is unavailable (e.g. OIDC not configured for the org), the
workflow falls back to `cosign sign-blob --key` using the `RELEASE_SIGNING_KEY`
repository secret (base64-encoded cosign private key) and
`RELEASE_SIGNING_PASSWORD`. Configure these secrets in
`Settings → Secrets and variables → Actions`.

If neither signing path is available the workflow **does not publish a release**
— it surfaces the failure explicitly. Fix the signing configuration (OIDC
permissions or the `RELEASE_SIGNING_KEY` secret) and re-run the workflow.

---

## Docker image

The release builds `criteria/runtime:<tag>` using `Dockerfile.runtime` and
saves it as `criteria-runtime-<tag>.tar`. It is included as a release asset for
local loading only:

```sh
docker load -i criteria-runtime-v0.3.0.tar
docker run --rm criteria/runtime:v0.3.0 --help
```

Registry publishing (Docker Hub, GHCR, ECR) is a project-level decision not
covered by this workflow; the image is not pushed to any registry during release.

---

## Release notes

Release notes are extracted automatically from `CHANGELOG.md`. The extractor
takes the content between the `## [vX.Y.Z]` heading and the next `##` heading.
If the tag has no matching section, the release body defaults to `Release vX.Y.Z`.

Keep `CHANGELOG.md` updated before tagging. See [CONTRIBUTING.md](../../CONTRIBUTING.md)
for the changelog entry format.

---

## Recovery: re-running a failed release

If the release workflow fails (e.g., signing secret missing, network error):

1. Fix the root cause (configure the secret, etc.).
2. Re-run the failed job from the GitHub Actions UI, or delete and re-push the tag
   if the workflow did not create a GitHub Release yet.
3. If a partial release was published, delete it via `gh release delete <tag>`,
   then re-push the tag.

---

## Tag-claim guard

The `tag-claim-check` job in `ci.yml` runs on every PR and every push to `main`.
It scans `README.md`, `PLAN.md`, `CHANGELOG.md`, `workstreams/README.md`, and
`docs/**/*.md` for version strings that appear alongside a "tag" or "release"
keyword (or as a `## [vX.Y.Z]` CHANGELOG heading) and verifies each claimed tag
exists on the remote. The guard prevents docs from claiming a tag before the tag
is pushed.

The extractor script is at `tools/release/extract-tag-claims.sh`.
Smoke tests are at `tools/release/tests/extract-tag-claims_test.sh`.

---

## Deferred: README.md cross-link

A cross-link from `README.md` to this document and to the RC artifact section
is deferred to [workstreams/phase3/21-phase3-cleanup-gate.md](../../workstreams/phase3/21-phase3-cleanup-gate.md),
which owns the `README.md` coordination set.

