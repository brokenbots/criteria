// multi_step.hcl — test 2: execute → review → cleanup → _continue.
workflow "t" {
  version       = "0.1"
  initial_state = "loop"
  target_state  = "done"

  for_each "loop" {
    items = ["a", "b"]
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
    outcome "approved" { transition_to = "cleanup" }
  }

  step "cleanup" {
    adapter = "noop"
    outcome "done" { transition_to = "_continue" }
  }

  state "done" {
    terminal = true
    success  = true
  }
}
