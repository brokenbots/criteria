workflow "for-each" {
  version       = "1"
  initial_state = "process"
  target_state  = "done"
}

adapter "noop" "default" {}

step "process" {
  target   = adapter.noop.default
  for_each = ["a", "b", "c"]

  input {
    item = each.value
  }

  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "failed" }
}

state "done" {
  terminal = true
  success  = true
}

state "failed" {
  terminal = true
  success  = false
}
