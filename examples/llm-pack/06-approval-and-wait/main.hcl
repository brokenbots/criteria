workflow {
  name = "approval-wait"
  version       = "1"
  initial_state = "deploy_window"
  target_state  = "done"
}

adapter "noop" "default" {}

wait "deploy_window" {
  signal = "deploy-ready"
  outcome "received" { next = approval.release_gate }
  outcome "expired"  { next = state.failed }
}

approval "release_gate" {
  approvers = ["ops-lead", "security-lead"]
  reason    = "Production release requires dual approval."
  outcome "approved" { next = step.deploy }
  outcome "rejected" { next = state.failed }
}

step "deploy" {
  target = adapter.noop.default
  outcome "success" { next = state.done }
  outcome "failure" { next = state.failed }
}

state "done" {
  terminal = true
  success  = true
}

state "failed" {
  terminal = true
  success  = false
}
