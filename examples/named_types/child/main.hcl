# Callee subworkflow: its own type namespace (parent type blocks are not
# visible here), with named types feeding its variable and output consumers.

workflow {
  name          = "named_types_child"
  version       = "0.1"
  initial_state = "execute"
  target_state  = "done"
}

type "count" {
  schema = number
}

adapter "noop" "default" {}

variable "count" {
  type = type.count
}

output "doubled" {
  type  = type.count
  value = var.count * 2
}

step "execute" {
  target = adapter.noop.default
  outcome "success" {
    next = state.done
  }
  outcome "failure" {
    next = state.failed
  }
}

state "done" {
  terminal = true
  success  = true
}

state "failed" {
  terminal = true
  success  = false
}
