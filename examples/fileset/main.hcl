# Example: demonstrates fileset() — enumerates files matching a glob and
# processes each one via for_each.
workflow {
  name = "fileset_demo"
  version       = "1"
  initial_state = "process"
  target_state  = "done"
}

adapter "shell" "echoer" {}

step "process" {
  for_each = fileset("inputs", "*.txt")
  target   = adapter.shell.echoer
  input {
    command = "echo Processing ${each.value}"
  }
  outcome "all_succeeded" { next = state.done }
  outcome "any_failed"    { next = state.failed }
}

state "done" {
  terminal = true
  success  = true
}

state "failed" {
  terminal = true
  success  = false
}
