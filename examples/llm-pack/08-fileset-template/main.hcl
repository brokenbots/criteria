workflow {
  name = "file-prompts"
  version       = "1"
  initial_state = "process"
  target_state  = "done"
}

adapter "shell" "default" {
  config {}
}

step "process" {
  target   = adapter.shell.default
  for_each = fileset("prompts", "*.md")

  input {
    command = file(each.value)
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
