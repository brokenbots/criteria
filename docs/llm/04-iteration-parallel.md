# Pattern: Concurrent iteration

## When to use

Use when step iterations are independent and can run simultaneously. The
adapter must declare the `parallel_safe` capability. Use `parallel_max` to
bound concurrency and `on_failure = "continue"` to run all iterations even
when some fail.

## Minimal example

```hcl
workflow "parallel" {
  version       = "1"
  initial_state = "fanout"
  target_state  = "done"
}

adapter "shell" "default" {
  config {}
}

step "fanout" {
  target       = adapter.shell.default
  parallel     = ["echo a", "echo b", "echo c", "echo d"]
  parallel_max = 4
  on_failure   = "continue"

  input {
    command = each.value
  }

  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "report" }
}

step "report" {
  target = adapter.shell.default
  input {
    command = "echo some-failed"
  }
  outcome "success" { next = "done" }
}

state "done" {
  terminal = true
  success  = true
}
```

## Key idioms

- **`parallel = [...]`** — list of items to iterate over concurrently; equivalent to `for_each` with concurrent dispatch.
- **`parallel_max = N`** — caps simultaneous executions; omit to run all items at once.
- **`on_failure = "continue"`** — runs all iterations even when some fail; default for parallel is `"abort"`.
- **`parallel_safe` capability** — `criteria validate` enforces this at compile time; the adapter must declare it in `Info()`, or validation fails.

## Common pitfalls

- **Shared mutable state** — avoid reading and writing `shared_variable` inside a parallel step; concurrent writes are not safe without `on_failure = "continue"` ordering guarantees.
- **Missing `report` path** — always handle `any_failed`; routing to a recovery or logging step is better than routing to a failure terminal.

## See also

- [LANGUAGE-SPEC.md § Iteration semantics](../LANGUAGE-SPEC.md#iteration-semantics)
- [03-iteration-for-each.md](03-iteration-for-each.md) for sequential iteration.
