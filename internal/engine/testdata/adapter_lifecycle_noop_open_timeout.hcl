workflow "agent_lifecycle_noop_open_timeout" {
  version = "0.1"
  initial_state = "open_agent"
  target_state  = "done"

  adapter "noop" "demo" {
    on_crash = "fail"
    config {
      bootstrap = "true"
  }
  }

  step "open_agent" {
    adapter = "noop.demo"
    lifecycle = "open"
    timeout   = "1s"
    outcome "success" { transition_to = "run_agent" }
    outcome "failure" { transition_to = "failed" }
  }

  step "run_agent" {
    adapter = "noop.demo"
    input {
      prompt = "hello"
    }
    outcome "success" { transition_to = "close_agent" }
    outcome "failure" { transition_to = "failed" }
  }

  step "close_agent" {
    adapter = "noop.demo"
    lifecycle = "close"
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
