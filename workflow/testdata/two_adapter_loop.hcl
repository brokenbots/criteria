workflow "two_agent_loop" {
  version       = "0.1"
  initial_state = "open_executor"
  target_state  = "done"

  adapter "copilot" "executor" {
    on_crash = "respawn"
  }

  adapter "copilot" "reviewer" {}

  step "open_executor" {
    adapter = adapter.copilot.executor
    lifecycle = "open"

    outcome "success" { transition_to = "open_reviewer" }
  }

  step "open_reviewer" {
    adapter = adapter.copilot.reviewer
    lifecycle = "open"

    outcome "success" { transition_to = "execute" }
  }

  step "execute" {
    adapter = adapter.copilot.executor
    on_crash = "abort_run"

    outcome "approved" { transition_to = "close_reviewer" }
    outcome "retry"    { transition_to = "review" }
  }

  step "review" {
    adapter = adapter.copilot.reviewer

    outcome "approved" { transition_to = "close_reviewer" }
    outcome "changes"  { transition_to = "execute" }
  }

  step "close_reviewer" {
    adapter = adapter.copilot.reviewer
    lifecycle = "close"

    outcome "success" { transition_to = "close_executor" }
  }

  step "close_executor" {
    adapter = adapter.copilot.executor
    lifecycle = "close"

    outcome "success" { transition_to = "done" }
  }

  state "done" {
    terminal = true
  }
}
