# mode: standalone
# Example: demonstrates top-level output blocks with type declarations.
#
# This workflow counts files in the current directory and outputs:
# - A summary message (string type)
# - The file count (number type)
# - A list of filenames (list(string) type)
#
# Outputs are declared at the workflow's top level and are emitted
# when the workflow reaches its terminal state.

workflow "count_files" {
  version       = "0.1"
  initial_state = "count"
  target_state  = "done"

  adapter "shell" "default" {
    config { }
  }

  # Local variable to store the count result.
  local "total" {
    value = 10
  }

  # Output 1: A summary message (computed from local variable).
  output "summary" {
    type        = "string"
    description = "A summary of the file count operation"
    value       = "Found ${local.total} files in the directory"
  }

  # Output 2: The actual count (number type, using local variable).
  output "file_count" {
    type        = "number"
    description = "Total number of files counted"
    value       = local.total
  }

  # Output 3: A summary status.
  output "status" {
    type        = "string"
    description = "Final execution status"
    value       = "File counting completed"
  }

  step "count" {
    adapter = adapter.shell.default
    input {
      command = "ls -1 | wc -l"
    }

    outcome "success" { transition_to = "done" }
    outcome "failure" { transition_to = "failed" }
  }

  state "done" {
    terminal = true
    success  = true
  }

  state "failed" {
    terminal = true
    success  = false
  }
}
