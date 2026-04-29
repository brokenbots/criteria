// branching.hcl — test 3: execute → review; review → execute on changes_requested,
// → cleanup on approved; cleanup → _continue.
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
    outcome "changes_requested" { transition_to = "execute" }
    outcome "approved"          { transition_to = "cleanup" }
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
