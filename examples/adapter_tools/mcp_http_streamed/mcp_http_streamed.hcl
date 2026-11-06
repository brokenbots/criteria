# mode: standalone
# Example: the mcp adapter against an HTTP-transport (Streamable HTTP) MCP
# server. The adapter POSTs one JSON-RPC 2.0 envelope per exchange to the
# configured URL with the transport's standard Mcp-Method / Mcp-Name headers,
# accepts application/json responses and SSE response streams (progress
# notifications route to the call in flight), and carries no session or
# resumability state — a broken response stream loses the in-flight call and
# a re-issued call is a brand-new request.
#
# The echo fixture binary (cmd/criteria-adapter-mcp/testfixtures/echo-mcp) is
# built by `make example-adapter-tools` as bin/criteria-echo-mcp; in HTTP mode
# it serves the stateless core on 127.0.0.1:48844 for this workflow.
#
# Run directly:
#   make build plugins
#   tmpdir=$(mktemp -d) && cp bin/criteria-adapter-mcp bin/criteria-echo-mcp "$tmpdir/"
#   MCP_HTTP_LISTEN=127.0.0.1:48844 "$tmpdir/criteria-echo-mcp" &
#   sleep 1
#   PATH="$tmpdir:$PATH" CRITERIA_ADAPTERS="$tmpdir" ./bin/criteria apply examples/adapter_tools/mcp_http_streamed/mcp_http_streamed.hcl
#   kill %1

workflow {
  name          = "adapter_tools_mcp_http_streamed"
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

permissions {
  allow_tools = ["echo"]
}

adapter "mcp" "tools" {
  config {
    transport = "http"
    url       = "http://127.0.0.1:48844"
    headers   = "X-Tenant: acme"
    timeout   = "5s"
  }
  dynamic_tools = true

  tool "echo" {
    in  = type.echo_request
    out = type.echo_response
  }
}

# A DIRECT step target over the HTTP transport: the step input is the
# adapter's Execute input, the transport POSTs it to the MCP server, and
# the SSE-delivered result maps into the declared tool contract.
step "echo" {
  target = adapter.mcp.tools

  input {
    tool    = "echo"
    message = "http streamed echo"
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
