# Workstream 9 — Docker dev container and operator runtime image

**Owner:** Workstream executor · **Depends on:** none · **Coordinates with:** [W13](13-rc-artifact-upload.md) (RC PRs upload the runtime image), [W14](14-phase2-cleanup-gate.md) (cleanup gate verifies a smoke run inside the container).

## Context

The Phase 2 plan ships the Docker dev container as the team's
**interim runtime sandbox** while OS-level isolation (sandbox-exec,
seccomp, Job Objects) and the architecture team's "environments /
plugs" abstraction are still deferred to later phases. Two separate
deliverables in this workstream:

1. **VS Code dev container** (`.devcontainer/devcontainer.json`) —
   for repo-level development. Lets a contributor open the repo in
   VS Code or any devcontainers-spec compatible IDE and have a
   ready-to-build environment with Go, buf, golangci-lint, etc.,
   without local toolchain drift.
2. **Operator runtime image** (`criteria/runtime:v0.3.0` / similar
   tag) — Alpine-based image containing `bin/criteria` plus the
   bundled adapter binaries (`criteria-adapter-copilot`,
   `criteria-adapter-mcp`, `criteria-adapter-noop`). Documented as
   the recommended way to run workflows in a sandboxed environment
   until per-environment plugs (Phase 3) and OS-level controls
   (Phase 4) land.

These are not the architecture's "environment plug" abstraction —
that is Phase 3 and lives in the plugin loader. This workstream is
the broad-stroke whole-process sandbox; the README must call out the
distinction explicitly so future readers do not conflate the two.

## Prerequisites

- `make ci` green on `main`.
- Docker installed locally for testing.
- Familiarity with the existing `Makefile` build targets (`make
  build`, `make plugins`).
- Familiarity with the existing examples under `examples/` — at
  least one will be used as the smoke-test workflow inside the image.

## In scope

### Step 1 — Author the operator runtime Dockerfile

Create `Dockerfile.runtime` at the repo root.

- **Base:** `golang:1.26-alpine` for the build stage; `alpine:3.20`
  (or current LTS) for the runtime stage. Multi-stage build.
- **Build stage:** copies the repo, runs `go work sync` then
  `make build` and `make plugins`. Outputs to `/out/bin/`.
- **Runtime stage:** copies binaries from `/out/bin/` into
  `/usr/local/bin/`. Sets up:
  - Non-root user `criteria` (UID 10001).
  - `/workspace` mount point (default working directory).
  - `/home/criteria/.criteria/plugins/` populated with the adapter
    binaries (so `criteria` discovers them).
  - `ENTRYPOINT ["/usr/local/bin/criteria"]` so `docker run
    criteria/runtime:v0.3.0 apply <args>` does the right thing.
  - `WORKDIR /workspace`.
  - No `CMD` (operator must specify the subcommand).

Dependencies inside the runtime image:

- `ca-certificates` (TLS).
- `git` (some workflows shell out to git).
- No build tools (the runtime image is for *running* workflows, not
  building Criteria from source).

The image must run as the non-root user. State writes to
`~/.criteria/` (which is `/home/criteria/.criteria/` inside the
container). Volume-mount `/workspace` for the workflow file and any
output artifacts. Document the expected `docker run` invocation.

### Step 2 — Author the VS Code dev container

Create `.devcontainer/devcontainer.json` and
`.devcontainer/Dockerfile`.

`devcontainer.json` shape (concrete fields — adjust to current
devcontainer spec):

```jsonc
{
  "name": "Criteria",
  "build": { "dockerfile": "Dockerfile" },
  "remoteUser": "vscode",
  "features": {
    "ghcr.io/devcontainers/features/go:1": { "version": "1.26" },
    "ghcr.io/devcontainers/features/docker-in-docker:2": {}  // for testing the runtime image
  },
  "postCreateCommand": "make bootstrap",
  "customizations": {
    "vscode": {
      "extensions": ["golang.go"]
    }
  }
}
```

`Dockerfile` (the dev container image):

- Base: `mcr.microsoft.com/devcontainers/go:1.26-bookworm` or current
  equivalent.
- Install: `buf`, `make`, `golangci-lint` (or rely on
  `go tool golangci-lint` per the existing Makefile).
- Pre-fetch Go modules via `RUN go mod download` for the workspace
  (optional optimization).

Validate by opening the repo in VS Code's "Dev Containers: Open
Folder in Container" and running `make ci` inside the container. The
contributor's first experience should be: clone, open in VS Code,
hit "Reopen in Container", wait, then `make ci` works.

### Step 3 — Build automation

Add `Makefile` targets:

