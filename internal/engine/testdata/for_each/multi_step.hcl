// multi_step.hcl — engine test fixtures for W08 multi-step for_each iteration.
// execute → review → cleanup → _continue, 3 items.
workflow "t" {
  version       = "0.1"
  initial_state = "loop"
  target_state  = "done"

  for_each "loop" {
    items = ["a", "b", "c"]
    do    = "execute"
    outcome "all_succeeded" { transition_to = "done" }
    outcome "any_failed"    { transition_to = "failed" }
  }

  step "execute" {
    adapter = "noop"
    input {
      value = each.value
    }
    outcome "success" { transition_to = "review" }
  }

  step "review" {
    adapter = "noop"
    input {
      value = each.value
    }
    outcome "success" { transition_to = "cleanup" }
    outcome "failure" { transition_to = "cleanup" }
  }

  step "cleanup" {
    adapter = "noop"
    input {
      value = each.value
    }
    outcome "success" { transition_to = "_continue" }
  }

  state "done" {
    terminal = true
    success  = true
  }
  state "failed" {
    terminal = true
    success  = false
  }
}
