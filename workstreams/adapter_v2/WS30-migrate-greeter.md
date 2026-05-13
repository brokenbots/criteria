# WS30 — Migrate `greeter` adapter to protocol v2

**Phase:** Adapter v2 · **Track:** Adapter migration · **Owner:** Workstream executor (in repo `criteria-typescript-adapter-greeter`) · **Depends on:** [WS23](WS23-typescript-sdk-v2.md), [WS28](WS28-reusable-publish-action.md). · **Unblocks:** later migrations validate the SDK + publish path against the simplest adapter first.

## Context

`README.md` D56. `greeter` is the smallest adapter (≈40 LOC). Migrating it first sanity-checks SDK ergonomics and the publish action before tackling the production adapters.

**All adapter-migration workstreams must replace any `process.env.X` (or equivalent) reads with `sdk.secrets.get("X")` (D69) and declare the corresponding entries in the adapter manifest's `secrets:` list.** The adapter binary's process environment is scrubbed by the sandbox, so any missed migration will fail loudly at first run. (Greeter has no secrets, so this is a no-op here — but the rule applies to every migration in WS31–WS36.)

## Prerequisites

WS23 (TS SDK v2 RC), WS28 (publish action available).

## In scope

### Step 1 — Update SDK dependency

`package.json`: bump `@criteria/adapter-sdk` to `2.0.0-rc.N`.

### Step 2 — Rewrite `index.ts` against v2 API

```ts
import { serve } from "@criteria/adapter-sdk";

serve({
  name: "greeter",
  version: "2.0.0",
  description: "Minimal hello-world adapter.",
  source_url: "https://github.com/criteria-adapters/greeter",
  capabilities: [],
  platforms: ["linux/amd64", "linux/arm64", "darwin/arm64"],
  config_schema: { fields: { recipient: { type: "string", required: false, description: "Who to greet" } } },
  input_schema:  { fields: { mood: { type: "string", required: false, description: "happy|sad|neutral" } } },
  output_schema: { fields: { greeting: { type: "string", required: true, description: "The composed greeting" } } },
  secrets: [],
  permissions: [],
  async execute(req, helpers) {
    const recipient = req.config.recipient ?? "world";
    const mood = req.input.mood ?? "happy";
    const greeting = mood === "happy" ? `Hello, ${recipient}!` : `Hi ${recipient}.`;
    await helpers.outcomes.finalize("greeted", { greeting });
  },
});
```

### Step 3 — CI workflow

Replace existing CI with `.github/workflows/publish.yml` invoking `criteria/publish-adapter@v1` with `sdk: typescript`, `with_image: false`.

### Step 4 — Tests

`tests/greeter.test.ts` using the WS23 `TestHost`:

```ts
import { TestHost } from "@criteria/adapter-sdk/testing";
test("greets happily by default", async () => {
  const host = new TestHost({ binary: "./out/adapter-linux-amd64" });
  await host.openSession({ config: { recipient: "team" } });
  const result = await host.execute({ step: "greet", input: { mood: "happy" } });
  expect(result.outcome).toBe("greeted");
  expect(result.outputs.greeting).toBe("Hello, team!");
});
```

### Step 5 — Tag and publish

Tag `v2.0.0` on a release commit; CI publishes to `ghcr.io/criteria-adapters/greeter:2.0.0` (or wherever the org is configured). Verify signature with `cosign verify --certificate-identity-regexp '.*' --certificate-oidc-issuer-regexp '.*' ghcr.io/criteria-adapters/greeter:2.0.0`.

## Out of scope

- Migration of other adapters — separate WSes.
- Any host-side change.

## Behavior change

**Yes** for users of the adapter:
- v1 of the adapter no longer works against criteria v2.
- v2 of the adapter requires criteria v2.
- Existing tests/workflows using `greeter` must update their lockfile.

## Tests required

- `bun test` green.
- Published artifact pulls + runs successfully.

## Exit criteria

- `ghcr.io/criteria-adapters/greeter:2.0.0` exists, signed.
- Conformance suite passes against this binary.

## Files this workstream may modify

- Everything in `criteria-typescript-adapter-greeter`.

## Files this workstream may NOT edit

- Other adapters / SDKs / criteria.
- Other workstream files.
