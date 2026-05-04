workflow "local_approval_simple" {
  version       = "0.1"
  initial_state = "review"
  target_state  = "done"

  adapter "noop" "demo" {}

  approval "review" {
    approvers = ["alice"]
    reason    = "needs review"
    outcome "approved" { transition_to = "run_step" }
    outcome "rejected" { transition_to = "rejected_state" }
  }

  step "run_step" {
    target = adapter.noop.demo
    input {
      prompt = "continue"
    }
    outcome "success" { transition_to = "done" }
    outcome "failure" { transition_to = "failed" }
  }

  state "done" {
    terminal = true
    success  = true
  }

  state "rejected_state" {
    terminal = true
    success  = false
  }

  state "failed" {
    terminal = true
    success  = false
  }
}
