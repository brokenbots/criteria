workflow "process_one" {
  version       = "0.1"
  initial_state = "process"
  target_state  = "success_outcome"
}

variable "idx" {
  type = "number"
}

variable "limit" {
  type = "number"
}

adapter "shell" "default" {
  config { }
}

step "process" {
  target = adapter.shell.default
  input  = { command = "echo Processing item ${var.idx}" }
  
  outcome "success" {
    next = state.success_outcome
  }
}

state "success_outcome" {
  terminal = true
  success  = true
}

output "reason" {
  type  = "string"
  value = "Processed ${var.idx}"
}