```make
docker-runtime: ## Build the operator runtime image (Dockerfile.runtime)
	docker build -t criteria/runtime:dev -f Dockerfile.runtime .

docker-runtime-smoke: docker-runtime ## Run a workflow inside the runtime image
	docker run --rm -v "$$PWD/examples:/workspace/examples:ro" \
	    criteria/runtime:dev apply /workspace/examples/hello.hcl
```

Add to `.PHONY`. The `dev` tag is for local testing; the actual
release tag (e.g. `v0.3.0-rc1`) is set by [W13](13-rc-artifact-upload.md)
in CI.

### Step 4 — Smoke test

The runtime image must successfully run `examples/hello.hcl` (or
whichever example does not require a server). Verify:

```sh
make docker-runtime-smoke
```

Returns 0 and the workflow run succeeds. Document the expected
output in reviewer notes.

If `examples/hello.hcl` is not standalone-runnable for some reason
(e.g. requires a plugin not in the image), pick another example or
add a minimal one specifically for the smoke test. The smoke test is
the defining acceptance criterion for the image.

### Step 5 — Document the two artifacts and their distinction

Create `docs/runtime/docker.md`:

1. **What this is.** The interim sandbox for running Criteria
   workflows in a confined process boundary. Whole-process Docker
   isolation.
2. **What this is not.** The per-adapter "environment plug"
   abstraction (Phase 3) or OS-level isolation (Phase 4). Note both
   future deliverables and link to PLAN.md.
3. **How to use it.**
   - `docker run criteria/runtime:<tag> apply /workspace/<file>.hcl`
     with the workspace volume-mounted.
   - Operator owns the volume; container has no host filesystem
     access outside the mount.
   - Plugins are baked into the image; custom plugins require
     rebuilding the image with the additional binaries placed under
     `/home/criteria/.criteria/plugins/`.
