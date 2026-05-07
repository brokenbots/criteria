# mode: standalone
# Example: demonstrates the `parallel = [...]` step modifier (W19).
#
# This workflow fetches metadata for three services in parallel, bounded to
# two concurrent executions at a time. Each iteration runs the same step body
# independently with `each.value` bound to the current service name.
#
# Run with:
#   criteria apply examples/phase3-parallel/parallel-demo.hcl

workflow "parallel-demo" {
  version       = "0.1"
  initial_state = "fetch"
  target_state  = "done"
}

adapter "noop" "default" {}

# Fetch metadata for three services in parallel, max two at a time.
step "fetch" {
  target       = adapter.noop.default
  parallel     = ["auth", "catalog", "billing"]
  parallel_max = 2
  on_failure   = "continue"

  input {
    service = each.value
  }

  # all_succeeded: all iterations produced a success outcome.
  outcome "all_succeeded" { next = "done" }

  # any_failed: at least one iteration produced a non-success outcome.
  # on_failure = "continue" ensures all iterations always run even if one fails.
  outcome "any_failed" { next = "done" }
}

state "done" {
  terminal = true
  success  = true
}
