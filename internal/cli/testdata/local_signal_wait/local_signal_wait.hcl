workflow {
  name = "local_signal_wait"
  version       = "0.1"
  initial_state = "gate"
  target_state  = "done"
}

adapter "noop" "demo" {}

wait "gate" {
  signal = "proceed"
  outcome "success" { next = step.run_step }
}

step "run_step" {
  target = adapter.noop.demo
  input {
    prompt = "continue"
  }
  outcome "success" { next = state.done }
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
