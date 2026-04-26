workflow "agent_lifecycle_noop" {
  version = "0.1"
  initial_state = "open_agent"
  target_state  = "done"

  agent "demo" {
    adapter  = "noop"
    on_crash = "fail"
    config {
      bootstrap = "true"
    }
  }

  step "open_agent" {
    agent     = "demo"
    lifecycle = "open"
    outcome "success" { transition_to = "run_agent" }
    outcome "failure" { transition_to = "failed" }
  }

  step "run_agent" {
    agent = "demo"
    input {
      prompt = "hello"
    }
    outcome "success" { transition_to = "close_agent" }
    outcome "failure" { transition_to = "failed" }
  }

  step "close_agent" {
    agent     = "demo"
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
