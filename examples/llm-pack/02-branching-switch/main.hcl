workflow "branching_switch" {
  version       = "0.1"
  initial_state = "classify"
  target_state  = "done"
}

adapter "shell" "default" {
  config { }
}

step "classify" {
  target = adapter.shell.default
  input {
    command = "echo 'approved'"
  }
  outcome "success" { next = "route" }
  outcome "failure" { next = "failed" }
}

switch "route" {
  condition {
    match = steps.classify.stdout == "approved"
    next  = step.approve
  }
  condition {
    match = steps.classify.stdout == "rejected"
    next  = step.reject
  }
  default {
    next = state.failed
  }
}

step "approve" {
  target = adapter.shell.default
  input { command = "echo 'approved path'" }
  outcome "success" { next = "done" }
  outcome "failure" { next = "failed" }
}

step "reject" {
  target = adapter.shell.default
  input { command = "echo 'rejected path'" }
  outcome "success" { next = "done" }
  outcome "failure" { next = "failed" }
}

state "done" {
  terminal = true
  success  = true
}

state "failed" {
  terminal = true
  success  = false
}
