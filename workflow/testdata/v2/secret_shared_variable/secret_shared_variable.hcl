workflow {
  name = "secret-shared-variable"
  version = "0.1"
  initial_state = "run"
  target_state  = "done"
}

shared_variable "token" {
  type   = string
  secret = true
}

adapter "noop" "default" {}

step "run" {
  target = adapter.noop.default
  secret_input {
    key = shared.token
  }
  outcome "success" { next = "done" }
}

state "done" { terminal = true }
