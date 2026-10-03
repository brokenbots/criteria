# mode: standalone
# Example: variant 2 — the mcp adapter consumed as a resource (CRI-172), the
# caller tool-calling an MCP tool served by the scripted echo fixture server.
# No network is involved: the server runs over stdio, launched by the mcp
# adapter.
#
# The echo fixture binary (cmd/criteria-adapter-mcp/testfixtures/echo-mcp) is
# built by `make example-adapter-tools` as bin/criteria-echo-mcp and resolved
# via PATH.
#
# KB-59: the mcp tool surface is dynamic (its tools/list is discovered at
# session open, so the adapter declares no input schema on purpose), which
# normally leaves tool arguments unvalidated at the seam. The named types
# here declare the echo tool's contract instead: the host validates both the
# caller's arguments (type.echo_request) and the callee's response
# (type.echo_response) at the adapter-tools boundary before either side
# reaches the mediator.
#
# Run with: make example-adapter-tools
# Or directly:
#   make build plugins
#   tmpdir=$(mktemp -d) && cp bin/criteria-adapter-noop bin/criteria-adapter-mcp bin/criteria-echo-mcp "$tmpdir/"
#   PATH="$tmpdir:$PATH" CRITERIA_ADAPTERS="$tmpdir" ./bin/criteria apply examples/adapter_tools/mcp_resource/mcp_resource.hcl

workflow {
  name          = "adapter_tools_mcp_resource"
  version       = "0.1"
  initial_state = "call"
  target_state  = "done"
}

# The echo tool's contract: arguments must be an object with the MCP tool
# name and an optional message; the response must carry a text payload.
type "echo_request" {
  schema = object({
    tool    = string
    message = optional(string)
  })
}

type "echo_response" {
  schema = object({
    text = optional(string)
  })
}

# Workflow-level allow_tools: the callee's own policy (ADR-0004 §4) — the
# permission surface of the mcp adapter's session, which gates the calls the
# mcp adapter itself makes against its MCP server.
permissions {
  allow_tools = ["echo", "structured"]
}

# The mcp adapter is consumed as a resource: dynamic_tools = true surfaces
# the fixture server's tools/list (echo, structured) at runtime. The
# `tool "echo"` contract types that one tool's call and response; the
# uncontracted tools keep the discovery-only surface.
adapter "mcp" "tools" {
  config {
    command = "criteria-echo-mcp"
  }
  dynamic_tools = true

  tool "echo" {
    in  = type.echo_request
    out = type.echo_response
  }
}

# The caller is the noop fixture running its tool-call caller mode.
adapter "noop" "caller" {}

step "call" {
  target = adapter.noop.caller

  # Step-level allow_tools grants the caller permission to call the mcp
  # adapter's discovered tool targets (the full target string, first
  # pattern to match wins, ADR-0004 §4).
  allow_tools = ["adapter.mcp.tools.tools.*"]

  input {
    tool_target = "adapter.mcp.tools.tools.echo"
    # The mcp callee's Execute contract takes the MCP tool name in "tool";
    # every other arg key becomes the MCP tools/call arguments.
    tool_args   = jsonencode({
      tool    = "echo"
      message = "hello from adapter tools"
    })
  }

  outcome "success" { next = state.done }
  outcome "failure" { next = state.failed }
}

state "done" { terminal = true }
state "failed" {
  terminal = true
  success  = false
}

# The mcp callee's outputs pass through typed under callee.* keys on the
# caller side: the echo tool's content lands as callee.outputs.text.
output "callee_outcome" {
  value = steps.call["callee.outcome"]
}
output "callee_outputs" {
  value = steps.call["callee.outputs"]
}