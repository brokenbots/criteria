// overlapping_subgraphs.hcl — test 5: two for_each nodes that both reference
// "cleanup" in their iteration bodies. Should fail to compile.
workflow "t" {
  version       = "0.1"
  initial_state = "loop_a"
  target_state  = "done"

  for_each "loop_a" {
    items = ["a"]
    do    = "execute_a"
    outcome "all_succeeded" { transition_to = "loop_b" }
    outcome "any_failed"    { transition_to = "done" }
  }

  for_each "loop_b" {
    items = ["b"]
    do    = "execute_b"
    outcome "all_succeeded" { transition_to = "done" }
    outcome "any_failed"    { transition_to = "done" }
  }

  step "execute_a" {
    adapter = "noop"
    outcome "success" { transition_to = "cleanup" }
  }

  step "execute_b" {
    adapter = "noop"
    outcome "success" { transition_to = "cleanup" }
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
