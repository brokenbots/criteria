workflow "file-prompts" {
  version       = "1"
  initial_state = "process"
  target_state  = "done"
}

adapter "shell" "default" {
  config {}
}

step "process" {
  target   = adapter.shell.default
  for_each = ["./prompts/alpha.md", "./prompts/beta.md"]

  input {
    command = file(each.value)
  }

  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "failed" }
}

state "done" {
  terminal = true
  success  = true
}

state "failed" {
  terminal = true
  success  = false
}
