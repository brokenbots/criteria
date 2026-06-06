# Release process

Criteria's adapter-protocol-v2 release is guarded by **four verification gates**
(README D57). All four are self-contained — they depend only on this repository,
with no reach-out to external adapter repos or a CI-owned registry org. Per-adapter
end-to-end coverage (real keyless publishing, language-specific conformance) lives
in each adapter's / SDK's own repo, not here.

| Gate | What it checks | Where it runs |
|------|----------------|---------------|
| **Gate 1** — conformance | Host ⇆ imported Go SDK + proto compatibility (per [ADR-0003](adrs/ADR-0003-conformance-scope.md)): `TestNoopAdapterConformance` (subprocess), the in-memory SDK suite, and `proto-drift`. | [`ci.yml`](../.github/workflows/ci.yml) `unit-tests` + `proto-drift` jobs (every push/PR) |
| **Gate 2** — in-tree adapters | Builds the in-tree adapters (`noop`, `mcp`) and validates + runs the example workflows end-to-end. | [`ci.yml`](../.github/workflows/ci.yml) `e2e` job (every push/PR) |
| **Gate 3** — remote transport e2e | Spins up a remote fixture adapter that phones home over mTLS, runs a representative workflow, and exercises crash-policy recovery. | [`remote-e2e.yml`](../.github/workflows/remote-e2e.yml), reused by [`release-gates.yml`](../.github/workflows/release-gates.yml) |
| **Gate 4** — publishing flow | Publishes the in-tree `noop` adapter to an ephemeral local OCI registry via `criteria adapter publish`, then pulls it back and verifies the manifest / `Info()` round-trip. | [`release-gates.yml`](../.github/workflows/release-gates.yml) `gate-4-publish-flow` job |

## Running the gates

- **Gates 1 & 2** run automatically on every push and pull request via `ci.yml`.
- **Gates 3 & 4** run on every `v*` tag push and on demand via `workflow_dispatch`
  on [`release-gates.yml`](../.github/workflows/release-gates.yml). The
  `gates-passed` aggregation job requires **both** to succeed.

To validate the tag-only gates on a release-candidate branch before tagging:

```sh
gh workflow run release-gates.yml --ref <branch>
# or just the remote gate on its own:
gh workflow run remote-e2e.yml --ref <branch>
```

## Gate 3 — remote transport end-to-end

Gate 3 reuses the WS22 remote smoke ([`remote-e2e.yml`](../.github/workflows/remote-e2e.yml)),
which builds the in-tree remote fixture adapter, dockerizes it, and runs
`go test ./internal/ci/smoke/...` with `CRITERIA_REMOTE_E2E=1`. It is gated to
tags / weekly schedule / dispatch (not every push) because it is heavier than the
PR suite. **Trigger it via `workflow_dispatch` on the release-candidate branch and
confirm green before tagging.**

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

## Enforcing the gates on a release

Wire `Release gates passed` as a **required status check** on tag/release
protection so a failed gate blocks publishing the release. The
[`release.yml`](../.github/workflows/release.yml) build runs in parallel; the
required check is what prevents promoting a tag whose gates are red.

## Tagging the release (WS40)

Once all four gates are green on the candidate and out-of-band manual testing has
signed off, tag the release. **"v2" is the adapter _protocol_ version, not the
product version** — this release is tagged on the `0.5.0` line:

```sh
git tag -s v0.5.0 -m "Criteria v0.5.0 (adapter protocol v2)"
git push origin v0.5.0
```

The signed tag triggers `release.yml` (binaries, Homebrew tap) and
`release-gates.yml` (Gates 3 & 4). Generate the GitHub Release notes from the
`CHANGELOG.md` v0.5.0 section.

## Verifying independence (WS43)

After the proto and adapters are extracted to their own repos, re-run the
independence audits to confirm the criteria repo carries only host / engine / CLI
code:

```sh
# No in-tree adapter implementations (noop/mcp test fixtures excepted):
find internal/builtin -type d -name '*adapter*' -not -empty   # expect: nothing
# Proto consumed as an external module, not vendored in-tree:
grep -rn 'github.com/brokenbots/criteria/proto' --include='*.go' .   # expect: nothing
```

The clean-machine three-SDK-family full-chain smoke (`criteria pull` of a workflow
whose lockfile references one TypeScript, one Python, and one Go adapter, then
`criteria apply`) is the canonical cross-repo demonstration. See
[WS43](../workstreams/adapter_v2/WS43-independence-verification.md).
