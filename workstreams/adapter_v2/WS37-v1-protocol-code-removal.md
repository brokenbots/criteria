# WS37 — Remove v1 protocol code paths

**Phase:** Adapter v2 · **Track:** Release gate · **Owner:** Workstream executor · **Depends on:** [WS30](WS30-migrate-greeter.md), [WS31](WS31-migrate-shell.md), [WS32](WS32-migrate-claude.md), [WS33](WS33-migrate-claude-agent.md), [WS34](WS34-migrate-codex.md), [WS35](WS35-migrate-openai.md), [WS36](WS36-migrate-copilot.md). · **Unblocks:** [WS41](WS41-extract-adapter-proto-repo.md).

## Context

`README.md` D2 — hard cut to v2. After all seven adapters are migrated and verified, the v1 host code paths are deleted. No deprecation period (it's already been one — this whole phase). This WS is the cleanup.

## Prerequisites

WS30–WS36 all merged. CI green with all migrated adapters running v2.

## In scope

### Step 1 — Delete v1 proto

```sh
git rm -r proto/criteria/v1
```

Update `Makefile` proto target to drop the v1 generation line. WS03 already did most of this — but if anything was left behind for migration ease, sweep it now.

### Step 2 — Delete v1 host wrappers

If `internal/adapter/` contains any "compat" helpers added for the migration period, remove them now.

### Step 3 — Update conformance test data

`internal/adapter/conformance/testdata/` — remove any v1 fixtures.

### Step 4 — Final grep sweep

```sh
! grep -rn 'criteria\.v1\b' --include='*.go' --include='*.proto' --include='*.yaml' .
! grep -rn 'AdapterPluginService' --include='*.go' --include='*.proto' .
! grep -rn '"criteria/internal/plugin"' --include='*.go' .
```

All three must return exit code 1 (no matches).

### Step 5 — CHANGELOG entry

Defer to WS39 cleanup gate. Leave a forward-pointer in this WS's PR description.

## Out of scope

- The proto-repo extraction — WS41.
- Documentation refresh — WS39.

## Behavior change

**No new behavior changes beyond what WS30–WS36 already delivered.** This is pure cleanup.

## Tests required

- `make ci` green.
- Three sanity greps return no matches.

## Exit criteria

- No v1 references in the tree (modulo `archived/` directories which preserve history).
- All conformance and integration tests pass.

## Files this workstream may modify

- Deletions across `proto/criteria/v1/`, `internal/adapter/`, conformance fixtures.
- `Makefile`.

## Files this workstream may NOT edit

- `README.md`, `PLAN.md`, etc. (WS39 cleanup gate).
- Other workstream files.
