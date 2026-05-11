# Pattern: Sequential iteration

## When to use

Use when you need to apply the same step logic to each element in a list, one
at a time. The aggregate `all_succeeded` / `any_failed` outcomes let you route
based on the collection result.

## Minimal example

```hcl
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
```

## Key idioms

- **`for_each = [...]`** — iterates the step body once per element; accepts a list or map literal or expression.
- **`each.value`** — bound to the current element value inside the step body; use `each.key` for the index or map key.
- **`each._prev`** — the output map of the previous iteration (`null` on the first); enables fold-style pipelines.
- **Aggregate outcomes** — `all_succeeded` fires when every iteration succeeds; `any_failed` fires when at least one fails.

## Common pitfalls

- **Using `parallel` outcomes** — `for_each` without `parallel` is sequential; do not expect concurrent execution.
- **Missing aggregate outcomes** — both `all_succeeded` and `any_failed` must be declared (or use `default_outcome`).

## See also

- [LANGUAGE-SPEC.md § Iteration semantics](../LANGUAGE-SPEC.md#iteration-semantics)
- [04-iteration-parallel.md](04-iteration-parallel.md) for concurrent iteration.
