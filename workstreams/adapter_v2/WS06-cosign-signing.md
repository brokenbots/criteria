# WS06 — Cosign keyless + key-based signature verification

**Phase:** Adapter v2 · **Track:** Distribution · **Owner:** Workstream executor · **Depends on:** [WS04](WS04-oci-cache-layout.md), [WS05](WS05-adapter-manifest.md). · **Unblocks:** [WS07](WS07-lockfile.md), [WS08](WS08-cli-adapter-group.md).

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

- `internal/adapter/signing/` package compiles and tests pass.
- A documented CI fixture artifact exists at a stable ref and is signed at every CI run.

## Files this workstream may modify

- `internal/adapter/signing/*.go` *(all new)*
- `go.mod`, `go.sum` adding sigstore-go and cosign/v2.
- Test fixtures under `internal/adapter/signing/testdata/`.

## Files this workstream may NOT edit

- `internal/adapter/oci/` — owned by WS04.
- `internal/adapter/manifest/` — owned by WS05.
- `workflow/` — owned by WS09.
- `internal/cli/` — owned by WS08.
