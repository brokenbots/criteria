workflow {
  name = "adapter-secrets"
  version = "0.1"
  initial_state = "run"
  target_state  = "done"
}

variable "api_key" {
  type    = string
  secret  = true
  default = "key"
}

adapter "exec" "default" {
  secrets {
    api_key = var.api_key
  }
}

step "run" {
  target = adapter.exec.default
  input {
    command = "echo hi"
  }
  outcome "success" { next = state.done }
}

state "done" { terminal = true }
