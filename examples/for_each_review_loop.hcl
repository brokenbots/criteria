# mode: standalone
#
# for_each Multi-Step Iteration
# ==============================
# Demonstrates a for_each whose iteration body spans multiple steps:
#
#   execute → review → cleanup → _continue
#
# Each item is processed end-to-end (execute, then review, then cleanup)
# before the loop advances to the next item. The `each.*` variables are
# available in all three iteration steps.
#
# Run:
#   criteria apply -f examples/for_each_review_loop.hcl

workflow "for_each_review_loop" {
  version       = "0.1"
  initial_state = "init"
  target_state  = "done"

  policy {
    max_total_steps = 50
  }

  step "init" {
    adapter = "noop"
    input { result = "success" }
    outcome "success" { transition_to = "process_items" }
  }

  for_each "process_items" {
    items = ["alpha", "beta", "gamma"]
    do    = "execute"
    outcome "all_succeeded" { transition_to = "done" }
    outcome "any_failed"    { transition_to = "failed" }
  }

  # Step 1 of the iteration body: execute work on each item.
  step "execute" {
    adapter = "noop"
    input {
      label  = "execute:${each.value}"
      result = "success"
    }
    outcome "success" { transition_to = "review" }
    outcome "failure" { transition_to = "cleanup" }
  }

  # Step 2 of the iteration body: review the result.
  step "review" {
    adapter = "noop"
    input {
      label  = "review:${each.value}"
      result = "success"
    }
    outcome "success" { transition_to = "cleanup" }
    # On review failure, skip cleanup and mark item failed immediately.
    outcome "failure" { transition_to = "_continue" }
  }

  # Step 3 of the iteration body: always clean up regardless of outcome.
  step "cleanup" {
    adapter = "noop"
    input {
      label  = "cleanup:${each.value} (index ${each.index})"
      result = "success"
    }
    outcome "success" { transition_to = "_continue" }
    outcome "failure" { transition_to = "_continue" }
  }

  state "done"   { terminal = true }
  state "failed" {
    terminal = true
    success  = false
  }
}
