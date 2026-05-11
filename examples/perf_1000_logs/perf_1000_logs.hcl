# Performance baseline workflow: runs 1000 shell echo commands to benchmark
# step throughput and measure engine overhead per event.
#
# mode: standalone
#
# How to run:
#   criteria apply examples/perf_1000_logs/
#
# What to expect:
#   The workflow runs a single shell step that emits 1000 lines of output via
#   a bash loop. It is useful for benchmarking step dispatch latency and engine
#   event throughput. Total wall time should be well under 5 seconds on a
#   modern machine; slower runs can indicate adapter or engine regressions.
#   Run `criteria apply --output json examples/perf_1000_logs/ | wc -l` to
#   count emitted events.
workflow "perf_1000_logs" {
  version       = "0.1"
  initial_state = "generate_logs"
  target_state  = "done"
}

adapter "shell" "default" {
  config { }
}

step "generate_logs" {
  target = adapter.shell.default
  input {
    command = "for i in {1..1000}; do echo \"Log line $i: This is a test log entry to measure throughput and latency.\"; done"
  }

  outcome "success" { next = "done" }
  outcome "failure" { next = "failed" }
}

state "done"   { terminal = true }
state "failed" {
  terminal = true
  success  = false
}
