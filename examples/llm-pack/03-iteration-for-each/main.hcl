workflow "for_each_iteration" {
  version       = "0.1"
  initial_state = "process"
  target_state  = "done"
}

adapter "shell" "default" {
  config { }
}

step "process" {
  target   = adapter.shell.default
  for_each = ["a", "b", "c"]
  input {
    command = "echo 'item ${each.value} (index ${each._idx})'"
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
