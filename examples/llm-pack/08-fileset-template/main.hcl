workflow "file_driven_prompts" {
  version       = "0.1"
  initial_state = "process"
  target_state  = "done"
}

adapter "shell" "default" {
  config { }
}

step "process" {
  target   = adapter.shell.default
  for_each = ["prompts/hello.md"]
  input {
    command = "cat '${each.value}'"
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
