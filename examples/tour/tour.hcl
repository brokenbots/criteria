# mode: standalone
#
# Feature tour: one workflow exercising the common constructs — variables,
# for_each iteration, parallel fan-out, a duration wait, a switch, and a
# top-level output. Uses the shell adapter.
workflow {
  name          = "tour"
  version       = "1"
  initial_state = "boot"
  target_state  = "done"
  policy {
    max_total_steps = 50
  }
}

adapter "shell" "default" {
  config {}
}

variable "label" {
  type        = string
  default     = "tour"
  description = "Label printed in step output."
}

step "boot" {
  target  = adapter.shell.default
  input   { command = "printf '=== %s ===\\n' '${var.label}'" }
  timeout = "10s"
  outcome "success" { next = step.process_each }
  outcome "failure" { next = state.aborted }
}

# for_each: run the step body once per list element, sequentially.
step "process_each" {
  target   = adapter.shell.default
  for_each = ["alpha", "beta", "gamma"]
  input    { command = "printf 'process %s (#%s)\\n' '${each.value}' '${each._idx}'" }
  timeout  = "30s"
  outcome "all_succeeded" { next = step.fan_out }
  outcome "any_failed"    { next = state.aborted }
}

# parallel: run iterations concurrently, bounded to two at a time.
step "fan_out" {
  target       = adapter.shell.default
  parallel     = ["auth", "catalog", "billing"]
  parallel_max = 2
  on_failure   = "continue"
  input        { command = "printf 'fetched %s\\n' '${each.value}'" }
  outcome "all_succeeded" { next = wait.settle }
  outcome "any_failed"    { next = state.aborted }
}

# wait: pause for a fixed duration before continuing.
wait "settle" {
  duration = "1s"
  outcome "elapsed" { next = switch.decide }
}

# switch: branch on an expression.
switch "decide" {
  match {
    condition = var.label == "tour"
    next      = step.finish
  }
  default { next = state.aborted }
}

step "finish" {
  target  = adapter.shell.default
  input   { command = "printf 'done\\n'" }
  timeout = "10s"
  outcome "success" { next = state.done }
  outcome "failure" { next = state.aborted }
}

# top-level output: evaluated when the workflow reaches a terminal state.
output "label" {
  type        = string
  description = "The label used for this run."
  value       = var.label
}

state "done" {
  terminal = true
  success  = true
}
state "aborted" {
  terminal = true
  success  = false
}
