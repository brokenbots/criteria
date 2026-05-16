# WS04 — OCI cache layout (`oras-go` integration)

**Phase:** Adapter v2 · **Track:** Distribution · **Owner:** Workstream executor · **Depends on:** [WS03](WS03-host-v2-wire.md) (host wire stable). · **Unblocks:** [WS05](WS05-adapter-manifest.md), [WS06](WS06-cosign-signing.md), [WS07](WS07-lockfile.md), [WS08](WS08-cli-adapter-group.md).

## Context

Today adapters live at `$CRITERIA_PLUGINS/criteria-adapter-<name>` or `~/.criteria/plugins/criteria-adapter-<name>` with no version concept and no manifest discovery (see `internal/adapter/discovery.go`).

The Adapter v2 plan (`README.md` D10, D53–D55) replaces this with an **OCI-image-spec-compliant** local cache at `~/.criteria/cache/oci/`. Benefits: `oras` and other OCI tools inspect/manipulate it directly; content-addressing dedupes; ecosystem interop.

This workstream introduces the cache layout, the pull machinery, and resolver/open APIs. Manifest parsing (WS05), signature verification (WS06), lockfile integration (WS07), and CLI verbs (WS08) consume what lands here.

## Prerequisites

- WS03 merged; host code is on v2 types.
- `oras.land/oras-go/v2` available as a Go module dependency. Vet it: it's pure Go, MIT licensed, actively maintained. Add to `go.mod` as part of this WS.
- A throw-away local OCI registry for integration tests (`ghcr.io/oras-project/registry:latest` running on `localhost:5000` — wrapped in a `testcontainers-go` helper).

## In scope

### Step 1 — Cache directory layout

Create `internal/adapter/oci/layout.go` defining the on-disk layout (per OCI Image Layout spec):

```
~/.criteria/cache/oci/
  oci-layout           # JSON: { "imageLayoutVersion": "1.0.0" }
  index.json           # OCI index manifest referencing all cached refs
  blobs/
    sha256/
      <digest>         # raw blob bytes (binary, manifest, signature)
```

Functions:

```go
type Layout struct { Root string }

func Open(root string) (*Layout, error)       // creates if absent, validates layout version
func (l *Layout) Index() (*ocispec.Index, error)
func (l *Layout) WriteIndex(ix *ocispec.Index) error
func (l *Layout) BlobPath(d digest.Digest) string
func (l *Layout) HasBlob(d digest.Digest) bool
func (l *Layout) WriteBlob(reader io.Reader, expect digest.Digest) error  // atomic via tmp + rename, verifies digest
func (l *Layout) Lock() (release func(), err error)                       // flock-based, blocks concurrent writers
```

The lock uses `golang.org/x/sys/unix` flock on Linux/macOS. Windows-later: replaced by a portable equivalent — leave a TODO comment.

**Per-artifact protocol-version annotation (S3.3).** When the puller writes a manifest reference into `index.json`, it sets two OCI annotations on the descriptor so the loader can discriminate cached artifacts by protocol version without re-parsing `adapter.yaml`:

```
dev.criteria.adapter.protocol_version: "2"
dev.criteria.adapter.schema_version:   "1"
```

Annotation keys match WS05's namespace decision (D87). The Layout exposes a typed accessor:

```go
// ArtifactProtocolVersion returns the sdk_protocol_version annotation on the
// descriptor for `d`, or 0 if absent (treat as "unknown — re-read adapter.yaml").
func (l *Layout) ArtifactProtocolVersion(d digest.Digest) uint32
```

The host loader (WS03, WS08 wiring) consults this on every load and refuses any artifact whose protocol version is outside the host's supported range. This means a host upgrade that introduces protocol v3 alongside v2 can coexist with a cache mixing both versions — no cache wipe required.

### Step 2 — Reference parser

Create `internal/adapter/oci/reference.go`:

```go
// Reference is a parsed OCI reference: ghcr.io/org/name:tag or @sha256:digest.
type Reference struct {
    Registry string
    Repo     string  // org/name
    Tag      string  // optional
    Digest   digest.Digest  // optional; if present, Tag is ignored
}

func Parse(s string) (Reference, error)
func (r Reference) String() string
func (r Reference) FullyQualified() bool  // true if both Registry and (Tag or Digest) present
```

Support the short-alias form (`claude:1.2.3`) by **not** resolving aliases here — alias resolution is a higher layer (WS08) that turns the short form into a fully-qualified `Reference` before calling into this package.

### Step 3 — Puller

Create `internal/adapter/oci/pull.go`:

```go
type Puller struct {
    Layout *Layout
    Auth   AuthProvider  // resolves registry credentials; default looks at ~/.docker/config.json and AWS/GCR helpers
}

// Pull fetches the artifact for `ref`, writing all blobs into the Layout
// under blobs/sha256/<digest>/ and updating index.json. Returns the
// resolved digest of the artifact's manifest (caller can subsequently
// Open the manifest blob to read the adapter.yaml).
func (p *Puller) Pull(ctx context.Context, ref Reference) (digest.Digest, error)
```

Implementation uses `oras-go/v2`'s remote `remote.NewRepository` + `oras.Copy()` between the remote and a `oras-go/v2/content/oci`-backed Store wrapping our Layout.

### Step 4 — Resolver

Already partly the Puller's job. Add a non-pulling resolver:

```go
// Resolve queries the registry for the canonical digest of ref without
// fetching blobs. Used by `criteria adapter lock` (WS07) to compute
// lockfile entries without downloading binaries.
func (p *Puller) Resolve(ctx context.Context, ref Reference) (digest.Digest, error)
```

### Step 5 — Opener

Create `internal/adapter/oci/open.go`:

```go
// Open returns a read-only fs.FS rooted at the adapter's manifest blob.
// The returned FS exposes:
//   adapter.yaml           # the manifest blob
//   bin/<platform>         # the per-platform binary blobs
//   signatures/cosign.sig  # cosign signature blob, if present
//
// Callers use this to: (a) read adapter.yaml without parsing OCI layers,
// (b) get the binary path for execve in the loader.
func (l *Layout) Open(d digest.Digest) (fs.FS, error)
```

The Open implementation reads the manifest pointed at by `d`, walks its layers, and synthesizes a virtual FS over them. Layer paths follow OCI annotations the publish action (WS28) sets.

### Step 6 — Eviction

Create `internal/adapter/oci/gc.go`:

```go
type GCOptions struct {
    MaxSize       int64          // bytes; 0 = unlimited
    OlderThan     time.Duration  // 0 = age-irrelevant
    KeepReachable bool           // keep blobs referenced by index.json
}

func (l *Layout) GC(opts GCOptions) (GCResult, error)
```

GC walks `index.json` to build the reachable set, deletes unreachable blobs, then applies MaxSize/OlderThan trimming over remaining refs (least-recently-used by mtime of `index.json` entry).

### Step 7 — Tests

- `oci_layout_test.go` — round-trips blob writes, validates digest mismatch is rejected, validates flock prevents concurrent writes.
- `oci_pull_test.go` — uses `testcontainers-go` to spin up `registry:2.8`, pushes a synthetic OCI artifact via `oras-go`, has the Puller fetch it, verifies layout content.
- `oci_open_test.go` — synthesizes a fixture artifact on disk, opens it, reads `adapter.yaml`.
- `oci_gc_test.go` — populates a layout with multiple versions, validates GC keeps reachable + trims by size.

## Out of scope

- Cosign / signature verification — WS06 (reads the signature blob written by this WS).
- Manifest parsing (`adapter.yaml` schema and validation) — WS05.
- Lockfile read/write — WS07.
- CLI verbs that call these APIs — WS08.
- Pulling-during-compile integration — WS08.

## Reuse pointers

- `oras.land/oras-go/v2` — OCI client.
- `github.com/opencontainers/image-spec/specs-go/v1` — types.
- `golang.org/x/sys/unix` flock for the layout lock.
- The existing `~/.criteria/` state-directory helpers in `internal/runtime/state/` (or equivalent) — reuse the path resolution + `CRITERIA_STATE_DIR` env-var honoring.

## Behavior change

**No host-facing behavior change.** This adds a new package. Existing local discovery (`$CRITERIA_PLUGINS`, `~/.criteria/plugins/`) still works untouched; WS08 is where the new path becomes the primary discovery mechanism.

## Tests required

- All unit tests in `internal/adapter/oci/*_test.go` pass.
- Integration test against `registry:2.8` via `testcontainers-go`.
- `make ci` green.

## Exit criteria

- `internal/adapter/oci/` package exists and is exercised by tests.
- `oras-go/v2` and `image-spec` are listed in `go.mod`.
- No regression in existing adapter tests (which still use the legacy discovery path).

## Files this workstream may modify

- `internal/adapter/oci/*.go` *(all new)*
- `go.mod`, `go.sum` — adding `oras-go/v2`, `image-spec`.
- Test fixtures under `internal/adapter/oci/testdata/`.

## Files this workstream may NOT edit

- `internal/adapter/discovery.go` — left alone; new resolution path lands in WS08.
- `internal/cli/` — touched by WS08.
- `workflow/` — touched by WS07/WS09.
- `README.md`, `PLAN.md`, etc.
