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

- [x] Author `Dockerfile.runtime` (multi-stage; non-root user; entry
      point `criteria`).
- [x] Author `.devcontainer/devcontainer.json` and
      `.devcontainer/Dockerfile`.
- [x] Update `.dockerignore`.
- [x] Add `make docker-runtime` and `make docker-runtime-smoke`
      targets.
- [x] Run `make docker-runtime-smoke` locally; confirm exit 0.
- [x] Author `docs/runtime/docker.md`.
- [x] Add the pointer line to `docs/plugins.md`.
- [x] Verify the dev container opens cleanly in VS Code and `make
      ci` runs inside it.
- [x] `make ci` green on the host (independent of the container).

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

## Reviewer notes (batch 1)

- Added `Dockerfile.runtime` with a multi-stage build (`golang:1.26-alpine` -> `alpine:3.20`), `CGO_ENABLED=0`, non-root runtime user `criteria` (UID 10001), `WORKDIR /workspace`, and `ENTRYPOINT ["/usr/local/bin/criteria"]`.
- Runtime image includes `ca-certificates` and `git`; adapter binaries are copied to `/usr/local/bin/` and baked into `/home/criteria/.criteria/plugins/`.
- Added Make targets:
  - `make docker-runtime`
  - `make docker-runtime-smoke`
- Added `.devcontainer/devcontainer.json` and `.devcontainer/Dockerfile` using the Go 1.26 devcontainer base, Docker-in-Docker feature, and `postCreateCommand: make bootstrap`.
- Devcontainer image now ensures writable Go module/build caches for `vscode` (`/go` and `/home/vscode/.cache`) so `make ci` works inside the container.

### Validation executed

- `make docker-runtime-smoke` ✅
  - Workflow `examples/hello.hcl` completed successfully inside the runtime image (`finalState":"done","success":true`).
- `docker run --rm --entrypoint id criteria/runtime:dev -u` ✅
  - Output: `10001`.
- `docker build -t criteria/devcontainer:dev -f .devcontainer/Dockerfile .` ✅
- `docker run --rm -v "$PWD:/workspace" -w /workspace criteria/devcontainer:dev bash -lc 'make ci'` ✅
- `make ci` (host) ✅

## Reviewer notes (batch 2)

- Added `docs/runtime/docker.md` with the required four sections:
  - What this is (interim whole-process Docker sandbox).
  - What this is not (explicitly distinguishes Phase 3 environment plugs and Phase 4 OS-level isolation, with `PLAN.md` link).
  - How to use it (`docker run ... criteria/runtime:<tag> apply /workspace/<file>.hcl` with workspace mount).
  - Known limitations (shell semantics, networking/GPU notes, approval/signal-wait mounting note, UID `10001` volume ownership guidance).
- Added the required top-of-file pointer sentence to `docs/plugins.md` before the first `##` heading:
  - `For containerized execution, see [docs/runtime/docker.md](runtime/docker.md).`
- Addressed reviewer nit by pinning Buf install in `.devcontainer/Dockerfile`:
  - `github.com/bufbuild/buf/cmd/buf@v1.68.4` (replaces `@latest`).

### Validation executed (batch 2)

- `docker build -t criteria/devcontainer:dev -f .devcontainer/Dockerfile .` ✅
- `docker run --rm criteria/devcontainer:dev buf --version` ✅ (`1.68.4`)
- `docker run --rm -v "$PWD:/workspace" -w /workspace criteria/devcontainer:dev bash -lc 'make ci'` ✅
- `make docker-runtime-smoke` ✅
- `make ci` (host) ✅

## Reviewer Notes

### Review 2026-04-29 — changes-requested

#### Summary

The Dockerfile, devcontainer, `.dockerignore`, and Makefile targets are well-implemented and functionally validated. The runtime image passes all container-level requirements: non-root UID 10001, correct entrypoint, no CMD, CGO_ENABLED=0 static binaries, plugins baked into the correct discovery path, and the smoke test exits 0 with `"finalState":"done","success":true`. However, **two exit criteria are unmet**: `docs/runtime/docker.md` does not exist, and the `docs/plugins.md` pointer has not been added. These are hard blockers — the workstream cannot be approved until they are delivered. One additional nit must also be addressed before approval.

#### Plan Adherence

- Step 1 (Dockerfile.runtime): ✅ Implemented. Multi-stage build, `golang:1.26-alpine` / `alpine:3.20`, `CGO_ENABLED=0`, non-root user `criteria` UID 10001, `ca-certificates` + `git`, adapters baked into `/home/criteria/.criteria/plugins/`, `ENTRYPOINT ["/usr/local/bin/criteria"]`, no `CMD`, `WORKDIR /workspace`. Matches spec exactly.
- Step 2 (.devcontainer): ✅ Implemented. `devcontainer.json` uses correct base, Go 1.26 feature, Docker-in-Docker feature, `postCreateCommand: make bootstrap`, Go extension. `.devcontainer/Dockerfile` installs `ca-certificates`, `curl`, `git`, `make`, and `buf`. Cache dirs for `vscode` are pre-created.
- Step 3 (Makefile targets): ✅ `docker-runtime` and `docker-runtime-smoke` added; both are in `.PHONY`.
- Step 4 (Smoke test): ✅ `make docker-runtime-smoke` exits 0. Full expected output documented in executor's batch-1 notes.
- Step 5 (docs/runtime/docker.md + docs/plugins.md pointer): ❌ **Neither delivered.** Tasks remain unchecked; neither file was created/modified. This is a hard exit criterion failure.
- Step 6 (.dockerignore): ✅ Excludes all plan-required paths (`bin/`, `.git/`, `tech_evaluations/`, `cover*.out`, `tmp/`, `node_modules/`).
- Exit criterion — VS Code "Reopen in Container": The executor performed the functional equivalent (built the devcontainer image and ran `make ci` inside it via `docker run`). The actual VS Code UI flow cannot be exercised in a CLI environment; the functional validation is accepted as equivalent for review purposes.

