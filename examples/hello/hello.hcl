# mode: standalone
# Example: trivial single-step workflow used by smoke tests. Uses the in-tree
# `noop` adapter so the smoke test is self-contained (no external adapter).
workflow {
  name = "hello"
  version       = "0.1"
  initial_state = "say_hello"
  target_state  = "done"
}

adapter "noop" "default" {
  config { }
}

output "greeting" {
  type = string
  description = "The greeting message produced by the workflow"
  value       = "Execution complete"
}

step "say_hello" {
  target = adapter.noop.default
  input {}

  outcome "success" { next = state.done }
  outcome "failure" { next = state.failed }
}

state "done"   { terminal = true }
state "failed" {
  terminal = true
  success  = false
}
