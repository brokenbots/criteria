workflow {
  name = "local_control_pause"
  version = "0.1"
  initial_state = "settle"
  target_state  = "done"
}

adapter "noop" "demo" {}

wait "settle" {
  duration = "700ms"
  outcome "elapsed" { next = step.run_step }
}

step "run_step" {
  target = adapter.noop.demo
  input {
    prompt = "finish"
  }
  outcome "success" { next = state.done }
}

state "done" {
  terminal = true
  success  = true
}
