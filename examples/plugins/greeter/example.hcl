# mode: standalone
# Example: greeter adapter — demonstrates a minimal third-party plugin.
# Run with: CRITERIA_PLUGINS=<dir-with-criteria-adapter-greeter> criteria apply example.hcl
workflow "greeter_example" {
  version       = "0.1"
  initial_state = "greet"
  target_state  = "done"

  step "greet" {
    adapter = "greeter"
    input {
      name = "world"
    }

    outcome "success" { transition_to = "done" }
    outcome "failure" { transition_to = "failed" }
  }

  state "done" { terminal = true }
  state "failed" {
    terminal = true
    success  = false
  }
}
