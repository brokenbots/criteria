workflow "shared_state" {
  version       = "0.1"
  initial_state = "init"
  target_state  = "done"
}

adapter "shell" "default" {
  config { }
}

shared_variable "status" {
  type  = "string"
  value = "pending"
}

step "init" {
  target = adapter.shell.default
  input {
    command = "echo 'processing'"
  }
  outcome "success" {
    next          = "finish"
    shared_writes = { status = "stdout" }
  }
  outcome "failure" { next = "failed" }
}

step "finish" {
  target = adapter.shell.default
  input {
    command = "echo 'done: ${shared.status}'"
  }
  outcome "success" {
    next          = "done"
    shared_writes = { status = "stdout" }
  }
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
