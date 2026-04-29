// cycle_no_exit.hcl — test 7: execute → review → execute (cycle), no _continue.
// Should fail to compile: no path out of the cycle.
workflow "t" {
  version       = "0.1"
  initial_state = "loop"
  target_state  = "done"

  for_each "loop" {
    items = ["a"]
    do    = "execute"
    outcome "all_succeeded" { transition_to = "done" }
    outcome "any_failed"    { transition_to = "done" }
  }

  step "execute" {
    adapter = "noop"
    outcome "success" { transition_to = "review" }
  }

  step "review" {
    adapter = "noop"
    outcome "request_changes" { transition_to = "execute" }
  }

  state "done" {
    terminal = true
    success  = true
  }
}
