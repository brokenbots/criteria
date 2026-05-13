# WS24 — Python adapter SDK v2

**Phase:** Adapter v2 · **Track:** SDK · **Owner:** Workstream executor (in repo `criteria-python-adapter-sdk`) · **Depends on:** [WS02](WS02-protocol-v2-proto.md). · **Unblocks:** [WS21](WS21-sdk-serveremote.md), [WS27](WS27-starter-repos.md), Python adapter migrations.

## Context

Same shape as WS23 but for Python. `criteria-python-adapter-sdk` is refactored against protocol v2 with the same helper APIs, secret semantics, test-host harness, and library mode. Nuitka single-binary build retained.

## Prerequisites

WS02 merged.

## In scope

### Step 1 — Vendor v2 proto + generate Python bindings

`grpcio-tools` for code generation. Pinned to the v2 `.proto` digest from the criteria repo until WS41.

### Step 2 — `serve({...})` v2

```python
from criteria_adapter_sdk import serve

async def execute(req, helpers):
    api_key = await helpers.secrets.get("ANTHROPIC_API_KEY")
    # ...

await serve({
  "name": "claude",
  "version": "1.2.3",
  "source_url": "https://github.com/criteria-adapters/claude",
  "capabilities": ["multi_turn", "tool_calling"],
  "platforms": ["linux/amd64", "linux/arm64", "darwin/arm64"],
  "config_schema":  pydantic_to_schema(MyConfigModel),
  "input_schema":   pydantic_to_schema(MyInputModel),
  "output_schema":  pydantic_to_schema(MyOutputModel),
  "secrets":        [{"name": "ANTHROPIC_API_KEY", "required": True}],
  "permissions":    ["read_file"],
  "execute":        execute,
  # ...
})
```

### Step 3 — `serve_remote({...})`

See WS21.

### Step 4 — Helper APIs

Mirror WS23 with Pythonic naming (`spawn_env` instead of `spawnEnv`).

### Step 5 — `--emit-manifest` mode

Same behavior as WS23.

### Step 6 — `pydantic_to_schema(...)` helper

Reflect over Pydantic v2 `BaseModel.model_fields` to produce the SDK schema shape.

### Step 7 — TestHost harness

`from criteria_adapter_sdk.testing import TestHost`. Programmatic API + CLI `criteria-py-adapter-test`.

### Step 8 — Library mode

Same as WS23 — direct handler invocation without process spawn.

### Step 9 — README + Shelling-out section (D74)

### Step 10 — Build matrix

Nuitka onefile builds for `linux-x64`, `linux-arm64`, `darwin-arm64`. Add `windows-x64` ready for future.

## Out of scope

- Same as WS23.

## Behavior change

**Yes — full API refactor.**

## Tests required

- Full SDK test suite green.
- pypi pre-release `criteria-adapter-sdk==2.0.0rc1`.

## Exit criteria

- Package published to test PyPI; verified install works.
- A Python migration target (if any of the seven adapters are Python — verify in WS36) succeeds against this SDK.

## Files this workstream may modify

- Everything under `criteria-python-adapter-sdk/`.

## Files this workstream may NOT edit

- The criteria monorepo.
- Other workstream files.
