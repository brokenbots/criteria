// each_scope_leak.hcl — test 6: a step outside the for_each subgraph
// references each.value. Should fail to compile.
workflow "t" {
  version       = "0.1"
  initial_state = "loop"
  target_state  = "done"

  for_each "loop" {
    items = ["a"]
    do    = "execute"
    outcome "all_succeeded" { transition_to = "post_process" }
    outcome "any_failed"    { transition_to = "post_process" }
  }

  step "execute" {
    adapter = "noop"
    outcome "success" { transition_to = "_continue" }
  }

  step "post_process" {
    adapter = "noop"
    input {
      value = each.value
    }
    outcome "done" { transition_to = "done" }
  }

  state "done" {
    terminal = true
    success  = true
  }
}
