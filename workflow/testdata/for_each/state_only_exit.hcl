// state_only_exit.hcl — test 4: execute → review → "done" state (no _continue).
// Should fail to compile: no path reaches _continue.
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
    outcome "done" { transition_to = "done" }
  }

  state "done" {
    terminal = true
    success  = true
  }
}
