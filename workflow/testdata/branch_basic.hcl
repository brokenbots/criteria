// branch_basic.hcl — exercises the branch node kind (W06).
// A two-arm branch selects between "deploy" and "skip_deploy" based on
// var.env and a captured step output. A default arm handles the fallback.
workflow "branch_basic" {
  version       = "0.1"
  initial_state = "build"
  target_state  = "done"

  variable "env" {
    type    = "string"
    default = "staging"
  }

  adapter "noop" "default" {}

  step "build" {
    adapter = adapter.noop.default
    outcome "success" { transition_to = "decide" }
  }

  branch "decide" {
    arm {
      when          = var.env == "prod"
      transition_to = "deploy"
    }
    arm {
      when          = var.env == "staging"
      transition_to = "deploy_staging"
    }
    arm {
      when          = steps.build.exit_code == "0"
      transition_to = "deploy_staging"
    }
    default {
      transition_to = "skip_deploy"
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
}
