# Regression fixture: fails the v0.5.57 outcome-completeness compile rule —
# the step declares only a success outcome, so the first fire against a
# criteria-base image stamped with that engine fails at pod-start compile.
workflow {
  name          = "armgate-incomplete"
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
}

state "done" {
  terminal = true
}
