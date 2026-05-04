# mode: standalone
# Example: trivial single-step workflow used by smoke tests.
workflow "hello" {
  version       = "0.1"
  initial_state = "say_hello"
  target_state  = "done"

  adapter "shell" "default" {
    config { }
  }

  output "greeting" {
    type        = "string"
    description = "The greeting message produced by the workflow"
    value       = "Execution complete"
  }

  step "say_hello" {
    target = adapter.shell.default
    input {
      command = "echo hello from criteria"
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
