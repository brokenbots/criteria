workflow {
  name = "secret-variable"
  version = "0.1"
  initial_state = "run"
  target_state  = "done"
}

variable "api_key" {
  type    = string
  secret  = true
  default = "key"
}

adapter "noop" "default" {}

step "run" {
  target = adapter.noop.default
  secret_input {
    key = var.api_key
  }
  outcome "success" { next = state.done }
}

state "done" { terminal = true }
