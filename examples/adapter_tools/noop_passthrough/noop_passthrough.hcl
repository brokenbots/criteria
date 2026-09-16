# mode: standalone
# Example: variant 1 — a caller adapter tool-calls a data-ish callee in one
# self-contained run (CRI-176, CRI-165, ADR-0004). Both roles are the in-tree
# noop fixture: the caller runs its tool-call mode (the step's input names
# the target and the tool arguments), the callee runs its outputs-passthrough
# mode (the tool args become the callee's step input, and the outputs JSON
# object passes back through unchanged).
#
# Run with: make example-adapter-tools
# Or directly:
#   make build plugins
#   tmpdir=$(mktemp -d) && cp bin/criteria-adapter-noop "$tmpdir/"
#   CRITERIA_ADAPTERS="$tmpdir" ./bin/criteria apply examples/adapter_tools/noop_passthrough/noop_passthrough.hcl

workflow {
  name          = "adapter_tools_noop_passthrough"
  version       = "0.1"
  initial_state = "call"
  target_state  = "done"
}

# The callee declares its tool surface at compile time; the tool's runtime
# behavior is the callee side of the fixture (the outputs passthrough).
adapter "noop" "callee" {
  tool "echo_data" {}
}

# The caller is the same fixture binary running its tool-call caller mode.
adapter "noop" "caller" {}

step "call" {
  target = adapter.noop.caller

  # Step-level tools grant (ADR-0004 §4): the caller may invoke the callee's
  # echo_data without any allow_tools on the caller's own side.
  tools = [adapter.noop.callee.tools.echo_data]

  input {
    tool_target = "adapter.noop.callee.tools.echo_data"
    tool_args   = jsonencode({
      outputs = jsonencode({
        rows  = 2
        items = ["alpha", "beta"]
        note  = "echoed back by the callee"
      })
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

# The callee's result passes through typed on the caller side under callee.*
# keys: callee.outcome (the callee's own outcome) and callee.outputs (the
# callee's typed outputs object). Nested attribute access works because the
# values stay typed.
output "callee_outcome" {
  value = steps.call["callee.outcome"]
}
output "callee_outputs" {
  value = steps.call["callee.outputs"]
}
output "callee_rows" {
  value = steps.call["callee.outputs"].rows
}