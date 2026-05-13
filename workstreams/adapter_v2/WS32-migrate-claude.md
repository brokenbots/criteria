# WS32 — Migrate `claude` adapter to protocol v2

**Phase:** Adapter v2 · **Track:** Adapter migration · **Owner:** Workstream executor (in repo `criteria-typescript-adapter-claude`) · **Depends on:** [WS23](WS23-typescript-sdk-v2.md), [WS28](WS28-reusable-publish-action.md), [WS30](WS30-migrate-greeter.md) (validates the path). · **Unblocks:** [WS37](WS37-v1-protocol-code-removal.md).

## Context

`README.md` D56. The `claude` adapter is the canonical reference TS production adapter (≈378 LOC currently). Migrating it demonstrates the SDK's session-state helper, outcome validator, redacting logger, and secret-channel usage at production scale.

**All `process.env.X` reads must be rewritten to `helpers.secrets.get("X")`** (D69). Declare every secret in the manifest's `secrets:` list.

## Prerequisites

WS23 (TS SDK v2 RC), WS28 (publish action), WS30 (greeter sanity-check complete).

## In scope

### Step 1 — Bump SDK dep

`package.json`: `@criteria/adapter-sdk: ^2.0.0-rc.N`.

### Step 2 — Rewrite handler against v2

Current adapter maintains a `Map<sessionId, SessionState>` manually. Replace with `helpers.session`:

```ts
serve({
  name: "claude",
  version: "2.0.0",
  source_url: "https://github.com/criteria-adapters/claude",
  capabilities: ["multi_turn", "tool_calling", "structured_events"],
  platforms: ["linux/amd64", "linux/arm64", "darwin/arm64"],
  config_schema:  zodToSchema(ConfigSchema),
  input_schema:   zodToSchema(InputSchema),
  output_schema:  zodToSchema(OutputSchema),
  secrets: [
    { name: "ANTHROPIC_API_KEY", required: true, description: "Anthropic API key" },
    { name: "ANTHROPIC_BASE_URL", required: false, description: "Override base URL" },
  ],
  permissions: ["read_file", "write_file"],
  async openSession(req, helpers) {
    const apiKey = await helpers.secrets.get("ANTHROPIC_API_KEY");
    const baseUrl = await helpers.secrets.get("ANTHROPIC_BASE_URL") ?? "https://api.anthropic.com";
    const client = new Anthropic({ apiKey, baseURL: baseUrl });
    helpers.session.set("client", client);
    helpers.session.set("turns", 0);
  },
  async execute(req, helpers) {
    const client = helpers.session.get("client");
    // ... iterate turns, call tools via helpers.permission.request, etc.
    await helpers.outcomes.finalize("success", { reply: finalText });
  },
  async snapshot(sessionId) { /* serialize turns + tool history */ },
  async restore(sessionId, blob) { /* rehydrate */ },
});
```

### Step 3 — Permission correlation via SDK helper

Replace the manual `pendingPermissions: Map<string, { resolve, reject }>` pattern with `helpers.permission.request(tool, args)` — the SDK correlates IDs and gives a promise.

### Step 4 — Logging via RedactingLogger

Replace `console.log` / `console.error` with `helpers.log.stdout(...)` / `helpers.log.stderr(...)`. The Log stream + redaction registry handle the rest.

### Step 5 — Snapshot/restore

Implement `snapshot` and `restore` so a paused Claude session can resume with its conversation history intact.

### Step 6 — CI

Switch CI to invoke `criteria/publish-adapter@v1`.

### Step 7 — Tests

Update existing tests to use `TestHost`. Add coverage for the new helpers.

## Out of scope

- Other adapter migrations.
- Anthropic SDK upgrade as a separate concern.

## Behavior change

**Yes** for users:
- Lockfile must reference v2 of the adapter.
- API key now flows over the secret channel; env-var setting on the host still works (the host's `secrets.provider = "env"` resolves it) but the adapter binary itself cannot read process env.

## Tests required

- All adapter tests green.
- Conformance suite passes against this binary.
- A representative claude workflow runs end-to-end against the published artifact.

## Exit criteria

- `ghcr.io/criteria-adapters/claude:2.0.0` exists, signed, pulls and runs.

## Files this workstream may modify

- Everything in `criteria-typescript-adapter-claude`.

## Files this workstream may NOT edit

- Other adapters / SDKs / criteria.
- Other workstream files.
