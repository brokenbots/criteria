# Release process

Criteria's adapter-protocol-v2 release is guarded by **four verification gates**.
All four are self-contained — they depend only on this repository,
with no reach-out to external adapter repos or a CI-owned registry org. Per-adapter
end-to-end coverage (real keyless publishing, language-specific conformance) lives
in each adapter's / SDK's own repo, not here.

| Gate | What it checks | Where it runs |
|------|----------------|---------------|
| **Gate 1** — conformance | Host ⇆ imported Go SDK + proto compatibility (per [ADR-0003](adrs/ADR-0003-conformance-scope.md)): `TestNoopAdapterConformance` (subprocess) + the in-memory SDK suite. | `release.yml` `gate-conformance` (also `ci.yml` `unit-tests` + `proto-drift` on every push/PR) |
| **Gate 2** — in-tree adapters | Builds the in-tree `mcp` adapter and the `noop` conformance fixture, then validates and runs the example workflows end-to-end. | `release.yml` `gate-e2e` (also `ci.yml` `e2e` on every push/PR) |
| **Gate 3** — remote transport e2e | Spins up a remote fixture adapter that phones home over mTLS, runs a representative workflow, and exercises crash-policy recovery. | `release.yml` `gate-remote` → reuses [`remote-e2e.yml`](../.github/workflows/remote-e2e.yml) |
| **Gate 4** — publishing flow | Publishes the in-tree `noop` adapter to an ephemeral local OCI registry via `criteria adapter publish`, then pulls it back and verifies the manifest / `Info()` round-trip. | `release.yml` `gate-publish` |

## The release is gated on the gates

On a release or pre-release tag, [`release.yml`](../.github/workflows/release.yml)
runs **all four gates first**; the `build`, `docker-image`, `checksum-and-sign`,
and `release` (publish) jobs `needs:` every gate. **If any gate fails, the build
and publish jobs are skipped — a release can never be published when a gate is
red.** This is the single source of truth on tags; there is no separate
release-gates workflow.

Gates 1 & 2 also run on every push/PR via `ci.yml` for fast feedback; the heavier
Gates 3 & 4 run only on a tag (inside `release.yml`) and on demand
(`workflow_dispatch` / weekly schedule for `remote-e2e.yml`).

To validate the heavy gates on a branch before tagging, dispatch the remote gate
directly, or simply cut a pre-release tag (which runs the full gated pipeline):

```sh
gh workflow run remote-e2e.yml --ref <branch>   # Gate 3 only
# or cut a pre-release to exercise all four gates + the gated publish:
git tag -a vX.Y.Z-rc1 -m "rc" && git push origin vX.Y.Z-rc1
```

## Gate 3 — remote transport end-to-end

Gate 3 reuses the remote smoke ([`remote-e2e.yml`](../.github/workflows/remote-e2e.yml)),
which builds the in-tree remote fixture adapter (`GOWORK=off`, since it is a nested
module under `testdata/`), dockerizes it, and runs `go test ./internal/ci/smoke/...`
with `CRITERIA_REMOTE_E2E=1`. `release.yml` invokes it as the `gate-remote` job.

## Gate 4 — publishing flow (self-contained)

Gate 4 proves the publish → pull contract end-to-end without any external
dependency:

1. Build the criteria CLI and the in-tree `noop` adapter.
2. Stand up an ephemeral `registry:2` service inside the job.
3. `criteria adapter publish bin/criteria-adapter-noop --registry localhost:5000/noop:v1.0.0`.
   Localhost registries default to plain HTTP (`oci.IsLocalhost`), so no TLS/auth
   setup is needed. The artifact is unsigned — signing has dedicated unit coverage
   ([`internal/adapter/publish/sign_test.go`](../internal/adapter/publish/sign_test.go))
   and the keyless path is proven in the adapter repos.
4. `criteria adapter pull --allow-unsigned localhost:5000/noop:v1.0.0`, then
   `criteria adapter info noop` and `criteria adapter where noop` to confirm the
   manifest round-trips and the platform binary resolves.

The **real keyless → GHCR publish** (cosign-keyless via job OIDC, multi-arch,
signature verification at pull) is validated in each adapter repo's own
`publish.yml` — that is where a genuine GitHub Actions identity and the production
registry namespace exist. Keeping that out of the criteria repo's CI is deliberate:
the host repo depends only on itself.

## Tagging the release

Once out-of-band manual testing has signed off, tag the release. **"v2" is the
adapter _protocol_ version, not the product version** — this release is tagged on
the `0.5.0` line:

```sh
git tag -s v0.5.0 -m "Criteria v0.5.0 (adapter protocol v2)"
git push origin v0.5.0
```

The signed tag triggers `release.yml`, which runs the four gates and — only if
they all pass — builds, signs, and publishes the release binaries. Full releases
additionally update the `brokenbots/homebrew-criteria` tap (see below). The
release-source guard additionally requires a full-release tag to point at a
commit on `main`. Generate the GitHub Release notes from the `CHANGELOG.md`
v0.5.0 section.

