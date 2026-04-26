# Demo tour - local mode variant (no approval, for testing without Castle)
#
# mode: standalone
#
# Demonstrates variables, for_each, wait (duration), and branch without requiring Castle.
workflow "demo_tour_local" {
  version       = "1"
  initial_state = "boot"
  target_state  = "done"

  policy {
    max_total_steps = 40
  }

  variable "mode" {
    type        = "string"
    default     = "local"
    description = "Execution mode identifier"
  }

  step "boot" {
    adapter = "shell"
    input {
      command = "printf '=== Demo (${var.mode} mode) ===\\n'"
    }
    timeout = "10s"
    outcome "success" { transition_to = "discover" }
    outcome "failure" { transition_to = "aborted" }
  }

  step "discover" {
    adapter = "shell"
    input {
      command = "printf 'discovering...\\n'; for t in alpha beta gamma; do printf '  -> %s\\n' \"$t\"; sleep 0.2; done"
    }
    timeout = "30s"
    outcome "success" { transition_to = "process_each" }
    outcome "failure" { transition_to = "aborted" }
  }

  for_each "process_each" {
    items = ["alpha", "beta", "gamma"]
    do    = "process"
    outcome "all_succeeded" { transition_to = "review" }
    outcome "any_failed"    { transition_to = "aborted" }
  }

  step "process" {
    adapter = "shell"
    input {
      command = "printf 'processing %s (#%s)\\n' \"${each.value}\" \"${each.index}\"; sleep 0.3"
    }
    timeout = "30s"
    outcome "success" { transition_to = "_continue" }
    outcome "failure" { transition_to = "_continue" }
  }

  step "review" {
    adapter = "shell"
    input {
      command = "printf 'review ok\\n'; echo 'ok'"
    }
    timeout = "10s"
    outcome "success" { transition_to = "wait_brief" }
    outcome "failure" { transition_to = "aborted" }
  }

  wait "wait_brief" {
    duration = "2s"
    outcome "elapsed" { transition_to = "decide" }
  }

  branch "decide" {
    arm {
      when          = steps.review.exit_code == "0"
      transition_to = "celebrate"
    }
    default {
      transition_to = "aborted"
    }
  }

  step "celebrate" {
    adapter = "shell"
    input {
      command = "printf '\\n=== DONE ===\\n'"
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
