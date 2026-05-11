workflow "approval_and_wait" {
  version       = "0.1"
  initial_state = "prepare"
  target_state  = "done"
}

adapter "shell" "default" {
  config { }
}

step "prepare" {
  target = adapter.shell.default
  input { command = "echo 'ready for release'" }
  outcome "success" { next = "release_gate" }
  outcome "failure" { next = "failed" }
}

approval "release_gate" {
  approvers = ["@engineering"]
  reason    = "Approve deployment to production"
  outcome "approved" { next = "deploy_window" }
  outcome "rejected" { next = "failed" }
}

wait "deploy_window" {
  signal = "deploy-ready"
  outcome "received" { next = "deploy" }
}

step "deploy" {
  target = adapter.shell.default
  input { command = "echo 'deploying'" }
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
