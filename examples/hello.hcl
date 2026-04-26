# Example: trivial single-step workflow used by smoke tests.
workflow "hello" {
  version       = "0.1"
  initial_state = "say_hello"
  target_state  = "done"

  step "say_hello" {
    adapter = "shell"
    input {
      command = "echo hello from overlord"
    }

    outcome "success" { transition_to = "done" }
    outcome "failure" { transition_to = "failed" }
  }

  state "done"   { terminal = true }
  state "failed" {
    terminal = true
    success  = false
  }
}
