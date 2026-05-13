# WS23 — TypeScript adapter SDK v2

**Phase:** Adapter v2 · **Track:** SDK · **Owner:** Workstream executor (in repo `criteria-typescript-adapter-sdk`) · **Depends on:** [WS02](WS02-protocol-v2-proto.md). · **Unblocks:** [WS21](WS21-sdk-serveremote.md), [WS27](WS27-starter-repos.md), all TS adapter migrations (WS30, WS32–WS35, WS36 if applicable).

## Context

`README.md` D44–D45 and D69–D71. Existing `criteria-typescript-adapter-sdk` is refactored against protocol v2 with new helpers, secret-channel-only `secrets.get`, redaction-safe `spawnEnv`, manifest emitter, test-host harness, and library-mode entry. Bun single-binary build retained.

This workstream lands in the **separate `criteria-typescript-adapter-sdk` repository**, not in the criteria monorepo. A companion PR / cross-repo reference is part of the WS40 release gate.

## Prerequisites

WS02 merged (Go proto bindings exist; TS proto bindings are generated in this WS from the same `.proto` file vendored or pinned by digest).

## In scope

### Step 1 — Vendor v2 proto + generate TS bindings

Add the v2 `.proto` files to the SDK repo (pinned by digest from the criteria repo until WS41 extracts the proto into its own repo). Use `protoc-gen-ts` + `@grpc/grpc-js`. Build script regenerates on every commit.

### Step 2 — `serve({...})` v2

```ts
import { serve } from "@criteria/adapter-sdk";

serve({
  name: "claude",
  version: "1.2.3",
  description: "...",
  source_url: "https://github.com/criteria-adapters/claude",
  capabilities: ["multi_turn", "tool_calling"],
  platforms: ["linux/amd64", "linux/arm64", "darwin/arm64"],
  config_schema:  zodToSchema(MyConfigZodSchema),
  input_schema:   zodToSchema(MyInputZodSchema),
  output_schema:  zodToSchema(MyOutputZodSchema),  // NEW
  secrets:        [{ name: "ANTHROPIC_API_KEY", required: true, description: "..." }],
  permissions:    ["read_file", "write_file"],
  compatible_environments: undefined,  // default = any
  async openSession(req, helpers) { ... },
  async execute(req, helpers) { ... },
  async closeSession(req) { ... },
  async snapshot(sessionId) { ... },
  async restore(sessionId, blob) { ... },
  async inspect(sessionId) { ... },
});
```

`helpers` is the new SDK API surface — see Step 4.

### Step 3 — `serveRemote({...})`

See WS21. Same handler shape as `serve`, but dials out instead of listening.

### Step 4 — Helper APIs

Each adapter today reimplements session state maps, outcome validation, permission correlation. SDK helpers absorb these:

```ts
helpers.session          // SessionStore — per-session keyed get/set
helpers.outcomes         // OutcomeValidator — validate string against allowed_outcomes
helpers.permission       // PermissionCorrelator — request(permission, details) → Promise<decision>
helpers.log              // RedactingLogger — log.stdout(...), log.stderr(...), log.adapterEvent(...)
helpers.secrets          // secrets.get(name) — secret-channel-only, no env-var fallback (D69)
helpers.secrets.spawnEnv(["ANTHROPIC_API_KEY"]) // returns env map for child_process.spawn (D75)
helpers.timestamps       // monotonic timestamps for events
```

### Step 5 — `--emit-manifest` mode

Adding a CLI flag handler in the SDK's serve loop: when the adapter binary is invoked with `--emit-manifest`, it prints `adapter.yaml` (matching WS05's schema) to stdout and exits 0 without starting the gRPC server. WS28's publish action uses this to extract the manifest.

### Step 6 — `zodToSchema(...)` helper

Convert a Zod schema to the SDK schema shape (matching `manifest.SchemaField`). Reflection over `ZodSchema._def`. Tests cover scalar types + nested objects + optional/required handling.

### Step 7 — TestHost harness

`@criteria/adapter-sdk/testing` exposes `TestHost`:

```ts
import { TestHost } from "@criteria/adapter-sdk/testing";

const host = new TestHost({
  binary: "./out/adapter",
  // OR  binary: { module: import("./src/index"), libraryMode: true },
});
await host.openSession({ config: { ... }, secrets: { ANTHROPIC_API_KEY: "..." } });
const events = await host.execute({ step: "go", input: { ... }, secret_inputs: { ... } });
expect(events).toMatchSnapshot();
```

Plus a CLI binary `criteria-ts-adapter-test` that consumes a YAML test file. CLI lands in WS27's starter or as a separate binary in this SDK repo.

### Step 8 — Library mode (D71)

Optional fast-path: directly import the adapter's handler functions for unit testing without process/IPC overhead. Documented as the "logic only" test path.

### Step 9 — README

Open with the **Shelling out: passing secrets safely** section per D74. Use `spawnEnv` example.

### Step 10 — Build matrix

Bun `--compile` targets retained: `linux-x64`, `linux-arm64`, `darwin-arm64`. Add a `windows-x64` target ready for when WS40-windows lifts the host non-goal.

## Out of scope

- The `serveRemote` implementation — separate file in WS21 but lands in this same repo.
- Conformance harness extension — WS26.
- Adapter migrations using this SDK — WS30, WS32–WS35.

## Behavior change

**Yes — entire SDK API refactor.** Adapters built against the old SDK will not work; each adapter is migrated in WS30/WS32–WS35.

## Tests required

- Full SDK test suite green.
- Build all platform targets in CI on each PR.

## Exit criteria

- npm package `@criteria/adapter-sdk@2.0.0-rc.N` published to a pre-release tag.
- Greeter migration (WS30) runs successfully against this SDK.

## Files this workstream may modify

- Everything under `criteria-typescript-adapter-sdk/`.

## Files this workstream may NOT edit

- The criteria monorepo (separate workstreams).
- Other workstream files.
