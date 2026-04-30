workflow "local_approval_simple" {
  version       = "0.1"
  initial_state = "review"
  target_state  = "done"

  agent "demo" {
    adapter = "noop"
  }

  approval "review" {
    approvers = ["alice"]
    reason    = "needs review"
    outcome "approved" { transition_to = "open_demo" }
    outcome "rejected" { transition_to = "rejected_state" }
  }

  step "open_demo" {
    agent     = "demo"
    lifecycle = "open"
    outcome "success" { transition_to = "run_step" }
    outcome "failure" { transition_to = "failed" }
  }

  step "run_step" {
    agent = "demo"
    input {
      prompt = "continue"
    }
    outcome "success" { transition_to = "close_demo" }
    outcome "failure" { transition_to = "failed" }
  }

  step "close_demo" {
    agent     = "demo"
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
