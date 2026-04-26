# Example: end-to-end "tour" workflow used by `make demo`.
#
# mode: orchestrator-required (uses approval; requires --castle)
#
# Showcases Phase 1.5 workflow language features: variables, for_each iteration,
# branch with step output expressions, wait with duration, and approval.
# All steps use the shell adapter for deterministic demo pacing.
#
# Total walltime ~25s on a developer laptop. Watch it live in Parapet.
#
# Local mode: `bin/overseer apply examples/demo_tour.hcl`
#   Runs up to the approval step, then errors with "approval requires --castle".
#
# Orchestrator mode: `bin/overseer apply examples/demo_tour.hcl --castle http://localhost:8080`
#   Pauses at approval; click "Approve" in Parapet to resume and reach done.
workflow "demo_tour" {
  version       = "1"
  initial_state = "boot"
  target_state  = "done"

  policy {
    max_total_steps = 40
  }

  variable "mode" {
    type        = "string"
    default     = "v1.5"
    description = "Demo mode identifier for display"
  }

  variable "phase_count" {
    type        = "number"
    default     = 3
    description = "Number of processing phases"
  }

  step "boot" {
    adapter = "shell"
    input {
      command = "printf '=== Overlord demo tour (${var.mode}) ===\\n'"
    }
    timeout = "10s"
    outcome "success" { transition_to = "discover" }
    outcome "failure" { transition_to = "aborted" }
  }

  step "discover" {
    adapter = "shell"
    input {
      command = "printf 'phase: discovering work (%s phases expected)...\\n' '${var.phase_count}'; for t in alpha beta gamma; do printf '  -> queued %s\\n' \"$t\"; sleep 0.4; done"
    }
    timeout = "30s"
    outcome "success" { transition_to = "process_each" }
    outcome "failure" { transition_to = "aborted" }
  }

  # Note: items uses a literal list due to tuple-to-list coercion limitation.
  # HCL treats ["a", "b"] as a tuple, not list(string), so declaring
  # variable "phases" { type = "list(string)"; default = [...] } would fail.
  # See docs/workflow.md §Variables for details. String/number variables work.
  for_each "process_each" {
    items = ["alpha", "beta", "gamma"]
    do    = "process"
    outcome "all_succeeded" { transition_to = "review" }
    outcome "any_failed"    { transition_to = "aborted" }
  }

  step "process" {
    adapter = "shell"
    input {
      command = "printf 'processing %s (#%s)\\n' \"${each.value}\" \"${each.index}\"; sleep 0.6"
    }
    timeout = "30s"
    outcome "success" { transition_to = "_continue" }
    outcome "failure" { transition_to = "_continue" }
  }

  step "review" {
    adapter = "shell"
    input {
      command = "printf 'review summary ready\\n'; echo 'ok'"
    }
    timeout = "10s"
    outcome "success" { transition_to = "wait_for_window" }
    outcome "failure" { transition_to = "aborted" }
  }

  wait "wait_for_window" {
    duration = "3s"
    outcome "elapsed" { transition_to = "decide_release" }
  }

  branch "decide_release" {
    arm {
      when          = steps.review.exit_code == "0"
      transition_to = "ship_approval"
    }
    default {
      transition_to = "aborted"
    }
  }

  approval "ship_approval" {
    approvers = ["dave"]
    reason    = "Approve demo deployment"
    outcome "approved" { transition_to = "celebrate" }
    outcome "rejected" { transition_to = "aborted" }
  }

  step "celebrate" {
    adapter = "shell"
    input {
      command = "printf '\\n=== ALL DONE ===\\n'"
    }
    timeout = "10s"
    outcome "success" { transition_to = "done" }
    outcome "failure" { transition_to = "aborted" }
  }

  state "done" {
    terminal = true
    success  = true
  }
  state "aborted" {
    terminal = true
    success  = false
  }
}
