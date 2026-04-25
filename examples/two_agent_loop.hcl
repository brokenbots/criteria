workflow "two_agent_loop" {
  version       = "1"
  initial_state = "open_executor"
  target_state  = "done"

  policy {
    max_total_steps = 50
  }

  agent "executor" {
    adapter = "copilot"
  }

  agent "reviewer" {
    adapter = "copilot"
  }

  step "open_executor" {
    agent     = "executor"
    lifecycle = "open"

    outcome "success" { transition_to = "open_reviewer" }
    outcome "failure" { transition_to = "failed" }
  }

  step "open_reviewer" {
    agent     = "reviewer"
    lifecycle = "open"

    outcome "success" { transition_to = "execute" }
    outcome "failure" { transition_to = "close_executor_failed" }
  }

  step "execute" {
    agent       = "executor"
    allow_tools = ["read_file", "write_file", "shell:git diff", "shell:go build*", "shell:go test*"]
    config = {
      max_turns = "8"
      prompt    = "You are the executor in a two-agent engineering loop. Implement the next item from the task list using small focused changes, then stop. End your final line with exactly one of: RESULT: needs_review | RESULT: success | RESULT: failure. Use RESULT: needs_review when you are ready for review."
    }

    outcome "needs_review" { transition_to = "review" }
    outcome "success"      { transition_to = "review" }
    outcome "failure"      { transition_to = "close_reviewer_failed" }
  }

  step "review" {
    agent       = "reviewer"
    allow_tools = ["read_file", "shell:git diff"]
    config = {
      max_turns = "8"
      prompt    = "You are the reviewer in a two-agent engineering loop. Review the latest changes for correctness and clarity. End your final line with exactly one of: RESULT: approved | RESULT: changes_requested | RESULT: failure."
    }

    outcome "approved"          { transition_to = "close_reviewer_done" }
    outcome "changes_requested" { transition_to = "execute" }
    outcome "failure"           { transition_to = "close_reviewer_failed" }
  }

  step "close_reviewer_done" {
    agent     = "reviewer"
    lifecycle = "close"

    outcome "success" { transition_to = "close_executor_done" }
    outcome "failure" { transition_to = "close_executor_failed" }
  }

  step "close_reviewer_failed" {
    agent     = "reviewer"
    lifecycle = "close"

    outcome "success" { transition_to = "close_executor_failed" }
    outcome "failure" { transition_to = "close_executor_failed" }
  }

  step "close_executor_done" {
    agent     = "executor"
    lifecycle = "close"

    outcome "success" { transition_to = "done" }
    outcome "failure" { transition_to = "failed" }
  }

  step "close_executor_failed" {
    agent     = "executor"
    lifecycle = "close"

    outcome "success" { transition_to = "failed" }
    outcome "failure" { transition_to = "failed" }
  }

  state "done" {
    terminal = true
    success  = true
  }

  state "failed" {
    terminal = true
    success  = false
  }
}