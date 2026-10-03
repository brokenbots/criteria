# mode: standalone
# Example: the mcp adapter as a DIRECT step target with a typed input block
# (KB-59). The adapter is dynamic (dynamic_tools = true, no declared step
# input schema), so the static unknown-field check has nothing to check
# against — the declared `tool "echo"` contract is the schema: the step's
# input keys must satisfy type.echo_request (the MCP tool name stays on the
# "tool" key; every other key becomes the MCP tools/call arguments).
#
# The echo fixture binary (cmd/criteria-adapter-mcp/testfixtures/echo-mcp) is
# built by `make example-adapter-tools` as bin/criteria-echo-mcp and resolved
# via PATH.
#
# Run directly:
#   make build plugins
#   tmpdir=$(mktemp -d) && cp bin/criteria-adapter-mcp bin/criteria-echo-mcp "$tmpdir/"
#   PATH="$tmpdir:$PATH" CRITERIA_ADAPTERS="$tmpdir" ./bin/criteria apply examples/adapter_tools/mcp_typed_direct/mcp_typed_direct.hcl

workflow {
  name          = "adapter_tools_mcp_typed_direct"
  version       = "0.1"
  initial_state = "echo"
  target_state  = "done"
}

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

# The mcp adapter's own permission surface: awaiting the echo tool call the
# bridge makes (ADR-0004 §4).
permissions {
  allow_tools = ["echo"]
}

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

# A DIRECT step target: no caller hop, the step input IS the adapter's
# Execute input. The declared contract types it — an unknown key or a
# mistyped value is a compile error (golden diagnostics), so the runtime
# only ever sees contract-valid arguments.
step "echo" {
  target = adapter.mcp.tools

  input {
    tool    = "echo"
    message = "direct typed echo"
  }

  outcome "success" { next = state.done }
  outcome "failure" { next = state.failed }
}

state "done" { terminal = true }
state "failed" {
  terminal = true
  success  = false
}

output "echo_text" {
  value = steps.echo.text
}