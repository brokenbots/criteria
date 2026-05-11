workflow "process_one" {
  version       = "0.1"
  initial_state = "execute"
  target_state  = "done"
}

adapter "shell" "default" {
  config { }
}

variable "item" {
  type    = "string"
  default = "default-item"
}

output "result" {
  type  = "string"
  value = "processed: ${var.item}"
}

step "execute" {
  target = adapter.shell.default
  input {
    command = "echo 'processing ${var.item}'"
  }
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
