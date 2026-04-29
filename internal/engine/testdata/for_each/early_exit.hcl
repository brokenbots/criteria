// early_exit.hcl — engine test fixture for W08 early-exit test (test 11).
// review transitions to a top-level escalate step (outside subgraph).
workflow "t" {
  version       = "0.1"
  initial_state = "loop"
  target_state  = "done"

  for_each "loop" {
    items = ["a", "b", "c"]
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
    outcome "success"  { transition_to = "_continue" }
    outcome "escalate" { transition_to = "escalate" }
  }

  step "escalate" {
    adapter = "noop"
    outcome "success" { transition_to = "done" }
  }

  state "done" {
    terminal = true
    success  = true
  }
}
