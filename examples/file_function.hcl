# mode: standalone
# Example: demonstrates file(), fileexists(), and trimfrontmatter() expression functions.
#
# The step reads a Markdown file with YAML frontmatter, strips the frontmatter
# with trimfrontmatter(), and passes the body to a shell adapter as the command.
# The shell command in file_function_prompt.md echos a greeting string.
workflow "file_function_demo" {
  version       = "0.1"
  initial_state = "greet"
  target_state  = "done"

  output "result" {
    type        = "string"
    description = "The result message produced by the workflow"
    value       = "Function evaluation complete"
  }

  state "done" {
    terminal = true
    success  = true
  }

  step "greet" {
    adapter = "shell"
    input {
      command = trimfrontmatter(file("./file_function_prompt.md"))
    }
    outcome "success" { transition_to = "done" }
    outcome "failure" { transition_to = "done" }
  }
}
