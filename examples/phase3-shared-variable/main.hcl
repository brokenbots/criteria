# mode: standalone
# Example: demonstrates `shared_variable` blocks for runtime-mutable workflow state.
#
# shared_variable provides engine-managed, workflow-scoped mutable state.
# Steps can read the current value via shared.<name> in any HCL expression,
# and write a new value by mapping an adapter output key in shared_writes.
#
# This workflow simulates a pipeline that tracks a message through processing:
# - shared_variable "status" starts as "pending"
# - step "start" writes "processing" into status via shared_writes
# - step "finish" writes "complete" into status via shared_writes
# - step "report" reads shared.status in its input expression
workflow "shared-variable-demo" {
  version       = "0.1"
  initial_state = "start"
  target_state  = "done"
}

adapter "noop" "default" {}

# Runtime-mutable workflow-scoped variable, initialised to "pending".
shared_variable "status" {
  type  = "string"
  value = "pending"
}

step "start" {
  target = adapter.noop.default

  outcome "success" {
    next = "finish"
    # Write the "next_status" output from the noop adapter into shared.status.
    shared_writes = { status = "next_status" }
  }
}

step "finish" {
  target = adapter.noop.default

  outcome "success" {
    next = "report"
    shared_writes = { status = "next_status" }
  }
}

step "report" {
  target = adapter.noop.default
  input {
    # Read the current value of shared.status into the step input.
    message = "Pipeline status is: ${shared.status}"
  }

  outcome "success" { next = "done" }
}

state "done" { terminal = true }
