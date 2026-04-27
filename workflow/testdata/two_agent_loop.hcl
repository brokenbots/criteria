workflow "two_agent_loop" {
  version       = "0.1"
  initial_state = "open_executor"
  target_state  = "done"

  agent "executor" {
    adapter  = "copilot"
    on_crash = "respawn"
  }

  agent "reviewer" {
    adapter = "copilot"
  }

  step "open_executor" {
    agent     = "executor"
    lifecycle = "open"

    outcome "success" { transition_to = "open_reviewer" }
  }

  step "open_reviewer" {
    agent     = "reviewer"
    lifecycle = "open"

    outcome "success" { transition_to = "execute" }
  }

  step "execute" {
    agent    = "executor"
    on_crash = "abort_run"

    outcome "approved" { transition_to = "close_reviewer" }
    outcome "retry"    { transition_to = "review" }
  }

  step "review" {
    agent = "reviewer"

    outcome "approved" { transition_to = "close_reviewer" }
    outcome "changes"  { transition_to = "execute" }
  }

  step "close_reviewer" {
    agent     = "reviewer"
    lifecycle = "close"

    outcome "success" { transition_to = "close_executor" }
  }

  step "close_executor" {
    agent     = "executor"
    lifecycle = "close"

    outcome "success" { transition_to = "done" }
  }

  state "done" {
    terminal = true
  }
}
