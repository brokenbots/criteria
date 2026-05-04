# Example: shell-only build → test → terminal
# Demonstrates linear flow with two terminal states.
# mode: standalone
workflow "build_and_test" {
  version       = "0.1"
  initial_state = "build"
  target_state  = "verified"

  adapter "shell" "default" {
    config { }
  }

  step "build" {
    adapter = "shell.default"
    input {
      command = "go build ./..."
    }
    timeout = "5m"

    outcome "success" { transition_to = "test" }
    outcome "failure" { transition_to = "failed" }
  }

  step "test" {
    adapter = "shell.default"
    input {
      command = "go test ./..."
    }
    timeout = "10m"

    outcome "success" { transition_to = "verified" }
    outcome "failure" { transition_to = "failed" }
  }

  state "verified" { terminal = true }
  state "failed" {
    terminal = true
    success  = false
  }

  policy {
    max_total_steps = 20
  }
}
