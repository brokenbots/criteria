workflow "phase3_marquee" {
  version       = "0.1"
  initial_state = "process_items"
  target_state  = "done"
}

variable "input_count" {
  type    = "number"
  default = 3
}

local "limit" {
  value = var.input_count * 2
}

environment "shell" "ci" {
  variables = { CI = "true" }
}

adapter "shell" "default" {
  config { }
}

# Step with parallel modifier to process items concurrently
step "process_items" {
  target       = adapter.shell.default
  parallel     = ["item_0", "item_1", "item_2"]
  input {
    command = "echo Processing ${each.value}"
  }
  
  outcome "all_succeeded" { next = "report" }
  outcome "any_failed" { next = "report" }
}

# Report step
step "report" {
  target = adapter.shell.default
  input {
    command = "echo Processing complete"
  }
  
  outcome "success" { next = "done" }
  outcome "failure" { next = "done" }
}

state "done" {
  terminal = true
  success  = true
}

# Top-level output block (Phase 3 W09 feature)
output "processed_count" {
  type  = "number"
  value = var.input_count
}
