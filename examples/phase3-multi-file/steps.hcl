step "greet" {
  target = adapter.shell.default
  input {
    command = "echo hello ${var.name}"
  }
  outcome "success" { next = "done" }
  outcome "failure" { next = "done" }
}

state "done" {
  terminal = true
  success  = true
}
