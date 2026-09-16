# copilot_mcp_resource — a real agent tool-calls the mcp resource

Variant 3 of the adapter-tools story (CRI-181): instead of a scripted
fixture, the **copilot adapter** — a real agent — is the caller, and it
tool-calls the **mcp adapter** (consumed as a resource,
`dynamic_tools = true`) mid-task. The step's policy grants the mcp tool
surface to the agent; the agent fetches the data it needs through its
`adapter_tool` tool; the round trip comes back typed under `callee.*` on
the copilot step's outputs.

Unlike the noop variants this sample is **not self-contained**: it needs
the copilot CLI, a `criteria-adapter-copilot` build declaring the
`adapter_tools` capability (the CRI-178 caller), and a reachable model
provider. `make example-adapter-tools-copilot` skips with a reason when
the runtime is absent (or the installed adapter predates the CRI-178
capability), and runs for real otherwise.

## Requirements

- `criteria-adapter-copilot` installed in `$CRITERIA_ADAPTERS`
  (default `~/.local/criteria/adapters`), declaring `adapter_tools`.
- `criteria-echo-mcp` on `PATH` (the `make` target builds it into `bin/`
  and stages it).
- `copilot` CLI on `PATH` (or `CRITERIA_COPILOT_BIN` pointing at it).
- A BYOK provider reachable at `provider_base_url`, or the GitHub
  backend (see below).

## Provider configuration (BYOK vs GitHub backend)

- **BYOK**: set `provider_base_url` and `model`. `provider_type`
  defaults to `openai` (what Ollama/vLLM-style `/v1` endpoints speak);
  `provider_wire_api` defaults to `completions`. `provider_api_key` is
  optional for local providers — keep secrets out of source.
- **GitHub backend**: omit the `provider_*` keys entirely. The copilot
  CLI authenticates with the logged-in GitHub credentials
  (`COPILOT_GITHUB_TOKEN` / `GH_TOKEN` / `GITHUB_TOKEN`).

## Run

```sh
make example-adapter-tools-copilot   # runs for real, or skips with a reason
make validate                        # compiles the workflow (no runtime needed)
```

Directly:

```sh
make build plugins
go build -o bin/criteria-echo-mcp ./cmd/criteria-adapter-mcp/testfixtures/echo-mcp
tmpdir=$(mktemp -d) && cp bin/criteria-adapter-mcp bin/criteria-echo-mcp "$tmpdir/"
PATH="$tmpdir:$PATH" CRITERIA_ADAPTERS="$tmpdir" ./bin/criteria apply \
  examples/adapter_tools/copilot_mcp_resource/copilot_mcp_resource.hcl
```

## When to use an agent caller vs the noop caller

Use the **noop caller** (variants 1–2) when you want a deterministic,
self-contained smoke of the tool-call mechanics: the call happens
immediately, the policy evaluation and the typed round trip are the whole
story, and CI can assert it byte-for-byte.

Use the **agent caller** (this variant) when you want to demonstrate —
or verify locally before wiring for real — that a *real* assistant can
be trusted with the same mechanism: the model must recognize that the
only way to satisfy the task is the granted adapter tool, form the §2
target and arguments itself, consume the typed result, and route its own
outcome on it. Anything the adapter exposes to the copilot session
becomes part of the agent's toolset, so this is also the sample to copy
when an agent should enrich its work with adapter-side data or actions
mid-task.

## What the agent sees

The host surfaces each granted callee tool to the copilot session through
the adapter's `adapter_tool` tool. The agent's tool call carries the §2
target (`adapter.mcp.tools.tools.echo`) plus the MCP tool arguments
(`{"tool": "echo", "message": "..."}` — the mcp callee takes the MCP tool
name under `tool`, every other key becomes an MCP argument). The typed
result it receives is the callee's result: the echo tool's text content —
the JSON-encoded argument map — arrives as the tool result text, and the
same payload passes through typed on the step as `callee.outcome` /
`callee.outputs.text` for the harness to assert.

## Security note

The call is gated twice, on two distinct surfaces (ADR-0004 §4):

1. **The caller step's policy** — `allow_tools = ["adapter.mcp.tools.tools.*"]`
   on the copilot step authorizes the agent's call of that target. A call
   matching no grant is denied before the callee ever runs; the agent only
   gets the tool because this step grants it.
2. **The callee's own `permissions { allow_tools }`** — the mcp adapter's
   session policy (`["echo", "structured"]` here), gating the calls the
   mcp adapter itself makes against its MCP server. The callee's grants
   are its own: granting the agent a target never widens what the callee
   may do inside its session.

Keep both minimal: the glob here is deliberately scoped to the mcp
callee's tool surface, and the workflow-level list to the two tools the
fixture server actually serves.