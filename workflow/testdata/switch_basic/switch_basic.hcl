// switch_basic.hcl — exercises the switch node kind (W16).
// A three-condition switch selects between "deploy", "deploy_staging", and
// "skip_deploy" based on var.env and a captured step output. A default arm
// handles the fallback.
workflow "switch_basic" {
  version       = "0.1"
  initial_state = "build"
  target_state  = "done"
}

variable "env" {
  type    = "string"
  default = "staging"
}

adapter "noop" "default" {}

step "build" {
  target = adapter.noop.default
  outcome "success" { next = "decide" }
}

switch "decide" {
  condition {
    match = var.env == "prod"
    next  = state.deploy
  }
  condition {
    match = var.env == "staging"
    next  = state.deploy_staging
  }
  condition {
    match = steps.build.exit_code == "0"
    next  = state.deploy_staging
  }
  default {
    next = state.skip_deploy
  }
}

state "deploy" {
  terminal = true
  success  = true
}

state "deploy_staging" {
  terminal = true
  success  = true
}

state "skip_deploy" {
  terminal = true
  success  = true
}

state "done" {
  terminal = true
  success  = true
}
