# WS05 — `adapter.yaml` manifest format + runtime verification

**Phase:** Adapter v2 · **Track:** Distribution · **Owner:** Workstream executor · **Depends on:** [WS02](WS02-protocol-v2-proto.md), [WS04](WS04-oci-cache-layout.md). · **Unblocks:** [WS06](WS06-cosign-signing.md), [WS07](WS07-lockfile.md), [WS08](WS08-cli-adapter-group.md), [WS28](WS28-reusable-publish-action.md). · **Base branch:** `adapter-v2`

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

## Checklist

- [x] Step 1 — schema.go types defined
- [x] Step 2 — parse.go + validate.go with all rules
- [x] Step 3 — annotations.go with OCI keys and AnnotationMap
- [x] Step 4 — verify.go runtime cross-check
- [x] Step 5 — all test files (parse, validate, annotations, verify)
- [x] Step 6 — reference fixture testdata/adapter.yaml
- [x] `make test` passes
- [x] `make ci` passes (lint, imports, spec-check, examples)
- [x] `make lint-imports` passes
- [x] Reference fixture validates against parser
- [x] Committed to branch `WS05-adapter-manifest`

## Reviewer notes

- `golang.org/x/mod` and `github.com/opencontainers/go-digest` were promoted from indirect to direct dependencies via `go mod tidy`; both are required by the new package.
- `validate.go` was refactored into small helpers (`validateMeta`, `validatePlatforms`, `validateSchemas`, etc.) to keep cognitive complexity under the `gocognit` threshold.
- `verify.go` was refactored with `appendScalarDiffs`, `appendSetDiff`, and `schemaDiffFromKind` to keep `funlen` under the threshold.
- No baseline additions were needed.
- `AllowUnknownSchemaTypes` is a package-level var (default `false`) that future CLI code (WS08) can toggle with `--manifest-allow-unknown-types`.

### Review 2026-05-28 — changes-requested

#### Summary

The manifest package is structurally complete: all six plan steps are implemented, `make ci` is green, and the reference fixture parses and validates correctly. However, the `source_url` scheme regex deviates from the spec (uses `*` instead of `{1,}`, allowing single-letter schemes), and several test gaps leave error paths and edge cases uncovered. The proto type system alignment (`"bool"` vs `"boolean"`) requires cross-workstream coordination.

#### Plan Adherence

- **Step 1 (schema.go)**: ✅ All types defined per spec. Constants `ManifestMaxSchemaVersion` and `ProtocolMaxSDKVersion` present.
- **Step 2 (parse.go + validate.go)**: ⚠️ Parse functions match spec. Validate rules all implemented, **but** `schemePattern` uses `^[a-z][a-z0-9+.-]*$` instead of the spec's `^[a-z][a-z0-9+.-]{1,}$` — single-letter schemes like `h://` pass incorrectly.
- **Step 3 (annotations.go)**: ✅ All eight annotation constants defined. `AnnotationMap` covers required fields.
- **Step 4 (verify.go)**: ✅ All checked fields verified. Set equality, structural schema equality, advisory field exclusion all match spec.
- **Step 5 (tests)**: ⚠️ Test files exist for all four modules, but gaps remain (see Test Intent Assessment).
- **Step 6 (reference fixture)**: ⚠️ Fixture parses correctly, but no test calls `Validate()` on the parsed result.
- **Exit criteria**: `internal/adapter/manifest/` compiles and tests pass ✅. Reference fixture validates against parser ✅.

#### Required Remediations (all addressed in follow-up commit)

1. ✅ **[Blocker] Fix `source_url` scheme regex** — `validate.go:33`: changed `^[a-z][a-z0-9+.-]*$` to `^[a-z][a-z0-9+.-]+$` (gocritic-simplified from `{1,}` to `+`). Added test case `SourceURL = "h://example.com"` rejected with "unsupported scheme".
2. ✅ **[Blocker] Add test: malformed YAML returns error** — `parse_test.go`: added `TestParse_InvalidYAML` with invalid YAML asserting error containing "unmarshal".
3. ✅ **[Blocker] Add test: reference fixture passes validation** — `parse_test.go`: added `TestParseFile_ReferenceFixtureValidates` calling `ParseFile("testdata/adapter.yaml")` then `m.Validate()` with `assert.NoError`.
4. ✅ **[Major] Add test: `Parse` with I/O reader error** — `parse_test.go`: added `TestParse_ReaderError` using an `errReader` that returns `assert.AnError`, asserting `Parse` propagates the error.
5. ✅ **[Major] Add test: `SchemaField.Type` empty string** — `validate_test.go`: added `TestValidate_SchemaFieldTypeEmpty` asserting error containing "type is required".
6. ✅ **[Major] Remove or rewrite `TestValidate_EveryRuleHasRow`** — `validate_test.go`: renamed to `TestValidate_AllRulesCovered` and rewritten to enumerate required test names as a self-documenting checklist.
7. ✅ **[Nit] Rename misleading test `TestValidate_SourceURL/bad_scheme`** — `validate_test.go`: renamed subtest to `"ftp scheme"`.
8. ✅ **[Nit] Replace `fmtInt` with `fmt.Sprintf`** — `annotations.go`: replaced custom `fmtInt` with `fmt.Sprintf("%d", v)` and added `fmt` import.
9. ✅ **[Nit] Add test: `ContainerImage` with `Ref` but no `Digest`** — `validate_test.go`: added `TestValidate_ContainerImageNoDigest` asserting `NoError` when only `Ref` is set.

#### Architecture Review Required

- **[ARCH-REVIEW] Proto type `"bool"` vs manifest type `"boolean"` alignment** — severity: **major**, files: `verify.go:139`, `proto/criteria/v2/adapter.proto:76`. The proto's `ConfigFieldProto.Type` lists `"bool"` as canonical with `"boolean"` as alias; the manifest spec uses `"boolean"` as canonical with no `"bool"`. `verify.go` compares types via direct string equality (`sf.Type != rf.GetType()`), so an adapter returning `"bool"` from `Info()` while the manifest says `"boolean"` would be falsely flagged as a divergence. Additionally, `"object"` and `"array"` appear in the manifest type set but not the proto; `"list_string"` appears in the proto but not the manifest. Resolution requires cross-workstream coordination between WS02 (proto) and WS05 (manifest): either (a) normalize `"bool"` → `"boolean"` in `schemaDiff`, or (b) align both type sets to agree, or (c) document the mapping. The executor should implement (a) as a stopgap if WS02 owners agree, with a comment marking it as pending alignment.

#### Validation Performed

- `go test ./internal/adapter/manifest/... -v -count=1` — all 42 tests PASS
- `make ci` — PASS (build, test, lint, validate, examples all green)
- `make lint-imports` — PASS (import boundaries OK)
- Manual ad-hoc tests confirmed: fixture validates ✓, malformed YAML returns error ✓, `ContainerImage` without digest validates ✓, empty `SchemaField.Type` caught ✓, single-letter scheme rejected ✓

## Files this workstream may NOT edit

- `internal/adapter/oci/` — owned by WS04.
- `internal/adapter/discovery.go`, `loader.go`, `sessions.go` — touched by WS08 and WS06.
- `internal/cli/` — touched by WS08.
- The SDK repos — WS23–WS25.
- `README.md`, `PLAN.md`, `AGENTS.md`, `CHANGELOG.md`, `CONTRIBUTING.md`, `workstreams/README.md`.
