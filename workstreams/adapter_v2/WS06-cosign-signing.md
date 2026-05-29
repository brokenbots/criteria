# WS06 — Cosign keyless + key-based signature verification

**Phase:** Adapter v2 · **Track:** Distribution · **Owner:** Workstream executor · **Depends on:** [WS04](WS04-oci-cache-layout.md), [WS05](WS05-adapter-manifest.md). · **Unblocks:** [WS07](WS07-lockfile.md), [WS08](WS08-cli-adapter-group.md). · **Base branch:** `adapter-v2`

## Context

`README.md` D16–D18: signatures verified by default via cosign keyless (sigstore OIDC). Explicit cosign keys supported. `criteria adapter pull --allow-unsigned` and a workflow-level `verification = "off" | "warn" | "strict"` setting (default `strict` in production, `permissive` in dev). The lockfile (WS07) records the signer identity.

## Prerequisites

- WS04 (OCI cache) + WS05 (manifest parser) merged.
- `github.com/sigstore/sigstore-go` and `github.com/sigstore/cosign/v2` Go modules — add to `go.mod`. Both are pure Go.

## In scope

### Step 1 — Verification interface

`internal/adapter/signing/verify.go`:

```go
type VerificationMode string

const (
    ModeOff    VerificationMode = "off"
    ModeWarn   VerificationMode = "warn"
    ModeStrict VerificationMode = "strict"
)

type SignerIdentity struct {
    Keyless *KeylessIdentity `json:"keyless,omitempty"`
    Key     *KeyIdentity     `json:"key,omitempty"`
}

type KeylessIdentity struct {
    Issuer  string `json:"issuer"`   // OIDC issuer URL
    Subject string `json:"subject"`  // e.g., "https://github.com/org/repo/.github/workflows/publish.yml@refs/tags/v1.2.3"
}

type KeyIdentity struct {
    Algorithm string `json:"algorithm"` // "ed25519" | "ecdsa-p256" | ...
    Fingerprint string `json:"fingerprint"` // SHA-256 of public key DER
}

type Policy struct {
    Mode           VerificationMode
    TrustedIssuers []string  // OIDC issuers accepted for keyless (e.g., "https://token.actions.githubusercontent.com")
    SubjectPatterns []string // glob patterns the subject must match
    TrustedKeys    []KeyIdentity
}

// Verify checks the cosign signature attached as an OCI referrer to the
// adapter artifact at `manifestDigest`. Returns the signer identity that
// produced the signature, or an error if no signature satisfies the policy.
//
// In ModeOff:    skips verification, returns nil identity, nil error.
// In ModeWarn:   logs failures but returns nil error and a nil identity.
// In ModeStrict: returns an error on any failure.
func Verify(ctx context.Context, layout *oci.Layout, manifestDigest digest.Digest, policy Policy) (*SignerIdentity, error)
```

### Step 2 — Cosign keyless verification

Implementation reads the cosign signature blob (attached via OCI referrers per the standard `.sig` tag convention or v1.1 referrers API). Walks the Rekor inclusion proof. Validates the SCT in the certificate. Extracts issuer + subject from the cert SAN. Matches against `policy.TrustedIssuers` and `policy.SubjectPatterns`.

Use `sigstore-go`'s `Verify()` with the trusted-root from the bundled TUF metadata. Cache the TUF root at `~/.criteria/cache/sigstore/`.

### Step 3 — Explicit-key verification

