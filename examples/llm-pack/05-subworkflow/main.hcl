workflow {
  name = "subwf-parent"
  version       = "1"
  initial_state = "prepare"
  target_state  = "done"
}

adapter "noop" "default" {}

subworkflow "process_one" {
  source = "./child"
  input = {
    item = "example"
  }
}

step "prepare" {
  target = adapter.noop.default
  outcome "success" { next = step.invoke }
  outcome "failure" { next = state.failed }
}

step "invoke" {
  target = subworkflow.process_one
  outcome "success" { next = step.finish }
  outcome "failure" { next = state.failed }
}

step "finish" {
  target = adapter.noop.default
  input {
    processed = steps.invoke.result
  }
  outcome "success" { next = state.done }
}

state "done" {
  terminal = true
  success  = true
}

state "failed" {
  terminal = true
  success  = false
}
