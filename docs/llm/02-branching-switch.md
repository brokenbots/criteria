# Pattern: Branching switch

## When to use

Use when a step produces a value and subsequent execution should follow one of
several paths based on that value. `switch` evaluates conditions in order and
routes to the first matching arm.

## Minimal example

```hcl
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
```

## Key idioms

- **`switch "route" { ... }`** — a named routing node; a step's outcome points `next` at it.
- **`condition { match = <bool expr> next = "..." }`** — evaluated in declaration order; first truthy match wins.
- **`steps.<name>.<key>`** — reads an output field from a previously executed step.
- **`default { next = "..." }`** — required when conditions are not exhaustive; omitting it risks a runtime routing error.

## Common pitfalls

- **Missing `default`** — if no condition matches and `default` is absent, the run fails with a routing error at runtime.
- **Order matters** — conditions are tested top-to-bottom; place more specific conditions before general ones.

## See also

- [LANGUAGE-SPEC.md § switch](../LANGUAGE-SPEC.md#switch-name--)
- [LANGUAGE-SPEC.md § Outcome model](../LANGUAGE-SPEC.md#outcome-model)
- [01-linear.md](01-linear.md) for the sequential baseline without branching.
