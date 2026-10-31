workflow {
  name = "two_agent_loop"
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

  outcome "approved" { next = step.review }
  outcome "retry"    { next = step.review }
  outcome "failure"  { next = state.done }
}

step "review" {
  target = adapter.copilot.reviewer

  outcome "approved" { next = state.done }
  outcome "changes"  { next = step.execute }
  outcome "failure"  { next = state.done }
}

state "done" {
  terminal = true
}
