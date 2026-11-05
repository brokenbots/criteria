# mode: standalone
# Example: greeter adapter — a minimal third-party adapter example.
# Run with: CRITERIA_ADAPTERS=<dir-with-criteria-adapter-greeter> criteria apply example.hcl
workflow {
  name = "greeter_example"
  version       = "0.1"
  initial_state = "greet"
  target_state  = "done"
}

adapter "greeter" "default" {
  config { }
}

step "greet" {
  target = adapter.greeter.default
  input {
    name = "world"
  }

  outcome "success" { next = state.done }
  outcome "failure" { next = state.failed }
}

state "done" { terminal = true }
state "failed" {
  terminal = true
  success  = false
}
