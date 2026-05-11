# Pattern: Concurrent iteration (parallel)

## When to use

Use this pattern when the same operation must run concurrently against multiple independent targets. Unlike `for_each`, iterations are dispatched simultaneously up to `parallel_max`; the adapter must declare the `parallel_safe` capability.

## Minimal example

```hcl
workflow "parallel_iteration" {
  version       = "0.1"
  initial_state = "fetch_all"
  target_state  = "done"
}

adapter "shell" "default" {
  config { }
}

step "fetch_all" {
  target       = adapter.shell.default
  parallel     = ["auth", "catalog", "billing", "payments"]
  parallel_max = 4
  on_failure   = "continue"
  input {
    command = "echo 'fetching ${each.value}'"
  }
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "done" }
}

state "done" { terminal = true }
```

## Key idioms

- **`parallel = [...]`** — like `for_each` but iterations run concurrently up to `parallel_max`.
- **`parallel_max = N`** — caps the concurrency window; all items run if `N ≥ len(list)`.
- **`on_failure = "continue"`** — keeps running remaining iterations when one fails; omit to stop on first failure.
- **`parallel_safe` capability** — the target adapter must declare this; the `shell` adapter does.

## Common pitfalls

- **Adapter not `parallel_safe`** — using an adapter without the `parallel_safe` capability causes a compile error; confirm before choosing an adapter type.
- **Wrong outcome names** — `parallel` steps use `"all_succeeded"` / `"any_failed"`, not `"success"` / `"failure"`.

## See also

- [LANGUAGE-SPEC.md § parallel](../LANGUAGE-SPEC.md#parallel)
- [03-iteration-for-each.md](./03-iteration-for-each.md) — sequential alternative; no capability requirement.
