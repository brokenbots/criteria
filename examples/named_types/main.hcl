# mode: standalone
# Example: named type blocks feeding every type consumer (KB-48, wave 5/5).
#
# This workflow exercises every place a type constraint is accepted today,
# all served from shared named `type` blocks:
#
#   - data block:      data "internal" "budget" { type = type.num … }
#   - output block:    output "spent"          { type = type.num … }
#   - variable block:  variable "limit"        { type = type.num … }
#   - step outcome:    outcome "success"       { schema = type.audit }
#   - subworkflow callee variable + output, and the parent-side binding
#     subworkflow "scale" { input = { count = var.limit } }
#
# Named and inline forms are indistinguishable at compile and run time: each
# resolves to one cty.Type (+Defaults) per position. This directory validates
# identically to its inline-type twin (see
# workflow/compile_types_consumer_test.go).
#
# Type blocks own their own workflow-wide namespace — distinct from
# step/state/data/variable names — and never reference other types
# (slice-1 restriction).
#
# Run with: make build && CRITERIA_WORKFLOW_ALLOWED_PATHS="$(pwd)" bin/criteria apply examples/named_types

workflow {
  name          = "named_types"
  version       = "0.1"
  initial_state = "prepare"
  target_state  = "done"
}

# Shared numeric vocabulary: referenced by the data block, the variable, the
# output projection, and (inside the callee, its own namespace) the child
# variable + output.
type "num" {
  schema = number
}

# Shared object vocabulary for the outcome contract: defaults are applied
# before validation, so the noop adapter's minimal payload passes.
type "audit" {
  schema = object({
    summary = optional(string, "(no summary)")
    files   = optional(number, 0)
  })
}

adapter "noop" "default" {}

data "internal" "budget" {
  type  = type.num
  value = 0
}

variable "limit" {
  type    = type.num
  default = 5
}

output "spent" {
  type  = type.num
  value = data.internal.budget.value
}

output "limit_snapshot" {
  type  = type.num
  value = var.limit
}

subworkflow "scale" {
  source = "./child"
  # The binding is type-checked against the callee's named-typed variable.
  input = {
    count = var.limit
  }
}

step "prepare" {
  target = adapter.noop.default
  outcome "success" {
    next = step.invoke
    write {
      target = data.internal.budget.value
      value  = var.limit
    }
  }
  outcome "failure" {
    next = state.failed
  }
}

step "invoke" {
  target = subworkflow.scale
  outcome "success" {
    next = step.finish
  }
  outcome "failure" {
    next = state.failed
  }
}

step "finish" {
  target = adapter.noop.default
  input {
    left = data.internal.budget.value
    scaled = steps.invoke.doubled
  }
  outcome "success" {
    schema = type.audit
    next   = state.done
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
