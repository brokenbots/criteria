workflow "process-one" {
  version       = "1"
  initial_state = "execute"
  target_state  = "done"
}

adapter "noop" "default" {}

variable "item" {
  type = "string"
}

output "result" {
  type  = "string"
  value = "processed:${var.item}"
}

step "execute" {
  target = adapter.noop.default
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
