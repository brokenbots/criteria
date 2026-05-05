workflow "two_agent_loop" {
  version       = "0.1"
  initial_state = "execute"
  target_state  = "done"
}

adapter "copilot" "executor" {
  on_crash = "respawn"
}

adapter "copilot" "reviewer" {}

step "execute" {
  target = adapter.copilot.executor
  on_crash = "abort_run"

  outcome "approved" { next = "review" }
  outcome "retry"    { next = "review" }
}

step "review" {
  target = adapter.copilot.reviewer

  outcome "approved" { next = "done" }
  outcome "changes"  { next = "execute" }
}

state "done" {
  terminal = true
}
