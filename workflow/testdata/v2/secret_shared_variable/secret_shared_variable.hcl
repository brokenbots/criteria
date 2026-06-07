workflow {
  name = "secret-shared-variable"
  version = "0.1"
  initial_state = "run"
  target_state  = "done"
}

data "internal" "token" {
  type   = string
  secret = true
}

adapter "noop" "default" {}

step "run" {
  target = adapter.noop.default
  secret_input {
    key = data.internal.token.value
  }
  outcome "success" { next = state.done }
}

state "done" { terminal = true }
