# WS12 — Container-mode runtime (docker / podman) with fail-closed selection

**Phase:** Adapter v2 · **Track:** Security · **Owner:** Workstream executor · **Depends on:** [WS04](WS04-oci-cache-layout.md), [WS09](WS09-environment-block-and-secret-taint.md). · **Unblocks:** [WS40](WS40-v2-release-gate.md) verification gate 1 for container path.

## Context

`README.md` D12c. Container-mode is the cross-platform "stronger isolation" path. Runtime selection:

1. `environment.runtime ∈ {docker, podman}` and adapter has published a runnable image (D12b, `adapter.yaml.container_image` set) → `docker run <image>`.
2. `environment.runtime` set but no image published → **fail closed** with the publisher-pointing message (D12c.2).
3. `environment.runtime = "none"` (default) → subprocess mode.

The wrapping-binary-in-host-rootfs fallback considered in earlier drafts is **gone** — fail closed is the policy. There is no auto-build of a container at runtime.

## Prerequisites

WS04 (cache + manifest blob access), WS09 (environment block parsing with `runtime` field on `container` type).

## In scope

### Step 1 — Container handler

`internal/adapter/environment/container/handler.go`:

```go
func (h *Handler) Prepare(ctx PrepareContext) (Prepared, error) {
    env := ctx.Environment // workflow.EnvironmentNode
    mft := ctx.Manifest    // *manifest.Manifest from adapter.yaml
    if env.TypeSpecific["runtime"].AsString() == "none" {
        return nil, fmt.Errorf("container environment %q has runtime = \"none\"; this is the subprocess path; use a sandbox or shell environment instead",
            env.Name)
    }
    if mft.ContainerImage == nil {
        return nil, FailClosed{
            Reason:    "adapter does not publish a container image",
            Adapter:   ctx.AdapterRef,
            SourceURL: mft.SourceURL,
            Runtime:   env.TypeSpecific["runtime"].AsString(),
        }
    }
    // ... build the docker/podman command-line ...
}
```

The `FailClosed` error type formats the canonical message from `README.md` D12c.2.

### Step 2 — Command construction

Translate `ResolvedPolicy` into `docker run` arguments:

| Policy field | Docker flag |
|---|---|
| `network.allow = ["api.x:443"]` | `--network=criteria-host`, host-network with iptables outbound restricted (or `--network=none` + a host-level proxy — pick simplest implementation: `--add-host` + `iptables` is complex, so use a per-session sidecar pattern documented in `docs/adapters.md`) |
| `filesystem.read = [...]` | `-v <path>:<path>:ro` |
| `filesystem.write = [...]` | `-v <path>:<path>` |
| `resources.cpu = "2"` | `--cpus=2` |
| `resources.memory = "1Gi"` | `--memory=1Gi` |
| `resources.timeout = "5m"` | host-side context cancellation; no docker flag |
| `secrets {...}` | host-resolved values flow over the gRPC channel; no env-var smuggling |

**Important**: secrets do **not** become `-e SECRET=...` env vars (per D72/D73). They flow exclusively over the secret channel after the adapter has started.

For network policy specifically, this WS opts for the simplest correct option: `--network=criteria-net-<session>` with a per-session bridge network and per-host firewall rules — vendored as a small Go helper that talks to `iptables`/`pfctl` via subprocess only on host platforms where it works. Skip if not configurable on the host with a warning (permissive) or error (strict).

### Step 3 — Image pull integration

When an adapter has `container_image` set, the WS08 pull path (already pulls the artifact) needs to also pull the image. This WS adds:

```go
// PullContainerImage ensures the image referenced in adapter.yaml is
// present in the local docker/podman daemon. Uses `docker pull` /
// `podman pull` shelled out via os/exec.
func PullContainerImage(ctx context.Context, ref manifest.ContainerImageRef, runtime string) error
```

Wire it into the WS08 pull path conditionally (only when an environment that would use this image exists in the lockfile-pinned set).

### Step 4 — Tests

- Unit: command-construction table — every policy combination → expected docker args.
- Integration (gated by `CRITERIA_CONTAINER_TESTS=1`): launch a tiny test adapter image in docker, run a workflow against it, assert success.
- Fail-closed test: lockfile pins an adapter without `container_image`; environment has `runtime = "docker"`; pull/compile fails with the exact D12c.2 message.

## Out of scope

- Linux/macOS host-native sandbox — WS10/WS11.
- Building container images — that happens in WS28's publish action with `with_image: true`.
- Anything Kubernetes-specific — the `remote` environment (WS20) handles cluster scenarios.

## Reuse pointers

- `os/exec` to call docker/podman.
- WS09's `ResolvedPolicy`.
- WS04's `Layout.Open` to read `adapter.yaml`.

## Behavior change

**Yes** — adapters bound to a `container`-type environment now run via `docker run` (or `podman run`). Fail closed if image is missing. Subprocess mode (the v1 default) continues to work for adapters bound to non-container environments.

## Tests required

- Unit and integration tests as above.
- Fail-closed message regression test using golden file.

## Exit criteria

- Container-mode adapters work end-to-end in CI (gated test).
- Fail-closed errors quote `manifest.SourceURL`.

## Files this workstream may modify

- `internal/adapter/environment/container/*.go` *(new)*.
- `internal/adapter/loader.go` — dispatch to container handler when applicable.
- `internal/cli/adapter_pull.go` — call PullContainerImage when needed (small addition to WS08's verb).

## Files this workstream may NOT edit

- `internal/adapter/environment/sandbox/` — WS10/WS11.
- `internal/adapter/environment/remote/` — WS20.
- Other workstream files.
