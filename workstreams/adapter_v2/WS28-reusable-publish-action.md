# WS28 — Reusable composite GitHub Action `criteria/publish-adapter`

**Phase:** Adapter v2 · **Track:** CI scaffolding · **Owner:** Workstream executor (new repo `criteria/publish-adapter` under brokenbots org) · **Depends on:** [WS05](WS05-adapter-manifest.md), [WS23](WS23-typescript-sdk-v2.md) / [WS24](WS24-python-sdk-v2.md) / [WS25](WS25-go-sdk-v1.md). · **Unblocks:** [WS27](WS27-starter-repos.md), every adapter migration.

## Context

`README.md` D15, D47, D12d. A reusable composite action that handles: multi-arch build → manifest emit (via `--emit-manifest`) → OCI artifact construction via `oras` → cosign keyless sign → push. When `with_image: true`, also builds + signs + pushes a runnable container image and updates the published `adapter.yaml` with the `container_image` block.

## Prerequisites

WS05 (manifest schema), at least one SDK at RC.

## In scope

### Step 1 — Action layout

`action.yml` with inputs:

```yaml
name: 'Publish criteria adapter'
inputs:
  sdk:               # "typescript" | "python" | "go"
    required: true
  registry:          # default: ghcr.io/${{ github.repository_owner }}
    required: false
  name:              # adapter name (defaults to repo name)
    required: false
  version:           # defaults to git tag without leading "v"
    required: false
  platforms:         # default: "linux/amd64,linux/arm64,darwin/arm64"
    required: false
  with_image:        # default: "false"
    required: false
  dockerfile_path:   # default: "./Dockerfile"; only used when with_image=true
    required: false
  signing_mode:      # "keyless" (default) | "key"
    required: false
  signing_key:       # cosign private key for key mode (consumed from a secret)
    required: false
```

### Step 2 — Build step

Per-SDK build invocation:

- **TypeScript**: `bun build --compile --target=bun-<platform> index.ts --outfile out/adapter-<platform>`.
- **Python**: `uv run python -m nuitka --onefile --standalone main.py -o out/adapter-<platform>` (with the appropriate platform setup).
- **Go**: `GOOS=<os> GOARCH=<arch> go build -o out/adapter-<platform> ./cmd/adapter`.

Run for each platform in the platforms input.

### Step 3 — Manifest emit

Invoke any one of the built binaries (linux/amd64 is canonical) with `--emit-manifest > adapter.yaml`. Validate against the WS05 schema (a small Go helper `criteria-manifest-validate` invoked from the action).

### Step 4 — OCI artifact construction

Use `oras`:

```bash
oras push $REGISTRY/$NAME:$VERSION \
  --artifact-type application/vnd.criteria.adapter.v1+json \
  --annotation-from-file adapter.yaml.annotations \
  adapter.yaml:application/vnd.criteria.adapter.manifest.v1+yaml \
  out/adapter-linux-amd64:application/vnd.criteria.adapter.binary.v1+octet-stream \
  out/adapter-linux-arm64:application/vnd.criteria.adapter.binary.v1+octet-stream \
  out/adapter-darwin-arm64:application/vnd.criteria.adapter.binary.v1+octet-stream
```

`adapter.yaml.annotations` is a generated file containing per-blob layer annotations (`com.brokenbots.criteria.adapter.platform` etc.) so the WS04 opener can build its virtual FS.

### Step 5 — Cosign signing

`cosign sign --yes $REGISTRY/$NAME:$VERSION` for keyless mode (uses workflow's OIDC token). Or `cosign sign --key env://COSIGN_PRIVATE_KEY ...` for key mode.

### Step 6 — Container image mode (`with_image: true`)

If enabled:

1. `docker build -t $REGISTRY/$NAME:$VERSION-image -f $DOCKERFILE_PATH .`.
2. `docker push $REGISTRY/$NAME:$VERSION-image`.
3. `cosign sign $REGISTRY/$NAME:$VERSION-image` (keyless or key).
4. Re-emit `adapter.yaml` with `container_image: { ref: ..., digest: ... }` filled in.
5. Re-push the OCI artifact with the updated manifest.

### Step 7 — Shared library

The build + manifest + push logic is also exposed as a Go library (or shell scripts the action calls) so `criteria adapter publish` (WS08 Step 5) can reuse the same code paths.

### Step 8 — Tests

- A test workflow in the action's repo that runs the action against a fixture adapter (one per SDK) and verifies:
  - The artifact is pushed.
  - The signature verifies.
  - `criteria adapter pull` succeeds against the pushed artifact.

## Out of scope

- GitLab CI / Makefile equivalents — WS29.
- Cosign key management documentation — included as a README section but no infrastructure.

## Behavior change

**N/A — new action.**

## Tests required

- The action's own CI runs it on every PR.

## Exit criteria

- Action published as `criteria/publish-adapter@v1` (tagged release on the new repo).
- All three starter repos (WS27) use it and publish successfully.

## Files this workstream may modify

- Everything in `criteria/publish-adapter/` repo.

## Files this workstream may NOT edit

- SDK repos directly (they consume this action).
- The criteria monorepo.
- Other workstream files.
