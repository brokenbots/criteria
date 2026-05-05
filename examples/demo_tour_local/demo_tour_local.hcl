# Demo tour - local mode variant (no approval, for testing without server)
#
# mode: standalone
#
# Demonstrates variables, for_each, wait (duration), and switch without requiring a server.
workflow "demo_tour_local" {
  version       = "1"
  initial_state = "boot"
  target_state  = "done"
}

adapter "shell" "default" {
  config { }
}

policy {
  max_total_steps = 40
}

variable "mode" {
  type        = "string"
  default     = "local"
  description = "Execution mode identifier"
}

step "boot" {
  target = adapter.shell.default
  input {
    command = "printf '=== Demo (${var.mode} mode) ===\\n'"
  }
  timeout = "10s"
  outcome "success" { next = "discover" }
  outcome "failure" { next = "aborted" }
}

step "discover" {
  target = adapter.shell.default
  input {
    command = "printf 'discovering...\\n'; for t in alpha beta gamma; do printf '  -> %s\\n' \"$t\"; sleep 0.2; done"
  }
  timeout = "30s"
  outcome "success" { next = "process_each" }
  outcome "failure" { next = "aborted" }
}

step "process_each" {
  target = adapter.shell.default
  for_each = ["alpha", "beta", "gamma"]
  input {
    command = "printf 'processing %s (#%s)\\n' \"${each.value}\" \"${each._idx}\"; sleep 0.3"
  }
  timeout = "30s"
  outcome "all_succeeded" { next = "review" }
  outcome "any_failed"    { next = "aborted" }
}

step "review" {
  target = adapter.shell.default
  input {
    command = "printf 'review ok\\n'; echo 'ok'"
  }
  timeout = "10s"
  outcome "success" { next = "wait_brief" }
  outcome "failure" { next = "aborted" }
}

wait "wait_brief" {
  duration = "2s"
  outcome "elapsed" { next = "decide" }
}

switch "decide" {
  condition {
    match = steps.review.exit_code == "0"
    next  = step.celebrate
  }
  default {
    next = state.aborted
  }
}

step "celebrate" {
  target = adapter.shell.default
  input {
    command = "printf '\\n=== DONE ===\\n'"
  }
  timeout = "10s"
  outcome "success" { next = "done" }
  outcome "failure" { next = "aborted" }
}

state "done" {
  terminal = true
  success  = true
}
state "aborted" {
  terminal = true
  success  = false
}