#### Required Remediations

- **[BLOCKER] `docs/runtime/docker.md` missing.**
  File path: `docs/runtime/docker.md` (new).
  Required per Step 5 and an explicit exit criterion. Must cover: (1) what this is — whole-process Docker sandbox; (2) what this is not — environment plug (Phase 3) / OS-level isolation (Phase 4), with links to PLAN.md; (3) how to use it — `docker run criteria/runtime:<tag> apply /workspace/<file>.hcl` with volume mount, no host filesystem access outside the mount, custom plugins require rebuilding; (4) known limitations — Alpine shell semantics, no GPU, no host network by default, approval/signal-wait nodes via W06 local-mode, operators must `chown -R 10001:10001` volumes or use `--user`.
  Acceptance: file exists, covers all four required sections, clearly names Docker as interim sandbox, distinguishes environment-plug Phase 3 and OS-level Phase 4 by name, links to PLAN.md.

- **[BLOCKER] `docs/plugins.md` pointer missing.**
  File path: `docs/plugins.md` (existing, line 1 area).
  Required per Step 5: add a one-line pointer at the top of the file: _"For containerized execution, see [docs/runtime/docker.md](runtime/docker.md)."_
  Acceptance: `docs/plugins.md` contains the exact pointer sentence before its first `##` heading.

- **[NIT] `buf` installed at `@latest` in `.devcontainer/Dockerfile`.**
  File: `.devcontainer/Dockerfile`, line 9: `go install github.com/bufbuild/buf/cmd/buf@latest`.
  `@latest` is non-deterministic across devcontainer rebuilds. Pin to the specific `buf` version already exercised by the repo's `buf.yaml` / CI (identify via `buf --version` in CI or `buf.yaml` required-version if set).
  Acceptance: `@latest` is replaced with a pinned semver tag (e.g., `v1.X.Y`).

#### Test Intent Assessment

This workstream explicitly defers Go tests in favour of container-level smoke verification. The smoke test is meaningful: it exercises the real binary, plugin discovery, the shell adapter, and event emission end-to-end inside the runtime image. The output includes structured event JSON including `StepLog`, `StepOutcome`, `StepOutputCaptured`, `StepTransition`, and `RunCompleted` with `"success":true`. That is sufficient behavioural evidence for the stated scope. No Go test additions are required or expected per the workstream.

#### Validation Performed

- `make ci` (host): exit 0 ✅
- `make docker-runtime-smoke`: exit 0 ✅. Observed output: StepLog `"hello from criteria"`, RunCompleted `"finalState":"done","success":true`.
- `docker run --rm --entrypoint id criteria/runtime:dev -u`: `10001` ✅
- `docker run --rm --entrypoint id criteria/runtime:dev -un`: `criteria` ✅
- `docker inspect criteria/runtime:dev` — User: `criteria`, WorkingDir: `/workspace`, Entrypoint: `[/usr/local/bin/criteria]`, Cmd: null ✅

### Review 2026-04-29-02 — approved

#### Summary

All three required remediations from the first review are resolved. `docs/runtime/docker.md` exists and covers all four required sections (what it is, what it isn't with explicit Phase 3 / Phase 4 distinction and PLAN.md link, how to use it, known limitations). The `docs/plugins.md` pointer appears correctly before the first `##` heading. `buf` is pinned to `v1.68.4` in `.devcontainer/Dockerfile`. Every exit criterion is met; all task checkboxes are complete. This workstream is approved.

#### Plan Adherence

- Step 1 (Dockerfile.runtime): ✅ Unchanged; confirmed correct from prior review.
- Step 2 (.devcontainer): ✅ `buf` now pinned to `v1.68.4`; all other fields unchanged and correct.
- Step 3 (Makefile targets): ✅ Unchanged; confirmed correct.
- Step 4 (Smoke test): ✅ `make docker-runtime-smoke` independently re-verified; exits 0, `"finalState":"done","success":true`.
- Step 5 (docs): ✅ `docs/runtime/docker.md` created with all four sections; PLAN.md link via `../../PLAN.md` is path-correct. `docs/plugins.md` pointer added after `# heading`, before first `##` section, matching acceptance criteria.
- Step 6 (.dockerignore): ✅ Unchanged; confirmed correct.
- All tasks: all nine task checkboxes marked complete by executor.

#### Validation Performed

- `make ci` (host): exit 0 ✅
- `make docker-runtime-smoke`: exit 0 ✅. Output: `"finalState":"done","success":true`.
- `docs/runtime/docker.md` content verified: four required sections present, Phase 3 and Phase 4 named explicitly, links to PLAN.md ✅
- `docs/plugins.md` pointer present before first `##` heading ✅
- `.devcontainer/Dockerfile` line 9: `buf@v1.68.4` (pinned) ✅
