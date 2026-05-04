workflow "two_agent_loop" {
  version       = "0.1"
  initial_state = "execute"
  target_state  = "done"

  adapter "copilot" "executor" {
    on_crash = "respawn"
  }

  adapter "copilot" "reviewer" {}

  step "execute" {
    adapter = adapter.copilot.executor
    on_crash = "abort_run"

    outcome "approved" { transition_to = "review" }
    outcome "retry"    { transition_to = "review" }
  }

  step "review" {
    adapter = adapter.copilot.reviewer

    outcome "approved" { transition_to = "done" }
    outcome "changes"  { transition_to = "execute" }
  }

  state "done" {
    terminal = true
  }
}
