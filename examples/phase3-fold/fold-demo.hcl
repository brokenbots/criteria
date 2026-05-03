# mode: standalone
# Example: demonstrates `local` blocks and the compile-time constant-fold pass.
#
# This workflow uses:
# - variable "base_dir": a run-time-overridable base path.
# - local "prompt_file": a compile-time constant built from the variable default.
# - local "greeting": a pure-literal constant.
#
# The fold pass resolves local.greeting and local.prompt_file at compile time;
# the engine then exposes them as `local.*` in runtime expressions.
workflow "fold-demo" {
  version       = "0.1"
  initial_state = "greet"
  target_state  = "done"

  variable "name" {
    type        = "string"
    default     = "world"
    description = "Name to greet"
  }

  # Compile-time constants.
  local "greeting" {
    description = "A friendly greeting built from var.name"
    value       = "Hello, ${var.name}!"
  }

  local "banner_line" {
    value = "---[ ${local.greeting} ]---"
  }

  step "greet" {
    adapter = "shell"
    input {
      command = "echo '${local.banner_line}'"
    }
    outcome "success" { transition_to = "done" }
    outcome "failure" { transition_to = "failed" }
  }

  state "done"   { terminal = true }
  state "failed" {
    terminal = true
    success  = false
  }
}
