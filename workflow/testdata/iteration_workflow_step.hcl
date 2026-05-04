// iteration_workflow_step.hcl — exercises the type="workflow" step with
// an inline body (W10). The body has a multi-step sequence that terminates
// via _continue to signal iteration advance.
workflow "iteration_workflow_step" {
  version       = "0.1"
  initial_state = "run_items"
  target_state  = "done"

  policy {
    max_total_steps = 50
  }

  adapter "noop" "default" {}

  step "run_items" {
    type     = "workflow"
    for_each = ["a", "b", "c"]

    workflow {
      adapter "noop" "default" {}

      step "prepare" {
        adapter = "noop.default"
        input   { result = "success" }
        outcome "success" { transition_to = "verify" }
        outcome "failure" { transition_to = "_continue" }
      }

      step "verify" {
        adapter = "noop.default"
        input   { result = "success" }
        outcome "success" { transition_to = "_continue" }
        outcome "failure" { transition_to = "_continue" }
      }
    }

    outcome "all_succeeded" { transition_to = "done" }
    outcome "any_failed"    { transition_to = "done" }
  }

  state "done" {
    terminal = true
    success  = true
  }
}
