workflow {
  name = "secret-inputs"
  version = "0.1"
  initial_state = "run"
  target_state  = "done"
}

variable "api_key" {
  type    = string
  secret  = true
  default = "key"
}

adapter "shell" "default" {}

step "run" {
  target = adapter.shell.default
  secret_input {
    command = var.api_key
  }
  outcome "success" { next = state.done }
}

state "done" { terminal = true }