4. **Known limitations.**
   - The shell adapter still has the same Phase 1 sandbox semantics
     (env allowlist, PATH sanitization, working-dir confinement)
     within the container — but the *container itself* now bounds
     the blast radius.
   - No GPU access. No host network access by default (use `--net`
     to override at the operator's choice).
   - Approval / signal-wait nodes work via [W06](06-local-mode-approval.md)'s
     local-mode mechanisms; operators using `file` mode must
     volume-mount the approvals dir if the decision file is written
     from outside the container.

Update [docs/plugins.md](../docs/plugins.md) to add a short pointer
at the top: "For containerized execution, see
[docs/runtime/docker.md](runtime/docker.md)."

Do **not** edit `README.md`, `PLAN.md`, `AGENTS.md`, `CHANGELOG.md`.
W14 picks up the README announcement.

### Step 6 — `.dockerignore`

Add or update `.dockerignore` to exclude `bin/`, `.git/`,
`tech_evaluations/`, `cover-*.out`, `tmp/`, `node_modules/` (if any),
and any other non-build artifacts. The build stage performs a fresh
`make build` inside the container; the host's `bin/` is irrelevant
and would only confuse the image layer cache.

## Behavior change

**Yes — new delivery surface; no engine behavior change.**

- New repo files: `Dockerfile.runtime`, `.devcontainer/`,
  `docs/runtime/docker.md`, `.dockerignore`.
- New Makefile targets: `docker-runtime`, `docker-runtime-smoke`.
- New published artifact: the runtime container image, tagged via
  CI ([W13](13-rc-artifact-upload.md)). The image is built from
  `Dockerfile.runtime` and contains the same binaries as a host
  `make build && make plugins`.
- CLI behavior when run on the host (outside any container) is
  **unchanged**.
- Inside the container, `~/.criteria/` is at
  `/home/criteria/.criteria/` (the non-root user's home). [W04](04-state-dir-permissions.md)'s
  `0o700` mode is honored.
- Plugins are discovered from
  `/home/criteria/.criteria/plugins/` (matches existing default).
  `${CRITERIA_PLUGINS}` override still works.

## Reuse

- Existing `make build` and `make plugins` targets — invoke from the
  Dockerfile build stage; do not duplicate Go build commands.
- Existing `examples/hello.hcl` (or another simple example) for the
  smoke test.
- Existing plugin discovery semantics (no new env var, no new code
  path).
- The non-root user pattern is standard; pick a UID that does not
  conflict with common host UIDs (10001 is conventional for service
  accounts).

## Out of scope

- The architecture's "environment plug" abstraction. That is Phase 3,
  living in `internal/plugin/loader.go`.
- macOS or Windows native sandboxing. Docker is the only deliverable.
- Multi-arch builds (linux/arm64). Add to a follow-up workstream if
  contributors need it; default to linux/amd64 for v0.3.0.
- Publishing the image to a registry. CI uploads it as a GitHub PR
  artifact via [W13](13-rc-artifact-upload.md); registry publish is
  the existing release process and out of this workstream.
- Custom-plugin injection at runtime via volume mount (the user
  provides their own plugin binary). Document but do not implement —
  baking into a derived image is the supported path for now.
- A `criteria-runtime-distroless` variant. Alpine is fine for v0.3.0.

## Files this workstream may modify

- `Dockerfile.runtime` (new).
- `.devcontainer/devcontainer.json` (new).
- `.devcontainer/Dockerfile` (new).
- `.dockerignore` (new or extended).
- `Makefile` (new `docker-runtime` and `docker-runtime-smoke`
  targets).
- `docs/runtime/docker.md` (new).
- `docs/plugins.md` (one-line pointer at the top).

This workstream may **not** edit `README.md`, `PLAN.md`, `AGENTS.md`,
`CHANGELOG.md`, `workstreams/README.md`, or any other workstream file.
It may **not** modify any code under `internal/`, `cmd/`, `workflow/`,
`sdk/`, or `events/` — the binaries it ships are the existing ones.

## Tasks

- [ ] Author `Dockerfile.runtime` (multi-stage; non-root user; entry
      point `criteria`).
- [ ] Author `.devcontainer/devcontainer.json` and
      `.devcontainer/Dockerfile`.
- [ ] Update `.dockerignore`.
- [ ] Add `make docker-runtime` and `make docker-runtime-smoke`
      targets.
- [ ] Run `make docker-runtime-smoke` locally; confirm exit 0.
- [ ] Author `docs/runtime/docker.md`.
- [ ] Add the pointer line to `docs/plugins.md`.
- [ ] Verify the dev container opens cleanly in VS Code and `make
      ci` runs inside it.
- [ ] `make ci` green on the host (independent of the container).

## Exit criteria

- `make docker-runtime` succeeds.
- `make docker-runtime-smoke` exits 0 with the smoke workflow
  succeeding inside the container.
- Image runs as non-root (UID 10001).
- VS Code "Reopen in Container" succeeds; `make ci` inside the
  container exits 0.
- `docs/runtime/docker.md` exists and clearly distinguishes the
  three layers (whole-process Docker now, environment plugs Phase 3,
  OS-level Phase 4).
- `make ci` green on the host.

## Tests

This workstream does not add Go tests. Verification is the
`make docker-runtime-smoke` target plus VS Code dev container open
and `make ci` execution inside the dev container. Document the
manual verification steps in reviewer notes.

If feasible, add a CI step in [W13](13-rc-artifact-upload.md)'s scope
that builds the runtime image as part of the artifact bundle. That
step is the durable signal that the Dockerfile stays buildable.

## Risks

| Risk | Mitigation |
|---|---|
| `golang:1.26-alpine` is not yet released when this workstream lands | Use `golang:1.26` (Debian-based) for the build stage; switch to alpine when available. The runtime stage stays alpine-based. |
| The Alpine runtime's `git` is incompatible with some workflows that depend on git features | Document the Alpine git version. If a workflow needs a newer git, the operator can build a derived image. |
| Plugin binaries built inside the container target a different libc than the host expects | The build stage uses the same toolchain as the runtime stage (Alpine → Alpine via build args, or static Go binaries via `CGO_ENABLED=0`). Set `CGO_ENABLED=0` in the build stage to produce fully static binaries that run on any kernel ≥ the build kernel. |
| Dev container image is large (several GB) and slow to build | Devcontainers are a one-time cost per contributor. Use Microsoft's prebuilt Go base; install only what `make ci` needs. |
| `${CRITERIA_PLUGINS}` defaults inside the container conflict with the operator's host expectations | Document explicitly: inside the container the plugins live at `/home/criteria/.criteria/plugins/` and are baked in. Operators can override via `--env CRITERIA_PLUGINS=/workspace/plugins -v ./plugins:/workspace/plugins`. |
| The smoke test workflow chokes on Alpine's `sh` (busybox) for shell-adapter steps | `examples/hello.hcl` is a noop-flavored example and does not exercise shell. If a future smoke test needs `bash`, switch the runtime base to a Debian slim. Acceptable for v0.3.0 to skip shell-heavy smoke tests. |
| The non-root UID conflicts with a host volume's ownership | Document: operators who mount a host directory must `chown -R 10001:10001` the dir or run with `--user $(id -u):$(id -g)`. This is standard Docker pain; not unique to Criteria. |
