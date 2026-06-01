# mode: standalone
# Example: demonstrates the `while` step modifier for condition-driven iteration.
#
# A `while = <bool expression>` modifier causes the step to be re-executed
# as long as the expression is true, re-evaluated before each iteration.
#
# Typical patterns:
#   while = data.internal.remaining.value > 0     — decrement a data counter each iteration
#   while = while.index < 10         — bounded by iteration index
#   while = data.internal.queue_empty.value == false  — drain a work queue
#
# NOTE: This example is for compile-validation only (used by `make validate`).
# The noop adapter does not return outputs, so write blocks referencing
# output.new_attempts never receive the key and data.internal.attempts.value
# is never decremented at runtime.
# If actually executed, the loop runs until `policy.max_total_steps` fires.
# A real queue-drain workflow would use an adapter that returns the updated counter
# as an output key.
#
# This workflow simulates a simple retry-until-done pattern:
#   - data "internal" "attempts" starts at 3
#   - step "work" re-runs while attempts > 0
#   - each iteration decrements attempts via write blocks
#   - when attempts reaches 0 the condition is false and the loop exits
#   - step "report" reads the final data state
workflow {
  name = "while-demo"
  version       = "0.1"
  initial_state = "work"
  target_state  = "done"
}

adapter "noop" "default" {}

# Runtime counter: each iteration of step "work" decrements this value.
data "internal" "attempts" {
  type = number
  value = 3
}

step "work" {
  target     = adapter.noop.default
  # Iterate as long as attempts > 0.
  while      = data.internal.attempts.value > 0
  on_failure = "continue"

  input {
    # while.index is the zero-based iteration counter (0, 1, 2, ...).
    iteration = while.index
    # while.first is true only on the first iteration.
    is_first  = while.first
  }

  # Per-iteration outcome: write the decremented counter back to data.internal.attempts.value.
  outcome "success" {
    next = continue
    write {
      target = data.internal.attempts.value
      value  = output.new_attempts
    }
  }

  # Aggregate outcomes are emitted once after the final iteration.
  outcome "all_succeeded" {
    next = step.report
  }
  outcome "any_failed" {
    next = state.done
  }
}

step "report" {
  target = adapter.noop.default
  input {
    # data.internal.attempts.value should be 0 after the loop.
    remaining = data.internal.attempts.value
  }
  outcome "success" { next = state.done }
}

state "done" {
  terminal = true
  success  = true
}