## Homebrew tap update

Full releases (`vX.Y.Z`, tags without `-rcN` or `-betaN`) automatically update
the `brokenbots/homebrew-criteria` tap. This is performed by the
`update-homebrew-tap` job in `.github/workflows/release.yml`, which runs after
the GitHub Release is published.

The job:

1. Installs cosign.
2. Checks out `brokenbots/homebrew-criteria` using a repository-scoped token.
3. Runs `.github/scripts/update-homebrew-tap.py` with the release tag.
4. Verifies the release's `SHA256SUMS` with the `SHA256SUMS.bundle` signature
   against the GitHub Actions identity before reading any checksums.
5. Writes `Formula/criteria.rb` using only hashes from the signed manifest.
6. Commits and pushes the formula to the tap repository.

### Required GitHub App

The job authenticates to the tap with a short-lived installation token minted at
run time by
[`actions/create-github-app-token@bcd2ba49218906704ab6c1aa796996da409d3eb1`](https://github.com/actions/create-github-app-token).
No personal access token is used.

Store the App credentials in `brokenbots/criteria`:

- Repository variable `HOMEBREW_TAP_APP_CLIENT_ID` — the App's Client ID.
- Repository secret `HOMEBREW_TAP_APP_PRIVATE_KEY` — the App's private key PEM.

The App needs **only** `contents: write` on `brokenbots/homebrew-criteria`.

#### Manual setup steps

1. In the `brokenbots` organization, create a GitHub App named
   `brokenbots-criteria-tap-writer`.
2. Under **Permissions > Repository permissions**, set **Contents** to
   **Read and write**. Leave all other permissions at **No access**.
3. Install the App on **only** `brokenbots/homebrew-criteria`.
4. Copy the App's **Client ID** and save it as the repository variable
   `HOMEBREW_TAP_APP_CLIENT_ID` in `brokenbots/criteria`.
5. Generate a private key for the App, download the `.pem` file, and save its
   contents as the repository secret `HOMEBREW_TAP_APP_PRIVATE_KEY` in
   `brokenbots/criteria`.

Until the App is created and both credentials are stored, the
`update-homebrew-tap` job will fail on the next full release. This visible
failure is intentional — it signals that the App setup is incomplete, and it
prevents the release from silently falling back to a less secure authentication
path.

### Full-release-only behavior

`update-homebrew-tap` is skipped for pre-release tags (`-rcN`, `-betaN`). The tap
should only advertise stable releases, so the job's `if:` condition excludes any
tag containing a hyphen.

### Failure handling

The job is **not** marked `continue-on-error`. If the cosign verification fails,
a required platform tarball hash is missing, or the push to the tap repository
fails, the entire release workflow is red.

## One-line installer

The file [`install.sh`](../install.sh) is served from the default branch at
`https://raw.githubusercontent.com/brokenbots/criteria/main/install.sh`. It goes
live as soon as a change lands on `main`, so the release artifact contract it
relies on must be preserved:

- Per-platform tarballs named `criteria-<tag>-<os>-<arch>.tar.gz`.
- A top-level `SHA256SUMS` file.
- A Sigstore bundle file `SHA256SUMS.bundle`, produced by keyless signing via
  GitHub OIDC in `release.yml`.

Changing the tarball layout, the filenames, or the checksum file format is a
breaking change for the installer and must be coordinated with an update to
`install.sh`.

### Signing behavior

From the first release that includes this change, `release.yml` signs
`SHA256SUMS` with `cosign sign-blob --bundle SHA256SUMS.bundle`. The key-based
fallback and detached `SHA256SUMS.sig` / `SHA256SUMS.cert` files are removed. If
keyless signing fails, the `checksum-and-sign` job fails immediately and no
release assets are published. Releases before this change provided `.sig` / `.cert`
and no bundle; those releases are not installable with the current `install.sh`.

## Verifying independence

The proto and the standalone adapters live in their own repos. These audits
confirm the criteria repo carries only host / engine / CLI code:

```sh
# The only in-tree adapter is the mcp bridge; noop is a conformance fixture:
ls -d cmd/criteria-adapter-*/                                        # expect: cmd/criteria-adapter-mcp/ only
# The adapter wire contract is consumed as an external module, not vendored:
grep -rn 'github.com/brokenbots/criteria/proto' --include='*.go' .   # expect: nothing
grep -rn 'criteria-adapter-proto' go.mod                            # expect: a pinned version
```

Note: `proto/criteria/v1/` (generated into `sdk/pb/criteria/v1/`) is the criteria
**server's** own transport API — host code, not an adapter contract — so it is
intentionally in-tree. Only the *adapter* protocol (`criteria-adapter-proto`) is
external.

The clean-machine three-SDK-family full-chain smoke (`criteria pull` of a workflow
whose lockfile references one TypeScript, one Python, and one Go adapter, then
`criteria apply`) is the canonical cross-repo demonstration.
