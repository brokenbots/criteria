# mode: standalone
# Example: demonstrates `local` blocks and the compile-time constant-fold pass.
#
# This workflow uses:
# - variable "name": a run-time-overridable name (default: "world").
# - local "greeting": a compile-time constant derived from var.name.
# - local "banner_line": a compile-time constant that chains local.greeting.
# - local "prompt_path": a compile-time file path derived from var.name,
#   demonstrating file(local.*) validation at compile time.
#
# The fold pass resolves all three locals at compile time. file(local.prompt_path)
# is validated during compilation — a missing file is caught before the workflow
# ever runs.
workflow "fold-demo" {
  version       = "0.1"
  initial_state = "greet"
  target_state  = "done"

  adapter "shell" "default" {
    config { }
  }

  variable "name" {
    type        = "string"
    default     = "world"
    description = "Name to greet"
  }

  # Compile-time constants.
  local "greeting" {
    value = "Hello, ${var.name}!"
  }

  local "banner_line" {
    value = "---[ ${local.greeting} ]---"
  }

  # Compile-time file path — file(local.prompt_path) is validated at compile.
  local "prompt_path" {
    value = "${var.name}_prompt.txt"
  }

  step "greet" {
    adapter = adapter.shell.default
    input {
      # file(local.prompt_path) is folded and validated at compile time.
      # The default var.name="world" resolves to "world_prompt.txt".
      command = "printf '%s\\n%s' '${local.banner_line}' '${file(local.prompt_path)}'"
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
