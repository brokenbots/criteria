workflow "agent_lifecycle_noop" {
  version = "0.1"
  initial_state = "run_agent"
  target_state  = "done"

  adapter "noop" "demo" {
    on_crash = "fail"
    config {
      bootstrap = "true"
  }
  }

  step "run_agent" {
    adapter = adapter.noop.demo
    input {
      prompt = "hello"
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
