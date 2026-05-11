workflow "parallel_iteration" {
  version       = "0.1"
  initial_state = "fetch_all"
  target_state  = "done"
}

adapter "shell" "default" {
  config { }
}

step "fetch_all" {
  target       = adapter.shell.default
  parallel     = ["auth", "catalog", "billing", "payments"]
  parallel_max = 4
  on_failure   = "continue"
  input {
    command = "echo 'fetching ${each.value}'"
  }
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "done" }
}

state "done" { terminal = true }
