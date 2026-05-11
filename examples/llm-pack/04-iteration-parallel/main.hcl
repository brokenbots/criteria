workflow "parallel" {
  version       = "1"
  initial_state = "fanout"
  target_state  = "done"
}

adapter "shell" "default" {
  config {}
}

step "fanout" {
  target       = adapter.shell.default
  parallel     = ["echo a", "echo b", "echo c", "echo d"]
  parallel_max = 4
  on_failure   = "continue"

  input {
    command = each.value
  }

  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "report" }
}

step "report" {
  target = adapter.shell.default
  input {
    command = "echo some-failed"
  }
  outcome "success" { next = "done" }
}

state "done" {
  terminal = true
  success  = true
}
