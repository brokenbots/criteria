# mode: standalone
# Example: variant 3 — a REAL agent (the copilot adapter) tool-calls the mcp
# adapter consumed as a resource, mid-task (CRI-181). The copilot step's
# policy grants the mcp adapter's tool surface, the agent calls it through
# its adapter_tool tool while doing its normal work, and the round trip
# comes back typed under callee.* on the copilot step's outputs.
#
# Unlike the noop variants, this sample is NOT self-contained: it needs the
# copilot CLI plus a model provider.
#
# Provider configuration (BYOK vs GitHub backend):
#   • BYOK: set provider_base_url (optionally provider_api_key — optional
#     for local providers like Ollama; prefer keeping secrets out of source)
#     and model. `provider_type` defaults to openai; `provider_wire_api`
#     defaults to completions.
#   • GitHub backend: omit the provider_* keys entirely — the copilot CLI
#     authenticates with the logged-in GitHub credentials
#     (COPILOT_GITHUB_TOKEN / GH_TOKEN / GITHUB_TOKEN), and `model` names
#     any model the backend serves.
#
# Requirements:
#   • criteria-adapter-copilot installed in $CRITERIA_ADAPTERS
#     (default ~/.local/criteria/adapters) — must be a build declaring the
#     adapter_tools capability (CRI-178 caller)
#   • criteria-echo-mcp on PATH (built by make example-adapter-tools-copilot
#     as bin/criteria-echo-mcp)
#   • copilot CLI on PATH (or pointed at via CRITERIA_COPILOT_BIN)
#   • a BYOK provider reachable at provider_base_url
#
# Run with: make example-adapter-tools-copilot
# Or directly:
#   make build plugins
#   go build -o bin/criteria-echo-mcp ./cmd/criteria-adapter-mcp/testfixtures/echo-mcp
#   tmpdir=$(mktemp -d) && cp bin/criteria-adapter-mcp bin/criteria-echo-mcp "$tmpdir/"
#   PATH="$tmpdir:$PATH" CRITERIA_ADAPTERS="$tmpdir" ./bin/criteria apply \
#     examples/adapter_tools/copilot_mcp_resource/copilot_mcp_resource.hcl

workflow {
  name          = "adapter_tools_copilot_mcp_resource"
  version       = "0.1"
  initial_state = "call"
  target_state  = "done"
  policy {
    max_total_steps = 20
  }
}

# Workflow-level allow_tools: the callee's own policy (ADR-0004 §4) — the
# permission surface of the mcp adapter's session, which gates the calls the
# mcp adapter itself makes against its MCP server.
permissions {
  allow_tools = ["echo", "structured"]
}

# The mcp adapter is consumed as a resource: dynamic_tools = true surfaces
# the fixture server's tools/list (echo, structured) at runtime.
adapter "mcp" "tools" {
  config {
    command = "criteria-echo-mcp"
  }
  dynamic_tools = true
}

# The caller is a real copilot agent. provider_base_url enables BYOK mode;
# omit the provider_* keys (and keep model) to use the GitHub backend
# instead, authenticated via the GitHub credentials described above.
adapter "copilot" "agent" {
  config {
    model             = "glm-5.2:cloud"
    provider_base_url = "http://192.168.17.116:11434/v1"
    provider_type     = "openai"
    max_turns         = 6
  }
}

step "call" {
  target = adapter.copilot.agent

  # Step-level grant (ADR-0004 §4): the full-target-string glob form, on
  # purpose. The mcp callee's tool surface is discovered at runtime
  # (dynamic_tools), so there is nothing static to reference, and a glob
  # exercises the first-match-wins policy. It also keeps validation free of
  # the static vocabulary check, which only applies to literal tool names.
  allow_tools = ["adapter.mcp.tools.tools.*"]

  input {
    prompt = <<-EOT
      You are finalizing release bookkeeping for adapter tools wave 2.5.
      The release code is not guessable: the only way to obtain it is to
      call the adapter_tool tool, exactly once, with:

        target = "adapter.mcp.tools.tools.echo"
        arguments = { "tool": "echo", "message": "release-code for build 2.5" }

      The release code comes back inside the tool result's text: read it
      from the tool result. Do not guess or invent the release code, and
      do not use any other tool.
      After you have read the tool result, report the release code and end
      your final line with exactly: RESULT: success
      If the tool call fails, end your final line with exactly: RESULT: failure
    EOT
  }

  outcome "success" { next = state.done }
  outcome "failure" { next = state.failed }
}

state "done" { terminal = true }
state "failed" {
  terminal = true
  success  = false
}

# The mcp callee's result passes through typed under callee.* keys on the
# caller side (CRI-165 convention): the echo tool's content text lands as
# callee.outputs.text, so the harness can assert the round trip.
output "callee_outcome" {
  value = steps.call["callee.outcome"]
}
output "callee_text" {
  value = steps.call["callee.outputs"].text
}