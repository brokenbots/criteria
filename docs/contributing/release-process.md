# Release process — RC artifact upload

## What this is

For every pull request that targets a release candidate, CI builds a
downloadable artifact bundle and attaches it to the workflow run. This
lets reviewers inspect or test the candidate binary without rebuilding
the project locally.

This mechanism covers the **review window** only. The final tagged
release uses a separate release workflow and publishes to the project's
GitHub Releases page.

## How to trigger it

Open a pull request where **at least one** of the following is true:

| Condition | Example branch / title | Artifact name |
|---|---|---|
| Branch name starts with `release/` | `release/v0.3.0-rc1` | `criteria-v0.3.0-rc1` |
| PR title contains `<semver>-rc<N>` | `Release v0.3.0-rc1: ...` | `criteria-v0.3.0-rc1` |
| PR title contains `-rc<N>` (no semver prefix) | `Hotfix -rc2 for storage` | `criteria-rc2` |

The `release-artifacts` CI job is skipped on all other PRs, so
regular feature and fix PRs are unaffected.

**Tag extraction rules (title-based triggers):**

The artifact name is derived from the PR title in two passes:

1. **Semver+RC match** — extracts the first token matching
   `v?X.Y.Z-rcN` (the `-rcN` suffix is **required**; a bare semver
   without it does not match this pass). Example: `Release v0.3.0-rc1`
   → `criteria-v0.3.0-rc1`.
2. **RC-marker fallback** — if no semver+RC token is found, extracts
   the first `-rc<N>` substring and strips the leading `-`. Example:
   `Hotfix -rc2 for storage` → `criteria-rc2`.

A title that triggers the job but matches neither pass (e.g. `Bugfix
foo-rc` with no digit after `-rc`) causes the job to fail loudly with
`ERROR: could not extract RC tag`. This is intentional: the operator
must fix the branch name or title before the artifact can be built.

> **Convention:** avoid PR titles that contain `-rc<N>` for non-release
> work (e.g. `refactor-rc1-...`). If false positives become frequent,
> switch to branch-name-only triggering by removing the title condition
> from `ci.yml`.

## What gets uploaded

| File | Description |
|---|---|
| `criteria` | Main CLI binary (linux/amd64) |
| `criteria-adapter-copilot` | Copilot adapter plugin |
| `criteria-adapter-mcp` | MCP adapter plugin |
| `criteria-adapter-noop` | No-op adapter plugin |
| `criteria-runtime.tar` | Runtime container image (load with `docker load`) |
| `SHA256SUMS` | Checksums for all files above |

## Where to find it

1. Open the PR on GitHub.
2. Click the **Checks** tab and select the `release-artifacts` job.
3. Scroll to the **Artifacts** section at the bottom of the job summary.
4. Download the zip named `criteria-<tag>` (e.g. `criteria-v0.3.0-rc1`).

## Verifying the download

```sh
# Unzip the artifact.
unzip criteria-v0.3.0-rc1.zip -d criteria-v0.3.0-rc1/
cd criteria-v0.3.0-rc1/

# Verify checksums.
sha256sum -c SHA256SUMS

# Load the runtime image.
docker load -i criteria-runtime.tar

# Run the CLI.
chmod +x criteria
./criteria --version
```

## Retention

Artifacts are retained for **30 days** from the workflow run. Download
before that window closes if you need the artifact beyond the review
cycle.

## What this is not

- This does **not** create a GitHub Release or publish to a registry.
- This does **not** sign the binaries (no GPG or sigstore).
- The runtime image is uploaded as a tar for local loading only; it is
  not pushed to any registry from the RC PR.

The final tagged release (post-merge, post-approval) is responsible for
signing, registry publish, and the official GitHub Release entry.
