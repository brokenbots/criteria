# WS07 — `.criteria.lock.hcl` format and helpers

**Phase:** Adapter v2 · **Track:** Distribution · **Owner:** Workstream executor · **Depends on:** [WS04](WS04-oci-cache-layout.md), [WS05](WS05-adapter-manifest.md), [WS06](WS06-cosign-signing.md). · **Unblocks:** [WS08](WS08-cli-adapter-group.md), [WS09](WS09-environment-block-and-secret-taint.md), [WS20](WS20-remote-environment-and-shim.md).

## Context

`README.md` D5, D7: per-workflow `.criteria.lock.hcl` records, for each referenced adapter: full OCI ref, resolved digest, signer identity, SDK protocol version, source URL, and any remote-endpoint pin (from WS20). Committed to VCS. Updated by `criteria adapter pull` and `criteria adapter lock`. Compile auto-pulls based on lockfile.

## Prerequisites

- WS04 (oci.Reference parser), WS05 (manifest types), WS06 (signing.SignerIdentity types) merged.

## In scope

### Step 1 — Lockfile grammar

`workflow/lockfile/schema.go`:

```hcl
# Example .criteria.lock.hcl

schema_version = 1

adapter "claude" "default" {
  reference          = "ghcr.io/criteria-adapters/claude:1.2.3"
  resolved_digest    = "sha256:abc123..."
  source_url         = "https://github.com/criteria-adapters/claude"
  sdk_protocol_version = 2
  platforms = ["linux/amd64", "linux/arm64", "darwin/arm64"]

  signature {
    keyless {
      issuer  = "https://token.actions.githubusercontent.com"
      subject = "https://github.com/criteria-adapters/claude/.github/workflows/publish.yml@refs/tags/v1.2.3"
    }
  }

  container_image {           # present only when D12 image mode is published
    ref    = "ghcr.io/criteria-adapters/claude:1.2.3-image"
    digest = "sha256:def456..."
  }
}

adapter "copilot" "default" {
  reference          = "ghcr.io/criteria-adapters/copilot:0.5.0"
  resolved_digest    = "sha256:..."
  source_url         = "https://github.com/criteria-adapters/copilot"
  sdk_protocol_version = 2
  platforms          = ["linux/amd64"]

  signature {
    key {
      algorithm   = "ed25519"
      fingerprint = "sha256:..."
    }
  }

  # If the workflow uses this adapter under a remote environment (WS20),
  # the lockfile also records the endpoint fingerprint.
  remote {
    listen_address    = "0.0.0.0:7778"
    server_cert_fingerprint = "sha256:..."
  }
}
```

### Step 2 — Go types

`workflow/lockfile/types.go`:

```go
type Lockfile struct {
    SchemaVersion int                       `hcl:"schema_version"`
    Adapters      []LockedAdapter           `hcl:"adapter,block"`
}

type LockedAdapter struct {
    Type               string                  `hcl:",label"`
    Name               string                  `hcl:",label"`
    Reference          string                  `hcl:"reference"`
    ResolvedDigest     string                  `hcl:"resolved_digest"`
    SourceURL          string                  `hcl:"source_url"`
    SDKProtocolVersion int                     `hcl:"sdk_protocol_version"`
    Platforms          []string                `hcl:"platforms"`
    Signature          *LockedSignature        `hcl:"signature,block"`
    ContainerImage     *LockedContainerImage   `hcl:"container_image,block"`
    Remote             *LockedRemote           `hcl:"remote,block"`
}
```

(plus the nested types, all decoded via `gohcl.DecodeBody`).

### Step 3 — Read / write / diff

`workflow/lockfile/io.go`:

```go
func Read(path string) (*Lockfile, error)
func Write(path string, lf *Lockfile) error               // canonical formatting (gocty + HCL printer)
func ReadFromDir(workflowDir string) (*Lockfile, error)   // looks for .criteria.lock.hcl next to workflow files
```

Writing is **canonical**: sorted by `<type>.<name>`, blocks always in the same order, field order consistent. This minimizes diff noise. Use `hclwrite.NewEmptyFile()` and `hclwrite.AppendNewBlock()` builders so the output is reproducible byte-for-byte across runs.

`workflow/lockfile/diff.go`:

```go
type Change struct {
    Adapter string         // "<type>.<name>"
    Kind    ChangeKind     // Added | Removed | DigestChanged | SignerChanged | ...
    Before  any            // previous value where applicable
    After   any
}

func Diff(old, new *Lockfile) []Change
```

Used by `criteria adapter lock` to print "this changed" rather than dumping a full file diff.

### Step 4 — Construction helpers

`workflow/lockfile/build.go`:

```go
// BuildEntry assembles a LockedAdapter from a successful pull. Inputs:
//   - the parsed Reference,
//   - the resolved digest from the registry,
//   - the parsed Manifest from adapter.yaml,
//   - the verified SignerIdentity (or nil if unsigned and policy allows it).
func BuildEntry(ref oci.Reference, dg digest.Digest, m *manifest.Manifest, signer *signing.SignerIdentity, remote *RemoteFields) (LockedAdapter, error)
```

`RemoteFields` is populated by WS20 when an adapter is bound to a `remote` environment.

### Step 5 — Validation against workflow

`workflow/lockfile/validate.go`:

```go
// ValidateAgainstWorkflow ensures every adapter referenced by the parsed
// workflow has a matching lockfile entry; every lockfile entry refers to
// an adapter still referenced by the workflow.
//
// Returns:
//   - missing: adapters referenced by workflow but not in lockfile (compile
//     hint: "run `criteria adapter lock`")
//   - stale:   adapters in lockfile but not referenced (lock command will
//     prune these next run)
func ValidateAgainstWorkflow(lf *Lockfile, graph *workflow.FSMGraph) (missing, stale []string)
```

### Step 6 — Tests

- `io_test.go` — round-trip canonical write/read; byte-identical for stable inputs.
- `diff_test.go` — table-driven over change kinds.
- `build_test.go` — every field flows from inputs to output.
- `validate_test.go` — missing/stale detection.
- Fixture lockfiles for several adapters + remote case + container-image case.

## Out of scope

- Pulling — WS04.
- Signing/verifying — WS06.
- The `criteria adapter lock` / `criteria adapter pull` verbs — WS08.
- Compile-time auto-pull integration — WS08 / WS09.
- Remote endpoint resolution — WS20 (passes its data through `BuildEntry`).

## Reuse pointers

- HashiCorp `hcl/v2` and `hclwrite` for grammar + canonical output.
- `digest.Digest` from `image-spec` (already in WS04's deps).

## Behavior change

**No.** Adds a package; no caller yet.

## Tests required

- All `workflow/lockfile/*_test.go` pass.
- Round-trip byte-stability tests.
- `make ci` green.

## Exit criteria

- `workflow/lockfile/` package compiles and tests pass.
- Canonical formatting is byte-stable across runs.

## Files this workstream may modify

- `workflow/lockfile/*.go` *(all new)*
- `workflow/lockfile/testdata/*.hcl` *(new fixtures)*

## Files this workstream may NOT edit

- `workflow/schema.go`, `workflow/compile*.go` — touched by WS09.
- `internal/cli/` — owned by WS08.
- `internal/adapter/oci/`, `manifest/`, `signing/` — owned by WS04/WS05/WS06.
