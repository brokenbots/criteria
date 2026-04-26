// for_each_basic.hcl — exercises the for_each node kind (W07).
// Iterates over a list of deploy targets; each iteration calls a noop step.
workflow "for_each_basic" {
  version       = "0.1"
  initial_state = "deploy_all"
  target_state  = "done"

  for_each "deploy_all" {
    items = ["svc-a", "svc-b", "svc-c"]
    do    = "deploy_one"
    outcome "all_succeeded" { transition_to = "done" }
    outcome "any_failed"    { transition_to = "failed" }
  }

  step "deploy_one" {
    adapter = "noop"
    input {
      value = each.value
    }
    outcome "success" { transition_to = "_continue" }
    outcome "failure" { transition_to = "_continue" }
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
