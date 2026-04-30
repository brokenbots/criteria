# Docker runtime sandbox

## What this is

The `criteria/runtime` image is the interim sandbox for running Criteria workflows in a whole-process Docker boundary. It packages `criteria` and bundled adapters so operators can run workflows in a confined container instead of directly on the host.

## What this is not

This is not the Phase 3 per-adapter environment-plug abstraction, and it is not Phase 4 OS-level isolation controls. Those are separate planned deliverables tracked in [PLAN.md](../../PLAN.md):

- Phase 3: environments/plugs architecture in plugin loading.
- Phase 4: OS-level isolation controls (for example seccomp/sandbox-exec/Job Objects).

## How to use it

Run workflows by mounting a workspace and invoking `apply`:

```bash
docker run --rm \
  -v "$PWD:/workspace" \
  criteria/runtime:<tag> apply /workspace/<file>.hcl
```

Notes:

- The container only has host filesystem access through mounted volumes.
- Plugins are baked into `/home/criteria/.criteria/plugins/`.
- To use custom plugins, build a derived image that adds binaries under `/home/criteria/.criteria/plugins/`.

## Known limitations

- Shell-adapter semantics from Phase 1 still apply inside the container (env allowlist, PATH sanitization, and working-directory confinement); Docker adds an outer blast-radius boundary.
- The runtime base is Alpine, so shell steps run with Alpine `/bin/sh` (BusyBox) semantics unless operators build a derived image with a different shell.
- No GPU support by default.
- No host-network access unless the operator explicitly opts in (for example with `--net=host`).
- Approval and signal-wait nodes use existing local-mode mechanisms; for file-based approvals, mount the approvals path into the container.
- Host-mounted volumes can require ownership alignment for UID `10001`; use `chown -R 10001:10001 <dir>` or run with `--user $(id -u):$(id -g)`.
