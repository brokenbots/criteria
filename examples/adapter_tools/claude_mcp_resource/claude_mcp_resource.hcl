# mode: standalone
# Example: variant 4 — a REAL agent (the claude-agent adapter, i.e. the
# Claude Code CLI) tool-calls the mcp adapter consumed as a resource,
# mid-task (CRI-182; the CRI-181 twin for the Claude side). The claude
# step's policy grants the mcp adapter's tool surface, the agent calls it
# through its adapter_tool tool while doing its normal work, and the round
# trip comes back typed under callee.* on the claude step's outputs.
#
# Unlike the noop variants, this sample is NOT self-contained: it needs the
# Claude Code CLI plus Anthropic credentials.
#
# Runtime requirements:
#   • The Claude Code CLI installed. The adapter resolves it per the
#     claude-agent repo's resolveClaudeExecutable (index.ts): the
#     claude_executable adapter-config field when set, otherwise an
#     executable named `claude` found on PATH.
#   • ANTHROPIC_API_KEY (or ANTHROPIC_AUTH_TOKEN) available via the
#     environment / secret store, or stored Claude Code credentials
#     (claude login / OAuth) on the machine.
#   • The bun-compiled plugin binary criteria-adapter-claude-agent installed
#     where the host discovers adapters: $CRITERIA_ADAPTERS or
#     $CRITERIA_HOME/adapters (default ~/.local/criteria/adapters). Discovery
#     never consults PATH for adapter binaries. Build/install per the
#     claude-agent repo's README:
#       bun install && bun run build       # -> bin/criteria-adapter-claude-agent
#       bun run build:linux-x64            # platform-targeted: bun scripts/build.ts --target=bun-linux-x64
#       bun run binary:install             # copies the binary into the adapters dir
#     (images/remote-adapters/Dockerfile.claude-agent builds it the same way
#     via scripts/build.ts with an explicit --target/--outfile.)
#   • criteria-echo-mcp on PATH (built by make example-adapter-tools-claude
#     as bin/criteria-echo-mcp): the mcp adapter spawns it as its MCP server
#     over stdio — THIS is a PATH lookup, unlike adapter discovery.
#
# Run with: make example-adapter-tools-claude
# Or directly:
#   make build plugins
#   go build -o bin/criteria-echo-mcp ./cmd/criteria-adapter-mcp/testfixtures/echo-mcp
#   tmpdir=$(mktemp -d) && cp bin/criteria-adapter-mcp bin/criteria-echo-mcp "$tmpdir/"
#   (put criteria-adapter-claude-agent in "$tmpdir" or an adapters root)
#   PATH="$tmpdir:$PATH" CRITERIA_ADAPTERS="$tmpdir" ./bin/criteria apply \
#     examples/adapter_tools/claude_mcp_resource/claude_mcp_resource.hcl

workflow {
  name          = "adapter_tools_claude_mcp_resource"
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
# the fixture server's tools/list (echo, structured) at runtime. This block
# is byte-identical to the CRI-181 sample's mcp resource block: the two
# real-agent samples share the fixture contract so it stays portable.
adapter "mcp" "tools" {
  config {
    command = "criteria-echo-mcp"
  }
  dynamic_tools = true
}

# The caller is a real Claude Code agent. `claude-agent` is the adapter type
# string the claude-agent repo declares in its README and example.hcl.
adapter "claude-agent" "agent" {
  config {
    model = "claude-sonnet-4-6"
  }
}

step "call" {
  target = adapter.claude-agent.agent

  # Step-level grant (ADR-0004 §4): the full-target-string glob form, on
  # purpose. The mcp callee's tool surface is discovered at runtime
  # (dynamic_tools), so there is nothing static to reference, and a glob
  # exercises the first-match-wins policy. It also keeps validation free of
  # the static vocabulary check, which only applies to literal tool names.
  allow_tools = ["adapter.mcp.tools.tools.*"]

  input {
    prompt = <<-EOT
      You are finalizing release bookkeeping for adapter tools wave 2.6.
      The release code is not guessable: the only way to obtain it is to
      call the adapter_tool tool, exactly once, with:

        target = "adapter.mcp.tools.tools.echo"
        arguments = { "tool": "echo", "message": "release-code for build 2.6" }

      The release code comes back inside the tool result's text: read it
      from the tool result. Do not guess or invent the release code, and
      do not use any other tool.
      After you have read the tool result, report the release code and end
      your final line with exactly: RESULT: success
      If the tool call fails, end your final line with exactly: RESULT: failure
      Then finalize the step by calling submit_outcome with outcome success
      (or outcome failure if the tool call failed).
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