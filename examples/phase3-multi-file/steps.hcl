step "greet" {
  target = adapter.shell.default
  input {
    command = "echo hello ${var.name}"
  }
  outcome "success" { next = state.done }
  outcome "failure" { next = state.done }
}

state "done" {
  terminal = true
  success  = true
}
