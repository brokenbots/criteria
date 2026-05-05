workflow "inner_task" {
  version       = "0.1"
  initial_state = "execute"
  target_state  = "complete"
}

adapter "shell" "default" {
  config { }
}

variable "work" {
  type = "string"
}

output "result" {
  type  = "string"
  value = "Task completed successfully"
}

step "execute" {
  target = adapter.shell.default
  input {
    command = "echo 'Processing task'"
  }
  outcome "success" { next = "complete" }
  outcome "failure" { next = "complete" }
}

state "complete" {
  terminal = true
  success  = true
}
