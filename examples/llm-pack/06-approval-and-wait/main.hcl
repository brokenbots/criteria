workflow "approval-wait" {
  version       = "1"
  initial_state = "deploy_window"
  target_state  = "done"
}

adapter "noop" "default" {}

wait "deploy_window" {
  signal = "deploy-ready"
  outcome "received" { next = "release_gate" }
  outcome "expired"  { next = "failed" }
}

approval "release_gate" {
  approvers = ["ops-lead", "security-lead"]
  reason    = "Production release requires dual approval."
  outcome "approved" { next = "deploy" }
  outcome "rejected" { next = "failed" }
}

step "deploy" {
  target = adapter.noop.default
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
