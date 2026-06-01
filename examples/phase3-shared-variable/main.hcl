# mode: standalone
# Example: demonstrates `data` blocks for runtime-mutable workflow state.
#
# data provides engine-managed, workflow-scoped mutable state.
# Steps can read the current value via data.<kind>.<name>.value in any HCL expression,
# and write a new value using a write block inside an outcome.
#
# This workflow simulates a pipeline that tracks a message through processing:
# - data "internal" "status" starts as "pending"
# - step "start" writes "processing" into status via a write block
# - step "finish" writes "complete" into status via a write block
# - step "report" reads data.internal.status.value in its input expression
workflow {
  name = "shared-variable-demo"
  version       = "0.1"
  initial_state = "start"
  target_state  = "done"
}

adapter "noop" "default" {}

# Runtime-mutable workflow-scoped variable, initialised to "pending".
data "internal" "status" {
  type = string
  value = "pending"
}

step "start" {
  target = adapter.noop.default

  outcome "success" {
    next = step.finish
    # Write a literal value into data.internal.status.value.
    write {
      target = data.internal.status.value
      value  = "processing"
    }
  }
}

step "finish" {
  target = adapter.noop.default

  outcome "success" {
    next = step.report
    write {
      target = data.internal.status.value
      value  = "complete"
    }
  }
}

step "report" {
  target = adapter.noop.default
  input {
    # Read the current value of data.internal.status.value into the step input.
    message = "Pipeline status is: ${data.internal.status.value}"
  }

  outcome "success" { next = state.done }
}

state "done" { terminal = true }
