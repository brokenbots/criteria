# WS35 — Migrate `openai` adapter to protocol v2

**Phase:** Adapter v2 · **Track:** Adapter migration · **Owner:** Workstream executor (in repo `criteria-typescript-adapter-openai`) · **Depends on:** [WS23](WS23-typescript-sdk-v2.md), [WS28](WS28-reusable-publish-action.md). · **Unblocks:** [WS37](WS37-v1-protocol-code-removal.md).

## Context

The `openai` adapter is a second production TS adapter (different SDK from claude/claude-agent, similar shape). Migrating it verifies multi-provider patterns under v2.

**All `process.env.X` reads must be rewritten to `helpers.secrets.get("X")`** (D69).

## Prerequisites

WS23, WS28.

## In scope

### Step 1 — Migrate to v2 SDK shape

Same pattern as WS32. Specific to openai:

- Multiple environment variables: `OPENAI_API_KEY`, `OPENAI_BASE_URL`, `OPENAI_ORG_ID`, `OPENAI_PROJECT_ID`. All declared as secrets in the manifest (the URL and IDs are not strictly secrets but flowing them via the secret channel keeps the adapter binary's process env clean and avoids accidental leakage). Mark only `OPENAI_API_KEY` as `required: true`.
- `helpers.secrets.spawnEnv(...)` if the adapter ever shells out to the official `openai` CLI for a feature (currently doesn't; documented as the pattern to use if added).

### Step 2 — Tool use + outcome validation

Use the SDK's `helpers.outcomes` to enforce `allowed_outcomes` from the workflow.

### Step 3 — CI + publish + tests

Standard pattern.

## Out of scope

- Other adapter migrations.

## Behavior change

**Yes** for users (same as WS32).

## Tests required

- All adapter tests green.
- Conformance suite passes.

## Exit criteria

- `ghcr.io/criteria-adapters/openai:2.0.0` exists, signed, pulls and runs.

## Files this workstream may modify

- Everything in `criteria-typescript-adapter-openai`.

## Files this workstream may NOT edit

- Other adapters / SDKs / criteria.
- Other workstream files.
