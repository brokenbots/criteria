# copilot_planning_then_execution.hcl
#
# Demonstrates per-step reasoning_effort override with the Copilot adapter.
#
# The planning step uses "high" reasoning effort to produce a careful plan.
# Execution steps inherit the adapter-level default of "medium".
#
# Requirements:
#   • criteria-adapter-copilot installed in $CRITERIA_PLUGINS or ~/.criteria/plugins/
#   • copilot CLI on PATH (or pointed at via CRITERIA_COPILOT_BIN)
#   • A running Criteria-compatible orchestrator (--server flag)
#
# Usage:
#   ./bin/criteria apply examples/copilot_planning_then_execution.hcl \
#     --server http://127.0.0.1:8080 --server-codec proto

workflow "copilot_planning_then_execution" {
  version       = "1"
  initial_state = "plan"
  target_state  = "done"

  policy {
    max_total_steps = 20
  }

  adapter "copilot" "engineer" {
    config {
      model            = "claude-sonnet-4.6"
      reasoning_effort = "medium"   # default for all steps
      max_turns        = 6
    }
  }

  # ── Planning (high reasoning effort) ────────────────────────────────────────

  step "plan" {
    adapter = adapter.copilot.engineer
    allow_tools = ["read_file"]
    input {
      # reasoning_effort = "high" overrides the adapter default for this step only.
      reasoning_effort = "high"
      prompt           = <<-EOT
        Draft a numbered implementation plan (3–5 steps) for adding a new "greet"
        shell command that prints "Hello, <name>!" given a name argument.
        End your final line with exactly: RESULT: success
      EOT
    }

    outcome "success" { transition_to = "execute" }
    outcome "failure" { transition_to = "failed" }
  }

  # ── Execution (inherits adapter-level "medium") ────────────────────────────────

  step "execute" {
    adapter = adapter.copilot.engineer
    allow_tools = ["read_file", "write_file", "shell:go build*", "shell:go test*"]
    input {
      prompt = <<-EOT
        Implement the plan from the previous step as a minimal Go program.
        End your final line with exactly: RESULT: success
      EOT
    }

    outcome "success" { transition_to = "done" }
    outcome "failure" { transition_to = "failed" }
  }

  # ── Terminal states ────────────────────────────────────────────────────────

  state "done" {
    terminal = true
    success  = true
  }

  state "failed" {
    terminal = true
    success  = false
  }
}
