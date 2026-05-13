# WS34 — Migrate `codex` adapter to protocol v2

**Phase:** Adapter v2 · **Track:** Adapter migration · **Owner:** Workstream executor (in repo `criteria-typescript-adapter-codex`) · **Depends on:** [WS23](WS23-typescript-sdk-v2.md), [WS28](WS28-reusable-publish-action.md). · **Unblocks:** [WS37](WS37-v1-protocol-code-removal.md).

## Context

The `codex` adapter exercises **streaming thread events** and **Zod schema generation**. Migrating it verifies the SDK's edge cases around streaming and schema reflection.

**All `process.env.X` reads must be rewritten to `helpers.secrets.get("X")`** (D69).

## Prerequisites

WS23, WS28.

## In scope

### Step 1 — Migrate to v2 SDK shape

Same pattern as WS32. Particular attention to:

- Codex emits many small structured events during execution; use `helpers.log.adapterEvent(...)` rather than ad-hoc log lines.
- Replace the bespoke `Zod` → schema conversion in the current adapter with `zodToSchema(...)` from the SDK.

### Step 2 — Streaming output

Codex streams partial thread events. Use the SDK's streaming sender helper to emit them incrementally; final outcome at end of stream.

### Step 3 — Snapshot/restore (optional for codex)

Implement if codex sessions are long enough to benefit. Otherwise document that snapshot/restore aborts a codex run.

### Step 4 — CI + publish + tests

Standard pattern.

## Out of scope

- Other adapter migrations.

## Behavior change

**Yes** for users (same as WS32).

## Tests required

- All adapter tests green.
- Streaming edge cases (large message, many small messages) tested.
- Conformance suite passes.

## Exit criteria

- `ghcr.io/criteria-adapters/codex:2.0.0` exists, signed, pulls and runs.

## Files this workstream may modify

- Everything in `criteria-typescript-adapter-codex`.

## Files this workstream may NOT edit

- Other adapters / SDKs / criteria.
- Other workstream files.
