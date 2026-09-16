# Adapter tools example

Two runnable variants of the M8.3 adapter-tools story (CRI-176): a caller
adapter tool-calls a callee mid-step, and the callee's result passes back
typed under `callee.*` keys. Both variants use the in-tree noop fixture as
the caller and stay self-contained — no external OCI pull, no network.

| File | Variant | Callee |
|---|---|---|
| [`noop_passthrough/noop_passthrough.hcl`](noop_passthrough/noop_passthrough.hcl) | 1 | The noop fixture as a data-ish callee in its outputs-passthrough mode (CRI-165). |
| [`mcp_resource/mcp_resource.hcl`](mcp_resource/mcp_resource.hcl) | 2 | The mcp adapter as a resource, driven by the scripted echo MCP fixture server over stdio (CRI-172). |

## How a caller tool-call works

The caller step's `input` block names a tool call instead of adapter work:

- `tool_target` — the §2 target grammar, `adapter.<type>.<name>.tools[.<tool>]`.
- `tool_args` — a JSON object rendered as the callee's step input.

The caller's adapter issues the call as a `permission.request` payload of
kind `adapter_tool`; the host pauses the step, evaluates the call against the
step's policy, runs the callee in its own session (its own environment, its
own `allow_tools`), and delivers the typed result back. The caller's
outcome routing then finishes the run — the callee never enters the FSM.

## The tools list

- Variant 1 declares `echo_data` on the callee's `adapter` block; the caller
  references it at compile time as
  `adapter.noop.callee.tools.echo_data` (`adapter.<type>.<name>.tools.<tool>`).
- Variant 2 uses `dynamic_tools = true`: the mcp adapter consumes the fixture
  server's `tools/list` at runtime, which presents `echo` and `structured`;
  targets look like `adapter.mcp.tools.tools.echo`.

## Expected outputs

Variant 1 (`noop_passthrough/noop_passthrough.hcl`):

- `callee_outcome` = `"success"` — the callee's own outcome.
- `callee_outputs` = `{"rows": 2, "items": ["alpha", "beta"], "note": "echoed back by the callee"}` — the callee's typed outputs, passed through unchanged.
- `callee_rows` = `2` — nested attribute access into the typed object.

Variant 2 (`mcp_resource/mcp_resource.hcl`):

- `callee_outcome` = `"success"`.
- `callee_outputs` = `{"text": "{\"message\":\"hello from adapter tools\"}"}` — the mcp adapter maps the MCP tool result into its outputs (`text` joins the tool's text content; `structured` appears when the server returns a `structuredContent` payload, which the `structured` tool does).

The mcp callee's Execute contract takes the MCP tool name under the `tool`
input key (the caller passes it in `tool_args`); every other arg key becomes
the MCP `tools/call` arguments.

A failed round-trip (denied call, unknown tool, callee crash) does not fail
the run: the caller reports the failure outcome with `callee.error` and the
run routes on the caller's own outcomes. A successful callee execution with
a callee-reported failure outcome passes through under `callee.outcome` /
`callee.outputs` with no `callee.error`.

## allow_tools interplay

Tool-call authorization has two distinct surfaces (ADR-0004 §4):

1. **Step-level grants on the caller** — `tools` (declared tool refs, variant
   1) or `allow_tools` (glob patterns on the full target string, variant 2).
   These say what the caller may invoke. First matching pattern wins;
   a call matching neither is denied.
2. **Workflow-level `permissions { allow_tools }`** — the callee's own
   policy, applied to the callee's synthetic step in its own session. The
   callee runs with its own environment and its own `allow_tools`, which is
   why variant 2 grants `["echo", "structured"]`: those are the calls the
   mcp adapter itself makes against its MCP server.

The two compose: the caller needs surface 1 to issue the call, and the
callee needs surface 2 for whatever it does next. See
[`internal/adapterhost/tool_call.go`](../../internal/adapterhost/tool_call.go)
and ADR-0004 for the full gate order.

## Run

```sh
make example-adapter-tools   # runs both variants with assertions (CI smoke)
make validate                # compiles every example, including this one
```

Both variants run without a server (no `wait`/`approval` nodes).