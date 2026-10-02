# mode: standalone
# Example: per-outcome payload contracts (KB-45, outcome-contract wave 2/5).
#
# This workflow demonstrates every authoring surface the engine exposes for
# outcome contracts:
#
#   - a top-level `type` block declaring a named schema, referenced from an
#     outcome with `schema = type.audit` (workflow-level namespace).
#   - an inline typeexpr constraint on another outcome (`schema = object({…})`).
#     Named and inline forms resolve to the same cty type + defaults on the
#     wire — they are indistinguishable to the adapter and the evaluator.
#   - schema `defaults` applied before validation: a payload omitting an
#     `optional(string, …)` field passes and binds the declared default.
#   - `require_comment = true`, which makes an empty adapter comment a
#     host-rejection (repairable through the standard attempt loop).
#   - `fallback = true` (at most one per step): the outcome that finalizes a
#     step whose adapter returned no result at all — distinct from a plain
#     `default` mapping, which fires when the adapter returns an unmapped
#     name or the repair loop is exhausted.
#
# The step uses the in-tree `noop` fixture, so the happy path is runnable
# standalone: the success outcome carries no required fields, so the noop's
# empty payload validates. `make validate` compiles this workflow against the
# engine's own noop adapter handshake.
#
# Compile-time discipline: an outcome schema must be a subset of the
# adapter's handshake output_schema; a schema the adapter can never satisfy
# is a compile error, not a runtime surprise.
#
# Run with: make build && CRITERIA_WORKFLOW_ALLOWED_PATHS="$(pwd)" bin/criteria apply examples/outcome_contracts/outcome_contracts.hcl

workflow {
  name          = "outcome_contracts"
  version       = "0.1"
  initial_state = "audit_step"
  target_state  = "done"
}

# Named schema namespace: referenced by outcomes as type.<name>. Names are
# unique workflow-wide, and type-to-type references inside a type block
# (schema = object({ x = type.other })) are a compile error.
type "audit" {
  schema = object({
    summary = optional(string, "(no summary)")
    files   = optional(number, 0)
  })
}

adapter "noop" "default" {
  config { }
}

step "audit_step" {
  target = adapter.noop.default

  outcome "success" {
    schema = type.audit
    next   = state.done
  }

  outcome "audited" {
    schema = object({
      reason = optional(string, "unspecified")
    })
    require_comment = true
    next            = state.audited
  }

  # Fires when the adapter returns no finalized result at all. The engine
  # synthesizes it with no payload and no comment — never validated against
  # its own schema.
  outcome "failure" {
    fallback = true
    next     = state.failed
  }

  # Fires when the adapter returns an unmapped outcome name and the repair
  # loop (policy.max_step_retries) is exhausted without a valid verdict.
  outcome "default" {
    next = state.defaulted
  }
}

state "done" { terminal = true }
state "audited" { terminal = true }
state "failed" {
  terminal = true
  success  = false
}
state "defaulted" {
  terminal = true
  success  = false
}

output "audit_summary" {
  type        = string
  description = "audit summary bound from the success payload (defaults applied)"
  value       = steps.audit_step["summary"]
}