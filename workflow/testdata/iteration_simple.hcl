// iteration_simple.hcl — exercises step-level for_each and count (W10).
// A step with for_each iterates over a list; a step with count iterates
// N times. Both declare all_succeeded and any_failed aggregate outcomes.
workflow "iteration_simple" {
  version       = "0.1"
  initial_state = "process"
  target_state  = "done"

  policy {
    max_total_steps = 30
  }

  adapter "noop" "default" {}

  step "process" {
    adapter = adapter.noop.default
    for_each = ["alpha", "beta", "gamma"]
    input {
      label  = "item:${each.value}"
      result = "success"
    }
    outcome "all_succeeded" { transition_to = "count_phase" }
    outcome "any_failed"    { transition_to = "done" }
  }

  step "count_phase" {
    adapter = adapter.noop.default
    count     = 3
    on_failure = "ignore"
    input {
      label  = "idx:${each._idx}"
      result = "success"
    }
    outcome "all_succeeded" { transition_to = "done" }
    outcome "any_failed"    { transition_to = "done" }
  }

  state "done" {
    terminal = true
    success  = true
  }
}
