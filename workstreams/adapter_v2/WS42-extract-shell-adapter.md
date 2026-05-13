# WS42 — Extract `shell` adapter to its own repo

**Phase:** Adapter v2 · **Track:** End-state independence · **Owner:** Workstream executor · **Depends on:** [WS31](WS31-migrate-shell.md), [WS40](WS40-v2-release-gate.md), [WS41](WS41-extract-adapter-proto-repo.md). · **Unblocks:** [WS43](WS43-independence-verification.md).

## Context

`README.md` D58. The last in-tree adapter is `shell`. After this WS, criteria's host code has zero in-tree adapter implementations — every adapter is an independent repo consuming the proto package and one of the SDKs.

## Prerequisites

WS31 (shell migrated to v2 on Go SDK while still in-tree), WS40 (release shipped), WS41 (proto extracted so the new repo can consume it as a package).

## In scope

### Step 1 — Create `criteria-adapter-shell` repo

Apache-2. Standard layout.

### Step 2 — Move sources with history

Use `git filter-repo` to extract `internal/builtin/shell/` from criteria's history. New repo's layout follows the Go SDK starter (WS27).

### Step 3 — Consume the Go SDK + proto package

`go.mod`:

```
require (
    github.com/brokenbots/criteria-adapter-proto v1.0.0
    github.com/brokenbots/criteria-go-adapter-sdk v1.0.0
)
```

### Step 4 — Adopt the standard build pipeline

`.github/workflows/publish.yml` invokes `criteria/publish-adapter@v1` with `sdk: go`, `with_image: false`. The shell adapter is pure-binary; no container image needed.

### Step 5 — Update criteria

Remove `internal/builtin/shell/`. Update the host loader: shell is now an external adapter that gets pulled like any other. Add a baked-in default registry ref `ghcr.io/criteria-adapters/shell:LATEST_STABLE` so default workflows still work without explicit pull.

The "criteria binary's built-in dispatch for `--builtin-shell`" path is removed.

### Step 6 — Update fixtures and tests

Every fixture workflow that uses `shell` now requires a lockfile entry. Add one to the canonical fixtures. The integration test suite must `criteria adapter pull` the shell adapter at setup.

### Step 7 — Tag a release

`v2.0.0` of the new repo, published as a cosign-signed OCI artifact.

## Out of scope

- Other adapter migrations — done already.
- Final verification — WS43.

## Behavior change

**Mostly invisible to users.** The shell adapter is no longer special — it gets pulled and cached like any other. Workflows that referenced `adapter "shell" "default"` continue to work as long as the lockfile pins shell. Workflows without a lockfile fail with the standard "run `criteria adapter lock`" hint (per WS08).

## Tests required

- The extracted shell adapter passes the conformance suite.
- criteria's existing shell-using fixtures pass after pulling shell as an external adapter.

## Exit criteria

- `criteria-adapter-shell` repo exists, published, signed.
- `internal/builtin/shell/` removed from criteria.
- All shell-using tests pass.

## Files this workstream may modify

- Everything in the new `criteria-adapter-shell` repo.
- Deletions in criteria's `internal/builtin/shell/`.
- Updates to criteria's fixtures + loader.

## Files this workstream may NOT edit

- Other workstream files.
