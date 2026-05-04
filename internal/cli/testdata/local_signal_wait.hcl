workflow "local_signal_wait" {
  version       = "0.1"
  initial_state = "gate"
  target_state  = "done"

  adapter "noop" "demo" {}

  wait "gate" {
    signal = "proceed"
    outcome "success" { transition_to = "run_step" }
  }

  step "run_step" {
    adapter = adapter.noop.demo
    input {
      prompt = "continue"
    }
    outcome "success" { transition_to = "done" }
    outcome "failure" { transition_to = "failed" }
  }

  state "done" {
    terminal = true
    success  = true
  }

  state "failed" {
    terminal = true
    success  = false
  }
}
