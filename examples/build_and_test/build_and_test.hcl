# Example: shell-only build → test → terminal
# Demonstrates linear flow with two terminal states.
# mode: standalone
workflow {
  name = "build_and_test"
  version       = "0.1"
  initial_state = "build"
  target_state  = "verified"
  policy {
    max_total_steps = 20
  }
}

adapter "shell" "default" {
  config { }
}

step "build" {
  target = adapter.shell.default
  input {
    command = "go build ./..."
  }
  timeout = "5m"

  outcome "success" { next = step.test }
  outcome "failure" { next = state.failed }
}

step "test" {
  target = adapter.shell.default
  input {
    command = "go test ./..."
  }
  timeout = "10m"

  outcome "success" { next = state.verified }
  outcome "failure" { next = state.failed }
}

state "verified" { terminal = true }
state "failed" {
  terminal = true
  success  = false
}

