# Example: shell + Copilot adapter
# The agent step instructs the model to end its final message with a
# `RESULT: success|needs_review|failure` line; the copilot adapter parses
# that to determine the outcome.
workflow "agent_fix_test" {
  version       = "0.1"
  initial_state = "test"
  target_state  = "verified"

  step "test" {
    adapter = "shell"
    input {
      command = "go test ./..."
    }
    timeout = "5m"

    outcome "success" { transition_to = "verified" }
    outcome "failure" { transition_to = "fix" }
  }

  step "fix" {
    adapter = "copilot"
    input {
      model  = "claude-sonnet-4.5"
      prompt = "The test suite is failing. Investigate, fix the failing tests (do not modify the assertions themselves), and confirm by running `go test ./...`. Reply ending with one line: RESULT: success | needs_review | failure"
    }
    timeout = "30m"

    outcome "success"      { transition_to = "test" }
    outcome "needs_review" { transition_to = "awaiting_human" }
    outcome "failure"      { transition_to = "failed" }
  }

  state "verified" { terminal = true }
  state "failed" {
    terminal = true
    success  = false
  }
  state "awaiting_human" {
    terminal = true
    success  = false
    requires = "human_input"
  }

  policy {
    max_total_steps  = 30
    max_step_retries = 2
  }
}
