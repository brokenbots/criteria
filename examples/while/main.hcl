# mode: standalone
# Example: demonstrates the `while` step modifier for condition-driven iteration.
#
# A `while = <bool expression>` modifier causes the step to be re-executed
# as long as the expression is true, re-evaluated before each iteration.
#
# Typical patterns:
#   while = shared.remaining > 0     — decrement a shared counter each iteration
#   while = while.index < 10         — bounded by iteration index
#   while = shared.queue_empty == false  — drain a work queue
#
# NOTE: This example is for compile-validation only (used by `make validate`).
# The noop adapter does not return outputs, so `shared_writes = { attempts = "new_attempts" }`
# never receives the key and `shared.attempts` is never decremented at runtime.
# If actually executed, the loop runs until `policy.max_total_steps` fires.
# A real queue-drain workflow would use an adapter that returns the updated counter
# as an output key.
#
# This workflow simulates a simple retry-until-done pattern:
#   - shared_variable "attempts" starts at 3
#   - step "work" re-runs while attempts > 0
#   - each iteration decrements attempts via shared_writes
#   - when attempts reaches 0 the condition is false and the loop exits
#   - step "report" reads the final shared state
workflow "while-demo" {
  version       = "0.1"
  initial_state = "work"
  target_state  = "done"
}

adapter "noop" "default" {}

# Runtime counter: each iteration of step "work" decrements this value.
shared_variable "attempts" {
  type  = "number"
  value = 3
}

step "work" {
  target     = adapter.noop.default
  # Iterate as long as attempts > 0.
  while      = shared.attempts > 0
  on_failure = "continue"

  input {
    # while.index is the zero-based iteration counter (0, 1, 2, ...).
    iteration = while.index
    # while.first is true only on the first iteration.
    is_first  = while.first
  }

  # Per-iteration outcome: write the decremented counter back to shared.attempts.
  outcome "success" {
    next          = "_continue"
    shared_writes = { attempts = "new_attempts" }
  }

  # Aggregate outcomes are emitted once after the final iteration.
  outcome "all_succeeded" {
    next = "report"
  }
  outcome "any_failed" {
    next = "done"
  }
}

step "report" {
  target = adapter.noop.default
  input {
    # shared.attempts should be 0 after the loop.
    remaining = shared.attempts
  }
  outcome "success" { next = "done" }
}

state "done" {
  terminal = true
  success  = true
}
