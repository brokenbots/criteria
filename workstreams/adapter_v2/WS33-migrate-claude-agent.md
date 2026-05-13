# WS33 — Migrate `claude-agent` adapter to protocol v2

**Phase:** Adapter v2 · **Track:** Adapter migration · **Owner:** Workstream executor (in repo `criteria-typescript-adapter-claude-agent`) · **Depends on:** [WS23](WS23-typescript-sdk-v2.md), [WS28](WS28-reusable-publish-action.md), [WS16](WS16-bidi-permission-stream.md). · **Unblocks:** [WS37](WS37-v1-protocol-code-removal.md).

## Context

`README.md` D56. The `claude-agent` adapter exercises the **bidirectional permission stream** (D24, WS16) at production scale — agent flows produce many concurrent permission requests as tool invocations fan out. Migrating this adapter is the canonical stress test of WS16.

**All `process.env.X` reads must be rewritten to `helpers.secrets.get("X")`** (D69).

## Prerequisites

WS23, WS28, WS16 (host bidi permission stream + audit log).

## In scope

### Step 1 — Migrate to v2 SDK shape (same pattern as WS32)

Bump SDK dep; refactor against `serve({...})`; use `helpers.session` / `helpers.outcomes` / `helpers.log` / `helpers.secrets`.

### Step 2 — Use `helpers.permission.request(...)` for tool gates

This is where claude-agent earns its keep. The agent's tool invocation loop fires many permissions concurrently:

```ts
const decisions = await Promise.all([
  helpers.permission.request({ tool: "read_file", args: { path: a } }),
  helpers.permission.request({ tool: "read_file", args: { path: b } }),
  helpers.permission.request({ tool: "write_file", args: { path: c } }),
]);
for (const dec of decisions) {
  if (dec.decision === "deny") { ... }
}
```

The SDK's correlator (WS23) keys decisions by request ID; the underlying bidi stream lets them happen in parallel.

### Step 3 — Snapshot/restore

Long agent sessions benefit most from `snapshot`. Implement so paused agent runs can resume with conversation + tool history intact, including pending permission requests (the host replays answered ones automatically per WS16).

### Step 4 — Tests

Stress test: a workflow that triggers 50 concurrent permission requests; verify they all decide correctly under contention.

### Step 5 — CI + publish

Standard pattern; uses `criteria/publish-adapter@v1`.

## Out of scope

- Other adapter migrations.

## Behavior change

**Yes** for users (same as WS32).

## Tests required

- All adapter tests green.
- Concurrent-permission stress test passes.
- Conformance suite passes.

## Exit criteria

- `ghcr.io/criteria-adapters/claude-agent:2.0.0` exists, signed, pulls and runs.

## Files this workstream may modify

- Everything in `criteria-typescript-adapter-claude-agent`.

## Files this workstream may NOT edit

- Other adapters / SDKs / criteria.
- Other workstream files.
