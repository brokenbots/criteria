workflow "branching" {
  version       = "1"
  initial_state = "classify"
  target_state  = "done"
}

adapter "noop" "default" {}

step "classify" {
  target = adapter.noop.default
  outcome "success" { next = "route" }
  outcome "failure" { next = "failed" }
}

switch "route" {
  # steps.classify.label is a placeholder — replace with your adapter's actual output key
  condition {
    match = steps.classify.label == "urgent"
    next  = "handle_urgent"
  }
  condition {
    match = steps.classify.label == "normal"
    next  = "handle_normal"
  }
  default { next = "handle_other" }
}

step "handle_urgent" {
  target = adapter.noop.default
  outcome "success" { next = "done" }
}

step "handle_normal" {
  target = adapter.noop.default
  outcome "success" { next = "done" }
}

step "handle_other" {
  target = adapter.noop.default
  outcome "success" { next = "done" }
}

state "done" {
  terminal = true
  success  = true
}

state "failed" {
  terminal = true
  success  = false
}
