# WS05 — `adapter.yaml` manifest format + runtime verification

**Phase:** Adapter v2 · **Track:** Distribution · **Owner:** Workstream executor · **Depends on:** [WS02](WS02-protocol-v2-proto.md), [WS04](WS04-oci-cache-layout.md). · **Unblocks:** [WS06](WS06-cosign-signing.md), [WS07](WS07-lockfile.md), [WS08](WS08-cli-adapter-group.md), [WS28](WS28-reusable-publish-action.md).

## Context

Per `README.md` D13–D15:

- Adapter metadata is **declared via the SDK `serve()` config** in the adapter source code (single source of truth for developers).
- The build step emits `adapter.yaml` by running the binary with `--emit-manifest`.
- The host reads `adapter.yaml` from the OCI artifact at pull time so it can validate without launching the binary.
- At first run, the host calls `Info()` and verifies the runtime response matches the static manifest — divergence aborts the run.

This workstream defines the YAML schema, the Go types, the parser, the validator, and the runtime cross-check. The actual `--emit-manifest` flag in each SDK is WS23–WS25.

## Prerequisites

- WS02 merged (the v2 `InfoResponse` shape is the authoritative source for what the manifest carries).
- WS04 merged (`internal/adapter/oci` provides the `fs.FS` opener that gives access to `adapter.yaml` inside an artifact).

## In scope

### Step 1 — Define `adapter.yaml` schema

Author `internal/adapter/manifest/schema.go`:

```go
type Manifest struct {
    SchemaVersion          int                `yaml:"schema_version"`              // = 1 for v2
    Name                   string             `yaml:"name"`
    Version                string             `yaml:"version"`                     // semver
    Description            string             `yaml:"description"`
    SourceURL              string             `yaml:"source_url"`                  // REQUIRED, see D13
    Capabilities           []string           `yaml:"capabilities"`
    Platforms              []Platform         `yaml:"platforms"`                   // GOOS/GOARCH list
    SDKProtocolVersion     int                `yaml:"sdk_protocol_version"`        // protocol v2 → 2
    ConfigSchema           Schema             `yaml:"config_schema"`
    InputSchema            Schema             `yaml:"input_schema"`
    OutputSchema           Schema             `yaml:"output_schema"`
    Secrets                []SecretDecl       `yaml:"secrets"`
    Permissions            []string           `yaml:"permissions"`
    CompatibleEnvironments []string           `yaml:"compatible_environments"`     // optional; default any (see D36)
    ContainerImage         *ContainerImageRef `yaml:"container_image,omitempty"`   // set when WS28 publishes with_image=true
}

type Platform struct { OS string `yaml:"os"`; Arch string `yaml:"arch"` }

type SecretDecl struct {
    Name        string `yaml:"name"`
    Description string `yaml:"description"`
    Required    bool   `yaml:"required"`
}

type ContainerImageRef struct {
    Ref    string `yaml:"ref"`     // ghcr.io/org/name:v1.2.3-image
    Digest string `yaml:"digest"`  // sha256:...
}

type Schema struct {
    Fields map[string]SchemaField `yaml:"fields"`
}

type SchemaField struct {
    Type        string `yaml:"type"`         // "string" | "number" | "boolean" | "object" | "array"
    Required    bool   `yaml:"required"`
    Description string `yaml:"description"`
    Default     any    `yaml:"default,omitempty"`
    Sensitive   bool   `yaml:"sensitive,omitempty"`  // marks output fields as taint sources (D63)
}
```

`source_url` is required (see `README.md` D13 — error messages quote it verbatim).

### Step 2 — Parser + validator

`internal/adapter/manifest/parse.go`:

```go
func Parse(reader io.Reader) (*Manifest, error)
func ParseFile(path string) (*Manifest, error)
func ParseFromFS(fsys fs.FS, name string) (*Manifest, error)  // typical: ParseFromFS(ociFS, "adapter.yaml")
```

`internal/adapter/manifest/validate.go`:

```go
func (m *Manifest) Validate() error
```

Validation rules:
- `schema_version >= 1 && schema_version <= ManifestMaxSchemaVersion` (host build constant; currently `1`). Forward-compat: a v2.1 host bumps the constant to `2` and accepts both. **Never use strict equality** — that turns every future field addition into a breaking change for hosts that haven't upgraded.
- `name` matches `^[a-z][a-z0-9-]*$`.
- `version` is valid semver per `golang.org/x/mod/semver`.
- `source_url` is a parseable URL with at least a scheme of `^[a-z][a-z0-9+.-]{1,}$` (RFC 3986). Allows `https`, `http`, `git`, `git+ssh`, on-prem schemes. The host does not fetch the URL; it only quotes it back in error messages (D13), so loose scheme acceptance is safe.
- `platforms` non-empty; each `(os, arch)` matches `^[a-z][a-z0-9]*$/ ^[a-z0-9_]+$` (open-ended `goos/goarch` tokens). Validation accepts any well-formed pair — including `linux/riscv64`, future Go arches, etc. The decision "can I run this on *this* host" is the per-host platform-mismatch check (D12c-alt), not the manifest validator. **Closing the platform set here would defeat the decentralized-publishing goal** (S1.2) — an adapter author shouldn't need a criteria release to publish a new arch.
- `sdk_protocol_version >= 2 && sdk_protocol_version <= ProtocolMaxSDKVersion` (host build constant; currently `2`). Same range/bump rule as `schema_version`.
- Every `SchemaField.Type` is one of the documented values (`string`, `number`, `boolean`, `object`, `array`). Unknown types pass through as a warning rather than an error so adapters can experiment with new types before they're standardised — but only with a `--manifest-allow-unknown-types` flag set, default off.
- `compatible_environments` entries match `^[a-z][a-z_]*$` or are `"*"`. Empty list is treated as `["*"]` (default = any per D36); `["*"]` is the canonical-explicit form.
- `container_image.digest` (if set) parses as a valid OCI digest.

