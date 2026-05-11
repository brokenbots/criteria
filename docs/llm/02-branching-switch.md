# Pattern: Branching switch

## When to use

Use this pattern when the workflow must take one of several paths based on a captured value. The `switch` block matches a step output against conditions and routes to the matching arm; a `default` arm handles the unmatched case.

## Minimal example

```hcl
workflow "branching_switch" {
  version       = "0.1"
  initial_state = "classify"
  target_state  = "done"
}

adapter "shell" "default" {
  config { }
}

step "classify" {
  target = adapter.shell.default
  input {
    command = "echo 'approved'"
  }
  outcome "success" { next = "route" }
  outcome "failure" { next = "failed" }
}

switch "route" {
  condition {
    match = steps.classify.stdout == "approved"
    next  = step.approve
  }
  condition {
    match = steps.classify.stdout == "rejected"
    next  = step.reject
  }
  default {
    next = state.failed
  }
}

step "approve" {
  target = adapter.shell.default
  input { command = "echo 'approved path'" }
  outcome "success" { next = "done" }
  outcome "failure" { next = "failed" }
}

step "reject" {
  target = adapter.shell.default
  input { command = "echo 'rejected path'" }
  outcome "success" { next = "done" }
  outcome "failure" { next = "failed" }
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

- **`switch "<name>" { condition { match = ... next = ... } }`** — evaluates conditions in order; first match wins.
- **`next = step.<name>` / `next = state.<name>`** — switch targets use reference syntax, not quoted strings.
- **`default { next = ... }`** — required catch-all inside every `switch` block.
- **`steps.<name>.stdout == "<value>"`** — string equality in `match`; the shell adapter exposes `stdout`, `stderr`, `exit_code`.

## Common pitfalls

- **Quoted `next` value** — `next = "approve"` is invalid inside `switch`; use the reference form `next = step.approve`.
- **Missing `default`** — a `switch` without a `default` arm fails validation.

## See also

- [LANGUAGE-SPEC.md § switch](../LANGUAGE-SPEC.md#switch)
- [01-linear.md](./01-linear.md) — simpler no-branching baseline.
