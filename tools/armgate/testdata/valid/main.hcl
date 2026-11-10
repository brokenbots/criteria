# Control tree: passes the outcome-completeness compile rule — a step with an
# adapter must handle failure/error paths (or declare fallback), here via the
# explicit failure outcome.
workflow {
  name          = "armgate-valid"
  version       = "0.1"
  initial_state = "work"
  target_state  = "done"
}

adapter "noop" "default" {
  config {}
}

step "work" {
  target = adapter.noop.default
  input {}

  outcome "success" { next = state.done }
  outcome "failure" { next = state.failed }
}

state "done" {
  terminal = true
}
state "failed" {
  terminal = true
  success  = false
}