Each failing rule returns an error that names the field and the offending value.

### Step 3 — OCI annotation mirror

`internal/adapter/manifest/annotations.go`: defines the OCI annotation keys used so consumers (the host's pull path, the CLI's `info` verb in WS08) can read top-level fields without parsing the YAML blob.

**Namespace decision (D87):** annotations use `dev.criteria.adapter.*`, not `com.brokenbots.criteria.adapter.*`. Project-name-based namespacing is durable across any future org or trademark change — the published artifacts will outlive the GitHub home. Matches the `org.opencontainers.image.*` convention.

```go
const (
    AnnotationName         = "dev.criteria.adapter.name"
    AnnotationVersion      = "dev.criteria.adapter.version"
    AnnotationSourceURL    = "dev.criteria.adapter.source_url"
    AnnotationCapabilities = "dev.criteria.adapter.capabilities"      // comma-joined
    AnnotationPlatforms    = "dev.criteria.adapter.platforms"          // comma-joined GOOS/GOARCH pairs
    AnnotationProtoVer     = "dev.criteria.adapter.protocol_version"
    AnnotationSchemaVer    = "dev.criteria.adapter.schema_version"     // manifest schema_version
    AnnotationSigner       = "dev.criteria.adapter.signer"             // cosign identity (issuer|subject or key fingerprint) — set by WS28 publish action so `adapter list --show-signer` works without referrer deref
)
```

`AnnotationMap(m *Manifest) map[string]string` produces the map for the publish action.

### Step 4 — Runtime cross-check

`internal/adapter/manifest/verify.go`:

```go
// Verify compares the static manifest from adapter.yaml to the runtime
// Info() response. Divergence in any of these fields is fatal:
//
//   - name
//   - version
//   - sdk_protocol_version
//   - capabilities (set equality)
//   - platforms (set equality)
//   - config_schema, input_schema, output_schema (structural equality, see below)
//   - declared secrets (set of names)
//   - compatible_environments (set equality; absent and ["*"] normalised to "any")
//
// Other fields (description, source_url, permissions) are allowed to differ
// at runtime: they're advisory or human-facing.
func Verify(static *Manifest, runtime *v2.InfoResponse) error
```

**Structural equality of schemas (S3.5).** Two schemas are equal iff they have the same set of field names, and for every name the `(type, required, sensitive)` triple is equal. `description` and `default` are **explicitly ignored** — runtime SDKs commonly elide defaults during marshalling, and human-facing descriptions may carry templated values. Comparison iterates fields in sorted name order; the function returns the first divergence found with both sides quoted in the error message.

**Set equality** is defined as: convert both sides to a sorted unique slice, then `slices.Equal`. Order-insensitive, duplicate-insensitive.

Returns a structured error with each diverging field enumerated, so the host can surface a clear message to the user (e.g., *"adapter `claude` declares version `1.2.3` in adapter.yaml but reports `1.2.2` at runtime; refusing to load"*).

### Step 5 — Tests

- `parse_test.go` — round-trip every field; round-trip with `omitempty` fields absent.
- `validate_test.go` — table-driven, every failure rule has its own row.
- `annotations_test.go` — round-trip annotation map → manifest top-level fields.
- `verify_test.go` — every divergent field produces an error; identical manifests verify successfully.

### Step 6 — Reference fixture

`internal/adapter/manifest/testdata/adapter.yaml` — the canonical example used by other workstreams' tests (and quoted in `docs/adapters.md` written by WS39).

## Out of scope

- The `--emit-manifest` flag implementation in each SDK — WS23–WS25.
- The publish action that writes `adapter.yaml` into the OCI artifact — WS28.
- The pull path that reads `adapter.yaml` from the cache and calls `Verify(...)` — WS08.

## Reuse pointers

- `gopkg.in/yaml.v3` (or `sigs.k8s.io/yaml` for JSON-equivalent strictness).
- `golang.org/x/mod/semver` for version validation.
- `internal/adapter/oci/open.go` (WS04) for `fs.FS` access to the manifest blob.

## Behavior change

**No.** Adds files; nothing else reads them yet.

## Tests required

- All `manifest/*_test.go` pass.
- `make ci` green.

## Exit criteria

- `internal/adapter/manifest/` package compiles and tests pass.
- Reference fixture validates against the parser.

## Files this workstream may modify

- `internal/adapter/manifest/*.go` *(all new)*
- `internal/adapter/manifest/testdata/*.yaml` *(new)*

## Files this workstream may NOT edit

- `internal/adapter/oci/` — owned by WS04.
- `internal/adapter/discovery.go`, `loader.go`, `sessions.go` — touched by WS08 and WS06.
- `internal/cli/` — touched by WS08.
- The SDK repos — WS23–WS25.
- `README.md`, `PLAN.md`, `AGENTS.md`, `CHANGELOG.md`, `CONTRIBUTING.md`, `workstreams/README.md`.
