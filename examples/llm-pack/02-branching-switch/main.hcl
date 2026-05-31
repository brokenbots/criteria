workflow {
  name = "branching"
  version       = "1"
  initial_state = "classify"
  target_state  = "done"
}

adapter "noop" "default" {}

step "classify" {
  target = adapter.noop.default
  outcome "success" { next = switch.route }
  outcome "failure" { next = state.failed }
}

switch "route" {
  # steps.classify.label is a placeholder — replace with your adapter's actual output key
  match {
    condition = steps.classify.label == "urgent"
    next = step.handle_urgent
  }
  match {
    condition = steps.classify.label == "normal"
    next = step.handle_normal
  }
  default { next = step.handle_other }
}

step "handle_urgent" {
  target = adapter.noop.default
  outcome "success" { next = state.done }
}

step "handle_normal" {
  target = adapter.noop.default
  outcome "success" { next = state.done }
}

step "handle_other" {
  target = adapter.noop.default
  outcome "success" { next = state.done }
}

state "done" {
  terminal = true
  success  = true
}

state "failed" {
  terminal = true
  success  = false
}
