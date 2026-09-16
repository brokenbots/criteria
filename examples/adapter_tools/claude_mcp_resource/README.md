# claude_mcp_resource — a real Claude Code agent tool-calls the mcp resource

Variant 4 of the adapter-tools story (CRI-182, the CRI-181 twin for the
Claude side): instead of a scripted fixture, the **claude-agent adapter** —
a real Claude Code CLI agent — is the caller, and it tool-calls the **mcp
adapter** (consumed as a resource, `dynamic_tools = true`) mid-task. The
step's policy grants the mcp tool surface to the agent; the agent fetches
the data it needs through its `adapter_tool` tool; the round trip comes
back typed under `callee.*` on the claude step's outputs.

The `adapter "mcp" "tools"` block is byte-identical to the CRI-181 sample's
([copilot_mcp_resource](../copilot_mcp_resource/)): the two real-agent
samples share the fixture contract so it stays portable.

Unlike the noop variants this sample is **not self-contained**: it needs
the Claude Code CLI, Anthropic credentials (or a logged-in CLI), and a
`criteria-adapter-claude-agent` build declaring the `adapter_tools`
capability (the CRI-178 caller, shipped by CRI-180).
`make example-adapter-tools-claude` skips with a reason when the runtime is
absent (or the installed adapter predates the CRI-178 capability), and runs
for real otherwise.

## Requirements

- Claude Code CLI installed and authenticated: the adapter resolves it per
  the claude-agent repo's `resolveClaudeExecutable` (`index.ts`) — the
  `claude_executable` adapter-config field when set, otherwise a `claude`
  executable on `PATH`.
- `ANTHROPIC_API_KEY` (or `ANTHROPIC_AUTH_TOKEN`) available through the
  environment / Criteria secret store, or the CLI's own stored credentials
  (`claude` login / OAuth).
- `criteria-adapter-claude-agent` discoverable by the host: the
  bun-compiled plugin binary must be in `$CRITERIA_ADAPTERS` or
  `$CRITERIA_HOME/adapters` (default `~/.local/criteria/adapters`) —
  Criteria never resolves adapter binaries from `PATH`. Build and install
  per the claude-agent repo's README:
  ```sh
  bun install && bun run build        # -> bin/criteria-adapter-claude-agent
  bun run build:linux-x64             # platform-targeted: bun scripts/build.ts --target=bun-linux-x64
  bun run binary:install              # copies the binary into the adapters dir
  ```
  (The same `bun scripts/build.ts` flow builds the adapter inside
  `images/remote-adapters/Dockerfile.claude-agent`.)
- `criteria-echo-mcp` on `PATH` (the `make` target builds it into `bin/`
  and stages it) — the mcp adapter spawns it as its MCP server over stdio.

For local iteration without installing, `criteria adapter dev <binary>`
registers a binary directly.

## Run

```sh
make example-adapter-tools-claude   # runs for real, or skips with a reason
make validate                       # compiles the workflow (no runtime needed)
```

Directly:

```sh
make build plugins
go build -o bin/criteria-echo-mcp ./cmd/criteria-adapter-mcp/testfixtures/echo-mcp
tmpdir=$(mktemp -d) && cp bin/criteria-adapter-mcp bin/criteria-echo-mcp "$tmpdir/"
# plus criteria-adapter-claude-agent in "$tmpdir" (or an adapters root)
PATH="$tmpdir:$PATH" CRITERIA_ADAPTERS="$tmpdir" ./bin/criteria apply \
  examples/adapter_tools/claude_mcp_resource/claude_mcp_resource.hcl
```

## What the agent sees — the typed callee result

The typed round trip (CRI-165 convention) is identical to the CRI-181
sample: the callee's result passes back under `callee.*` keys on the
caller step's outputs, whatever the caller adapter is —

- `callee.outcome` — the mcp callee's own outcome (`"success"`).
- `callee.outputs.text` — the echo tool's content text (the JSON-encoded
  argument map), because the mcp adapter joins the tool's text content into
  its `text` output.

The harness asserts exactly these keys (the `output` blocks at the bottom
of the workflow expose `callee_outcome` and `callee_text`), and the event
stream carries the host-side `tool.call` / `tool.call_result` events
attributed to the mcp target `adapter.mcp.tools.tools.echo`.

## Tool naming on the claude side

Inside the Claude Code session the agent's toolset is the Claude Code
preset plus the adapter's own MCP server, so the agent internally calls:

```
mcp__<server>__adapter_tool
```

That naming is **invisible to the workflow**. The mangled `mcp__…` name is
purely a Claude-SDK-side surface detail of how the adapter registers its
caller tool in the session; it never crosses the Criteria wire. What the
workflow sees — and what `allow_tools` authorizes — is only the
`adapter_tool` call with the Criteria target string inside it
(`adapter.mcp.tools.tools.echo` here), matching the §2 target grammar and
the step's grant (`adapter.mcp.tools.tools.*`, the full-target-string glob
form of ADR-0004 §4). A copilot agent and a Claude agent making the same
call produce byte-identical host-side events; swapping the caller adapter
changes nothing about the callee contract.

## Security note

The call is gated twice, on two distinct surfaces (ADR-0004 §4), exactly as
in the CRI-181 sample:

1. **The caller step's policy** — `allow_tools = ["adapter.mcp.tools.tools.*"]`
   on the claude step authorizes the agent's call of that target. A call
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

## When to use an agent caller vs the noop caller

Use the **noop caller** (variants 1–2) when you want a deterministic,
self-contained smoke of the tool-call mechanics that CI can assert
byte-for-byte. Use an **agent caller** (this variant or the CRI-181
copilot twin) when you want to demonstrate that a *real* assistant can be
trusted with the same mechanism — the model must recognize that the only
way to satisfy the task is the granted adapter tool, form the §2 target
and arguments itself, consume the typed result, and route its own outcome
on it. This is also the sample to copy when a Claude Code agent should
enrich its work with adapter-side data or actions mid-task.

## References

- CRI-180 — the claude-agent adapter's `adapter_tool` caller capability.
- CRI-181 — the copilot twin of this sample
  ([copilot_mcp_resource](../copilot_mcp_resource/)).
- CRI-178 — the `adapter_tools` capability gate the make target checks.
- [ADR-0004](../../../docs/adrs/ADR-0004-adapter-tools.md) — the
  adapter-tools design: §2 target naming, §4 caller-side grants, §5 return
  semantics.