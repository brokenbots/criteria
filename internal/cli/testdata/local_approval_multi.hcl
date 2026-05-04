workflow "local_approval_multi" {
  version       = "0.1"
  initial_state = "first_review"
  target_state  = "done"

  adapter "noop" "demo" {}

  approval "first_review" {
    approvers = ["alice"]
    reason    = "first gate"
    outcome "approved" { transition_to = "second_review" }
    outcome "rejected" { transition_to = "rejected_state" }
  }

  approval "second_review" {
    approvers = ["bob"]
    reason    = "second gate"
    outcome "approved" { transition_to = "open_demo" }
    outcome "rejected" { transition_to = "rejected_state" }
  }

  step "open_demo" {
    adapter = "noop.demo"
    lifecycle = "open"
    outcome "success" { transition_to = "run_step" }
    outcome "failure" { transition_to = "failed" }
  }

  step "run_step" {
    adapter = "noop.demo"
    input {
      prompt = "continue"
    }
    outcome "success" { transition_to = "close_demo" }
    outcome "failure" { transition_to = "failed" }
  }

  step "close_demo" {
    adapter = "noop.demo"
    lifecycle = "close"
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
