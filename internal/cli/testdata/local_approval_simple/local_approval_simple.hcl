workflow {
  name = "local_approval_simple"
  version       = "0.1"
  initial_state = "review"
  target_state  = "done"
}

adapter "noop" "demo" {}

approval "review" {
  approvers = ["alice"]
  reason    = "needs review"
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
