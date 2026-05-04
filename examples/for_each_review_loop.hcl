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
# The example also demonstrates body variable scoping: the outer `prefix`
# variable is threaded into the body via `input = { prefix = var.prefix }`,
# so body steps can reference `var.prefix` independently of the outer scope.
#
# Run:
#   criteria apply -f examples/for_each_review_loop.hcl

workflow "for_each_review_loop" {
  version       = "0.1"
  initial_state = "init"
  target_state  = "done"

  adapter "noop" "default" {
    config { }
  }

  variable "prefix" {
    type    = "string"
    default = "item"
  }

  policy {
    max_total_steps = 50
  }

  output "status" {
    type        = "string"
    description = "Final status of the iteration processing"
    value       = "Processing complete"
  }

  output "processed_items" {
    description = "List of items that were processed"
    value       = ["alpha", "beta", "gamma"]
  }

  step "init" {
    adapter = adapter.noop.default
    input { result = "success" }
    outcome "success" { transition_to = "process_items" }
  }

  # type="workflow" step iterates its inline body once per item.
  # input = { ... } binds outer variables into the body's own var.* scope.
  step "process_items" {
    type     = "workflow"
    for_each = ["alpha", "beta", "gamma"]
    input    = { prefix = var.prefix }

    workflow {
      # Body declares its own variable; value is injected by the parent step
      # via the input = { ... } attribute above.
      variable "prefix" {
        type = "string"
      }

      adapter "noop" "default" {
        config { }
      }

      # Step 1 of the iteration body: execute work on each item.
      step "execute" {
        adapter = adapter.noop.default
        input {
          label  = "${var.prefix}:execute:${each.value}"
          result = "success"
        }
        outcome "success" { transition_to = "review" }
        outcome "failure" { transition_to = "cleanup" }
      }

      # Step 2 of the iteration body: review the result.
      step "review" {
        adapter = adapter.noop.default
        input {
          label  = "${var.prefix}:review:${each.value}"
          result = "success"
        }
        outcome "success" { transition_to = "cleanup" }
        outcome "failure" { transition_to = "_continue" }
      }

      # Step 3 of the iteration body: always clean up.
      step "cleanup" {
        adapter = adapter.noop.default
        input {
          label  = "${var.prefix}:cleanup:${each.value} (index ${each._idx})"
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
