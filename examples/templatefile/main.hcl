# mode: standalone
# Example: demonstrates templatefile() — reads a Go text/template file and
# renders it with the provided variable bindings.
workflow "templatefile_demo" {
  version       = "1"
  initial_state = "render"
  target_state  = "done"
}

variable "topic" {
  type    = "string"
  default = "release notes"
}

adapter "shell" "echoer" {}

state "done" {
  terminal = true
  success  = true
}

step "render" {
  target = adapter.shell.echoer
  input {
    command = templatefile("prompts/intro.tmpl", { topic = var.topic })
  }
  outcome "success" { next = "done" }
  outcome "failure" { next = "done" }
}
