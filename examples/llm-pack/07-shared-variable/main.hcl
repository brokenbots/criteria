workflow {
  name = "shared-var"
  version       = "1"
  initial_state = "increment"
  target_state  = "done"
}

adapter "noop" "default" {}
data "internal" "counter" {
  type = string
  value = "0"
}

step "increment" {
  target = adapter.noop.default
  outcome "success" {
    next = step.double
    write {
      target = data.internal.counter.value
      value  = "1"
    }
  }
  outcome "failure" { next = state.failed }
}

step "double" {
  target = adapter.noop.default
  input {
    current = data.internal.counter.value
  }
  outcome "success" {
    next = state.done
    write {
      target = data.internal.counter.value
      value  = "2"
    }
  }
  outcome "failure" { next = state.failed }
}

state "done" {
  terminal = true
  success  = true
}

state "failed" {
  terminal = true
  success  = false
}