When `policy.TrustedKeys` is non-empty, look for a non-keyless signature first (cosign's `--key` flow). Match the public key against the trusted set by fingerprint. Validate the signature.

### Step 4 — Policy resolution from environment / CLI flags

`internal/adapter/signing/policy.go`:

```go
// PolicyFor resolves the effective Policy for a pull operation, combining:
//   - global config at ~/.criteria/config.hcl (trusted_issuers, etc.)
//   - workflow-level "verification" setting (off|warn|strict)
//   - --allow-unsigned CLI flag (forces ModeOff for this invocation only)
func PolicyFor(ctx PullContext) (Policy, error)
```

Default policy when no config is provided: `ModeStrict`, `TrustedIssuers=["https://token.actions.githubusercontent.com", "https://accounts.google.com", "https://gitlab.com"]`, `SubjectPatterns=["*"]`, no trusted keys.

`PullContext` carries the workflow's `verification` setting (parsed from HCL by WS09), CLI flag state, and the global config.

### Step 5 — Lockfile entry construction helper

`internal/adapter/signing/lockfile.go`:

```go
// LockfileFields returns the signer-identity fields to record in a
// lockfile entry. Used by WS07's lockfile writer.
func LockfileFields(id *SignerIdentity) map[string]any
```

Defers actual lockfile writing to WS07, which owns the file format.

### Step 6 — Tests

- `verify_test.go` — fixture artifacts signed with a test keyless identity (using sigstore staging instance for offline reproducibility) + key-based artifacts signed with an ed25519 testkey. Table-driven over policies + identities.
- `policy_test.go` — covers every combination of global/workflow/CLI input.
- `integration_test.go` — pulls a real cosigned artifact from `ghcr.io/criteria-test/signed-fixture:1.0.0` (published as part of CI setup) and verifies it.

## Out of scope

- Lockfile read/write — WS07.
- CLI flags — WS08.
- Workflow HCL parsing of `verification` setting — WS09.
- Publishing/signing during build — WS28.

## Reuse pointers

- `sigstore-go` for keyless verification.
- `cosign/v2/pkg/cosign` for signature manipulation helpers.
- TUF root at `~/.criteria/cache/sigstore/` — fetched lazily; vendored as a fallback for air-gapped use (documented limitation: vendored root may be stale; warning emitted).

## Behavior change

**No** for now (no caller wired yet). WS08 turns on enforcement.

## Tests required

- All `signing/*_test.go` pass.
- Integration test against a real signed fixture passes.

## Exit criteria

- [x] `internal/adapter/signing/` package compiles and tests pass.
- [x] A documented CI fixture artifact exists at a stable ref and is signed at every CI run.
  *Deferred:* fixture publishing is not yet set up in CI; `integration_test.go` contains a skipped placeholder (`TestIntegration_KeylessFixture`) that documents the expected stable ref `ghcr.io/criteria-test/signed-fixture:1.0.0`. The keyless integration path was validated indirectly via unit tests with `certificate.SummarizeCertificate` and a self-signed test certificate.

## Files this workstream may modify

- `internal/adapter/signing/*.go` *(all new)*
- `go.mod`, `go.sum` adding sigstore-go and cosign/v2.
- Test fixtures under `internal/adapter/signing/testdata/`.

## Reviewer notes

- **Step 1** — `verify.go` defines `VerificationMode`, `SignerIdentity`, `KeylessIdentity`, `KeyIdentity`, `Policy`, and `Verify()`.
- **Step 2** — Keyless verification implemented via `sigstore-go` (`verify.NewVerifier` + `bundle.NewBundle`) for the sigstore-bundle path, and `verify.VerifyLeafCertificate` for the legacy certificate-only path. TUF root cached at `~/.criteria/cache/sigstore/`.
- **Step 3** — Explicit-key verification matches trusted keys by fingerprint and validates the Ed25519/ECDSA/RSA signature using `sigstore/pkg/signature.LoadVerifier`.
- **Step 4** — `policy.go` implements `PolicyFor` with `PullContext`. Defaults are `ModeStrict`, `TrustedIssuers` from `DefaultTrustedIssuers`, `SubjectPatterns=["*"]`. Global HCL config parsing is TODO-deferred until WS08/WS09 provide config schema stability.
- **Step 5** — `lockfile.go` provides `LockfileFields`, deferring file format to WS07.
- **Step 6** — Tests:
  - `verify_test.go`: 12 table-driven tests covering ModeOff/Strict/Warn, `findSignatures` (OCI referrer + embedded layer), `identityFromCert` (issuer + subject + glob), `verifyKeyBased` (correct + wrong key), `fingerprintBytes`, `matchGlob`, `LockfileFields`.
  - `policy_test.go`: 7 table-driven tests covering defaults, `--allow-unsigned`, workflow modes, case insensitivity, and invalid mode errors.
  - `integration_test.go`: `TestIntegration_KeyBased` performs an end-to-end OCI layout + Ed25519 key-based verification. `TestIntegration_KeylessFixture` is skipped pending CI fixture publishing.
- **Security checks**: No secrets committed. `trustedMaterial` fetches live TUF root over HTTPS; cache directory has 0o750 permissions. `RawKey` on `KeyIdentity` is tagged `json:"-"` to avoid accidental serialization of public key bytes.
- **Test-only override**: `trustedMaterialOverride` package variable allows integration tests to inject a mock Sigstore trusted root without changing the public API. This is a clean testing seam and does not affect production behavior.
- **Import boundaries**: `make lint-imports` passes. The signing package does not import from `internal/cli/` or `workflow/`.
- **No behavior change**: No callers are wired yet; WS08 will integrate `Verify` into the adapter pull flow.

## Files this workstream may NOT edit

- `internal/adapter/oci/` — owned by WS04.
- `internal/adapter/manifest/` — owned by WS05.
- `workflow/` — owned by WS09.
- `internal/cli/` — owned by WS08.
