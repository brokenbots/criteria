# WS40 — v2 release gate: tag and ship

**Phase:** Adapter v2 · **Track:** Release gate · **Owner:** Workstream executor · **Depends on:** WS01–WS39 (all). · **Unblocks:** [WS41](../archived/v4/adapter-v2/WS41-extract-adapter-proto-repo.md), [WS42](../archived/v4/adapter-v2/WS42-extract-shell-adapter.md), [WS43](WS43-independence-verification.md). · **Base branch:** `adapter-v2`

> **Reassessment (2026-06-05).** The four gates were re-scoped against current reality:
> - **Gate 1 (conformance) — DONE, rescoped.** Per [ADR-0003](../../docs/adrs/ADR-0003-conformance-scope.md) the host validates host + the imported Go SDK + `proto-drift`, all green in `ci.yml`. It no longer requires every SDK's reference adapter on every platform; each SDK owns its own conformance.
> - **Gate 2 (migrated adapters in CI) — rescope to match.** `ci.yml` e2e already builds and exercises the in-tree adapters (shell/copilot/noop/mcp) + example workflows. The host should not run all five external TS adapters in its own CI; it should run the in-tree set plus **one representative external adapter via the OCI pull path** (available once WS28 lands). External adapters self-test in their repos.
> - **Gate 3 (remote e2e) — real, needs a run.** `remote-e2e.yml` builds the remote smoke adapter, dockerizes it, and runs `go test ./internal/ci/smoke/...`. It triggers only on `tag v*` / weekly / `workflow_dispatch`, so it has not run on `adapter-v2`. Validate via dispatch and wire into the RC flow.
> - **Gate 4 (publishing-flow) — the real remaining blocker.** Needs **WS28** (publish action) + **WS27** (starters). This is the next work item.
>
> Net: only Gate 4 (WS28/WS27) is substantive; Gate 3 needs one validation run; Gates 1–2 are done/rescoped.

## Context

`README.md` D57. Stand up the four verification gates, confirm they pass on the tip of main, and tag the v2 release.

The four gates:

1. **Conformance suite** (WS26) green for all SDKs on all platforms.
2. **All seven migrated adapters** (WS30–WS36) run their representative workflows in CI.
3. **Remote transport end-to-end** (WS22, WS38 gate 3).
4. **Publishing-flow gate** (WS38 gate 4).

## Prerequisites

WS01–WS39 merged.

## In scope

### Step 1 — Run gates against tip-of-main

Trigger the four CI workflows manually against `main` (or against a candidate release branch). All must pass.

### Step 2 — Tag

```sh
git tag -s v2.0.0 -m "Adapter v2 release"
git push origin v2.0.0
```

The signed tag triggers the existing release-tag CI (which produces the criteria binary releases, publishes to Homebrew tap, etc. — existing infrastructure, unchanged).

### Step 3 — GitHub Release notes

Generate from `CHANGELOG.md` v2.0.0 section (written in WS39). Include links to:

- Each migrated adapter's published v2 artifact.
- Each SDK's npm/pypi/Go module published v2 release.
- The starter templates.
- The migration guide.

### Step 4 — Archive workstreams

After release: move `workstreams/adapter_v2/` to `workstreams/archived/v2-adapters/`. Update `workstreams/README.md` to reflect closure.

## Out of scope

- The independence-extraction WSes (WS41–WS43) which happen *after* v2 ships.

## Behavior change

**N/A — release process.**

## Tests required

- All four gates green.
- Signed tag verifies.
- Homebrew tap update succeeds.

## Exit criteria

- v2.0.0 tagged, signed, released.
- `workstreams/adapter_v2/` archived.

## Files this workstream may modify

- `workstreams/README.md` (close-out edit) — under cleanup-gate-equivalent permission, justified by WS39 having opened the cleanup window.
- Move (`git mv`) of `workstreams/adapter_v2/` to `archived/`.

## Files this workstream may NOT edit

- Source code (all done earlier).
- Other workstream files except via `git mv`.
