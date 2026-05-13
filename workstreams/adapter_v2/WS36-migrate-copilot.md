# WS36 — Migrate `copilot` adapter to protocol v2

**Phase:** Adapter v2 · **Track:** Adapter migration · **Owner:** Workstream executor (in `criteria-adapter-copilot` repo — verify language before kickoff) · **Depends on:** [WS23](WS23-typescript-sdk-v2.md) or [WS25](WS25-go-sdk-v1.md), [WS28](WS28-reusable-publish-action.md), [WS16](WS16-bidi-permission-stream.md). · **Unblocks:** [WS37](WS37-v1-protocol-code-removal.md).

## Context

The `copilot` adapter has the richest permission model and the most complex tool-call lifecycle of the seven adapters being migrated. It's last in the migration order because it stress-tests the entire stack — bidi permissions, snapshot/restore, secret channel, output schemas — all at production scale.

**Pre-WS check**: confirm copilot's current SDK language. The earlier explorer noted it lives at `/cmd/criteria-adapter-copilot/copilot.go` (Go inside the criteria repo). If still Go-in-tree, this WS first extracts it to its own repo, then migrates. If it's already TypeScript in a separate repo, this WS only migrates.

**All `os.Getenv` / `process.env.X` reads must be rewritten to `secrets.Get(...)` / `helpers.secrets.get(...)`** (D69).

## Prerequisites

WS23 or WS25 (depending on language), WS28, WS16, WS18 (snapshot/restore).

## In scope

### Step 1 — Language and repo confirmation

Verify copilot's language and repo. If in-tree Go: extract to `criteria-adapter-copilot` first, mirroring WS42's pattern. If already external TS: confirm repo and proceed.

### Step 2 — Migrate to v2 SDK shape (whichever language)

Same patterns as WS32/WS33. Special attention to:

- The **rich permission model** with aliases like `read_file → read`, `write_file → write` (currently handled in `internal/adapter/policy.go:41–46`). With v2's bidi stream + the SDK's permission correlator, the alias logic lives entirely on the host side (in the policy evaluator from WS16). The adapter just requests permissions with their canonical names.
- **GitHub auth secrets**: `GITHUB_TOKEN`, possibly OAuth flows. All flow through the secret channel.

### Step 3 — Bidi permission stress

Copilot agent flows can issue dozens of concurrent permission requests for file ops; the bidi stream + correlator make this clean. Stress-test in tests.

### Step 4 — Snapshot/restore

Long Copilot sessions are common. Implement snapshot/restore including the agent's conversation history + tool invocation log.

### Step 5 — CI + publish + tests

Standard pattern.

## Out of scope

- Host-side changes — all owned by other WSes.

## Behavior change

**Yes** for users (same shape as WS32; additionally, the alias logic for `read_file`/`write_file` moves from adapter to host, but users don't see this).

## Tests required

- All adapter tests green.
- Concurrent-permission + snapshot stress passes.
- Conformance suite passes.
- End-to-end with a real GitHub Copilot workflow runs successfully.

## Exit criteria

- `ghcr.io/criteria-adapters/copilot:2.0.0` (or wherever the org lands) exists, signed, pulls and runs.
- The last of the seven migrations — unblocks WS37.

## Files this workstream may modify

- Everything in `criteria-adapter-copilot` (post-extraction if applicable).
- If extracting from in-tree: a small follow-up PR in criteria deletes `cmd/criteria-adapter-copilot/` (similar to WS42 for shell).

## Files this workstream may NOT edit

- Other adapters / SDKs / host.
- Other workstream files.
