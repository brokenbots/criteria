workflow {
  name = "local_approval_multi"
  version       = "0.1"
  initial_state = "first_review"
  target_state  = "done"
}

adapter "noop" "demo" {}

approval "first_review" {
  approvers = ["alice"]
  reason    = "first gate"
  outcome "approved" { next = approval.second_review }
  outcome "rejected" { next = state.rejected_state }
}

approval "second_review" {
  approvers = ["bob"]
  reason    = "second gate"
  outcome "approved" { next = step.run_step }
  outcome "rejected" { next = state.rejected_state }
}

step "run_step" {
  target = adapter.noop.demo
  input {
    prompt = "continue"
  }
  outcome "success" { next = state.done }
  outcome "failure" { next = state.failed }
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
