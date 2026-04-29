# mode: standalone
#
# Step-Level Iteration (for_each / workflow step)
# ================================================
# Demonstrates a step with a for_each field that runs a multi-step inline
# sub-workflow for each item using a type="workflow" step:
#
#   init → process_items (iterates: execute → review → cleanup) → done
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

  # type="workflow" step iterates its inline body once per item.
  step "process_items" {
    type     = "workflow"
    for_each = ["alpha", "beta", "gamma"]

    workflow {
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
        outcome "failure" { transition_to = "_continue" }
      }

      # Step 3 of the iteration body: always clean up.
      step "cleanup" {
        adapter = "noop"
        input {
          label  = "cleanup:${each.value} (index ${each._idx})"
          result = "success"
        }
        outcome "success" { transition_to = "_continue" }
        outcome "failure" { transition_to = "_continue" }
      }
    }

    outcome "all_succeeded" { transition_to = "done" }
    outcome "any_failed"    { transition_to = "failed" }
  }

  state "done"   { terminal = true }
  state "failed" {
    terminal = true
    success  = false
  }
}

