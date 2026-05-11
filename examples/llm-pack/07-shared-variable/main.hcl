workflow "shared-var" {
  version       = "1"
  initial_state = "increment"
  target_state  = "done"
}

adapter "noop" "default" {}

shared_variable "counter" {
  type  = "string"
  value = "0"
}

step "increment" {
  target = adapter.noop.default
  outcome "success" {
    next          = "double"
    shared_writes = { counter = "next_value" }
  }
}

step "double" {
  target = adapter.noop.default
  input {
    current = shared.counter
  }
  outcome "success" {
    next          = "done"
    shared_writes = { counter = "next_value" }
  }
}

state "done" {
  terminal = true
  success  = true
}
