# Performance baseline workflow: generates 1000 StepLog events
# mode: standalone
workflow "perf_1000_logs" {
  version       = "0.1"
  initial_state = "generate_logs"
  target_state  = "done"

  adapter "shell" "default" {
    config { }
  }

  step "generate_logs" {
    adapter = "shell.default"
    input {
      command = "for i in {1..1000}; do echo \"Log line $i: This is a test log entry to measure throughput and latency.\"; done"
    }

    outcome "success" { transition_to = "done" }
    outcome "failure" { transition_to = "failed" }
  }

  state "done"   { terminal = true }
  state "failed" {
    terminal = true
    success  = false
  }
}
